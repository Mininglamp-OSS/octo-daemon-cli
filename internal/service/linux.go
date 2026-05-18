//go:build linux

package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

type systemdUserService struct{}

const systemdUnit = "octo-daemon.service"

func (*systemdUserService) Install(cfg InstallConfig, force bool) error {
	unitPath, err := SystemdUnitPath()
	if err != nil {
		return err
	}

	// force: best-effort pre-uninstall (ignore errors — state may be partial).
	if force {
		_ = (&systemdUserService{}).Uninstall()
	} else if _, statErr := os.Stat(unitPath); statErr == nil {
		return fmt.Errorf("service already installed at %s (use --force to reinstall)", unitPath)
	}

	// Materialize directories.
	for _, d := range []string{ServiceEnvDir(), LogDir(), filepath.Dir(unitPath)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	// Write wrapper.sh (rw-r--r-- + exec bit).
	if err := os.WriteFile(WrapperScriptPath(), []byte(WrapperScriptContent()), 0o755); err != nil {
		return fmt.Errorf("write wrapper: %w", err)
	}

	// Build env file (always inject UNDER_SERVICE flag).
	env := map[string]string{}
	for k, v := range cfg.Env {
		env[k] = v
	}
	env[OctoDaemonUnderServiceEnv] = "1"
	envBody, err := RenderEnvFile(env)
	if err != nil {
		return fmt.Errorf("render env: %w", err)
	}
	if err := os.WriteFile(EnvFilePath(), []byte(envBody), 0o600); err != nil {
		return fmt.Errorf("write env: %w", err)
	}

	// Render + write systemd unit.
	unitBody, err := RenderSystemdUnit(SystemdUnitRenderArgs{
		WrapperScript: WrapperScriptPath(),
		EnvFile:       EnvFilePath(),
		ExecPath:      cfg.ExecPath,
		ConfigPath:    cfg.ConfigPath,
	})
	if err != nil {
		return fmt.Errorf("render unit: %w", err)
	}
	if err := os.WriteFile(unitPath, []byte(unitBody), 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}

	// Reload + enable --now.
	if out, err := runSystemctlUser("daemon-reload"); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload failed: %w\n%s", err, out)
	}
	if out, err := runSystemctlUser("enable", "--now", systemdUnit); err != nil {
		return fmt.Errorf("systemctl --user enable --now failed: %w\n%s", err, out)
	}

	// Lingering check — without this, the service dies on logout. We warn
	// rather than fail because:
	//   - on desktop sessions with active login this works anyway
	//   - enabling lingering may require `loginctl enable-linger $USER` which
	//     some setups restrict; user should decide
	if !isLingeringEnabled() {
		fmt.Fprintln(os.Stderr, "[WARN] user lingering is NOT enabled. The service will stop when you log out.")
		fmt.Fprintln(os.Stderr, "       Run: loginctl enable-linger $USER")
	}
	return nil
}

func (*systemdUserService) Uninstall() error {
	unitPath, err := SystemdUnitPath()
	if err != nil {
		return err
	}

	// Order matters: stop → remove files → daemon-reload.
	// Reloading BEFORE removing the unit leaves systemd's in-memory cache
	// still referencing the deleted file on disk; reloading AFTER gives a
	// clean uninstall state. Errors on the stop step are ignored — unit
	// may already be absent.
	_, _ = runSystemctlUser("disable", "--now", systemdUnit)

	for _, p := range []string{unitPath, WrapperScriptPath(), EnvFilePath()} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	// Prune service-env dir if empty (best-effort).
	_ = os.Remove(ServiceEnvDir())

	_, _ = runSystemctlUser("daemon-reload")
	return nil
}

func (*systemdUserService) Status() (StatusInfo, error) {
	unitPath, err := SystemdUnitPath()
	if err != nil {
		return StatusInfo{}, err
	}
	info := StatusInfo{}
	if _, err := os.Stat(unitPath); err == nil {
		info.Installed = true
	}

	// systemctl is-active: prints "active" / "inactive" / "failed" etc.
	// Exit code is 0 only when active — but we want to treat both states
	// without raising errors, so ignore exit code and read stdout.
	out, _ := runSystemctlUser("is-active", systemdUnit)
	if strings.TrimSpace(out) == "active" {
		info.Running = true
	}

	// MainPID property — "0" when not running.
	pidOut, err := runSystemctlUser("show", "--property=MainPID", "--value", systemdUnit)
	if err == nil {
		if pid, perr := strconv.Atoi(strings.TrimSpace(pidOut)); perr == nil && pid > 0 {
			info.PID = pid
			// is-active said inactive but MainPID > 0 is unusual; trust is-active.
			if !info.Running {
				info.PID = 0
			}
		}
	}
	return info, nil
}

func (*systemdUserService) Restart() error {
	if out, err := runSystemctlUser("restart", systemdUnit); err != nil {
		return fmt.Errorf("systemctl --user restart failed: %w\n%s", err, out)
	}
	return nil
}

// LogPath returns "" so the CLI knows to call journalctl for logs.
func (*systemdUserService) LogPath() string { return "" }

// runSystemctlUser always prepends --user; keep callers uniform.
func runSystemctlUser(args ...string) (string, error) {
	all := append([]string{"--user"}, args...)
	cmd := exec.Command("systemctl", all...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// isLingeringEnabled returns true when `loginctl show-user $USER --property=Linger --value`
// says "yes". Absence (e.g. loginctl not installed) returns false — we'll warn.
func isLingeringEnabled() bool {
	u, err := user.Current()
	if err != nil {
		return false
	}
	cmd := exec.Command("loginctl", "show-user", u.Username, "--property=Linger", "--value")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "yes"
}
