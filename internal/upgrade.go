package internal

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// DaemonNpmPackage is the npm global package that ships the daemon binary.
const DaemonNpmPackage = "@mininglamp-oss/octo-daemon"

// ErrNpmNotFound signals npm is unavailable (e.g. a k8s container where the
// binary is baked into the image), so callers can treat npm-based upgrade as
// unsupported rather than a hard failure.
var ErrNpmNotFound = errors.New("npm not found")

// InstallDaemonNpm replaces the on-disk daemon binary via
// `npm install -g <DaemonNpmPackage>@<version>`. version is "latest" or a pinned
// version like "0.0.5". Shared by the `octo-daemon upgrade` CLI and the
// fleet-dispatched daemon self-upgrade so both go through npm (rather than the
// in-process binary swap that fought npm and was removed).
func InstallDaemonNpm(ctx context.Context, version string) error {
	npm, err := exec.LookPath("npm")
	if err != nil {
		return ErrNpmNotFound
	}
	cmd := exec.CommandContext(ctx, npm, "install", "-g", DaemonNpmPackage+"@"+version)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// installedDaemonVersion returns the npm-installed version of the daemon package
// from DetectDeviceComponents (the npm `npm ls -g` source of truth), or "" if it
// isn't installed via npm. Used as the upgrade loop guard: comparing the on-disk
// installed version (not the running process's ldflags CLIVersion) against the
// task's target keeps the daemon's judgement aligned with what npm actually has.
func installedDaemonVersion() string {
	for _, c := range DetectDeviceComponents() {
		if c.ComponentKey == DaemonNpmPackage {
			return c.Version
		}
	}
	return ""
}

func (d *Daemon) handleUpgrade(ctx context.Context, up *PendingUpgrade) error {
	switch up.Component {
	case "octo", ccOctoPluginName:
		// Both are octo-adapter "plugins" (openclaw's bundled octo / claude's
		// cc-channel-octo gateway). handlePluginUpgrade branches on the
		// component to pick the install command + liveness probe.
		return d.handlePluginUpgrade(ctx, up)
	case "", "octo-daemon":
		return d.handleDaemonUpgrade(ctx, up)
	case "claude", "openclaw":
		return d.handleComponentUpgrade(ctx, up)
	default:
		log.Printf("[ERROR] unsupported upgrade component: %s", up.Component)
		return d.reportUpgrade(ctx, up.TaskID, "failed", "unsupported component: "+up.Component)
	}
}

// handleDaemonUpgrade applies a fleet-dispatched daemon upgrade by re-installing
// the npm package at the requested version, then stopping the process gracefully
// (exit 0) so the supervisor (pm2 / systemd / supervisord / k8s) restarts it on
// the new binary — the same npm + stop flow as the `octo-daemon upgrade` CLI. It
// uses npm rather than swapping the binary in-process (the old approach that
// fought npm's package management and was removed).
//
// Success is never reported explicitly (consistent with runtime/plugin upgrades):
// the respawned process re-registers with the new version and the server closes
// the task on match. We only report "failed" when the upgrade genuinely didn't
// happen (npm missing, npm install error, or the installed version not reaching
// the target).
//
// Two guards keep it safe:
//   - already at the target version → skip the reinstall/restart entirely, so a
//     task the server keeps re-dispatching can't crash-loop the daemon.
//   - npm unavailable → report "failed" (k8s/image deployments manage the binary
//     via the orchestration layer, not npm).
func (d *Daemon) handleDaemonUpgrade(ctx context.Context, up *PendingUpgrade) error {
	installed := installedDaemonVersion()
	if up.TargetVersion != "" && installed == up.TargetVersion {
		log.Printf("[INFO] daemon already at target version %s (task=%s) — skipping", up.TargetVersion, up.TaskID)
		return nil
	}

	version := up.TargetVersion
	if version == "" {
		version = "latest"
	}

	_ = d.reportUpgrade(ctx, up.TaskID, "installing", "")
	log.Printf("[INFO] upgrading daemon: %s → %s (task=%s)", installed, version, up.TaskID)

	if err := InstallDaemonNpm(ctx, version); err != nil {
		if errors.Is(err, ErrNpmNotFound) {
			const msg = "npm not found — daemon binary is managed by the orchestration layer (k8s/image), not upgradable in-process"
			log.Printf("[INFO] daemon upgrade task %s: %s", up.TaskID, msg)
			return d.reportUpgrade(ctx, up.TaskID, "failed", msg)
		}
		return d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("npm install failed: %v", err))
	}

	// Verify the install actually produced the target on disk before restarting;
	// npm exiting 0 without the expected version would otherwise burn a pointless
	// stop/respawn cycle and leave the task stuck.
	if post := installedDaemonVersion(); up.TargetVersion != "" && post != up.TargetVersion {
		msg := fmt.Sprintf("npm install exited 0 but installed version is %q, want %q", post, up.TargetVersion)
		log.Printf("[WARN] daemon upgrade task %s: %s", up.TaskID, msg)
		return d.reportUpgrade(ctx, up.TaskID, "failed", msg)
	}

	// New binary on disk. Stop gracefully (exit 0); the supervisor restarts us on
	// it, and the respawned process re-registers the new version to close the task.
	log.Printf("[INFO] daemon upgraded to %s on disk — stopping; the process supervisor will restart it on the new binary (task=%s)", version, up.TaskID)
	if err := stopForRestart(); err != nil {
		// Signalling ourselves should never fail; if it does, the operator must
		// restart manually to pick up the new binary.
		log.Printf("[WARN] daemon upgrade installed but self-stop failed: %v — restart the daemon manually to apply", err)
	}
	return nil
}

// stopForRestart signals the current process to shut down gracefully (exit 0),
// the same path as `octo-daemon stop` and an external SIGTERM. The supervisor
// then restarts it. Kept supervisor-agnostic: we exit cleanly rather than using
// a special respawn exit code.
func stopForRestart() error {
	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGTERM)
}

// reportUpgrade 把 status 发回 fleet. 返回 ReportUpgrade 的实际 err.
//
// caller 决定 swallow vs propagate (Jerry-Xin Critical fix):
//   - progress status (downloading/installing/restarting) — 失败不致命, 用
//     `_ = d.reportUpgrade(...)` swallow. 后续 terminal report 会覆盖.
//   - terminal status (failed) — 失败致命, 必须 `return d.reportUpgrade(...)`
//     往 handler 上抛 → adapter (HandleUpgrade) 透传 → dispatcher 不 markDone
//     → SSE replay / heartbeat 兜底重试. 不传则 fleet 永远不知道 task 终结,
//     daemon 端 dedup 已 markDone, SSE replay/heartbeat 都不会再触发,
//     task 永远卡在 dispatched/installing/... 直到 sweeper timeout 误报.
func (d *Daemon) reportUpgrade(ctx context.Context, taskID, status, errMsg string) error {
	reportCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.client.ReportUpgrade(reportCtx, taskID, status, errMsg); err != nil {
		log.Printf("[WARN] upgrade report failed (status=%s): %v", status, err)
		return err
	}
	log.Printf("[INFO] upgrade status: %s", status)
	return nil
}
