package internal

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"time"
)

// componentUpgradeSpec 描述一个 provider 组件的升级方式。
// 各 CLI 都自带 update 子命令（claude update / codex update / hermes update / openclaw update），
// daemon 只负责调用 + 注入非交互环境 + 探活。
type componentUpgradeSpec struct {
	// Command 返回 exec.CommandContext 的 argv。targetVersion 仅 openclaw 用（pin tag）。
	Command func(targetVersion string) []string
	// ExecTimeout 比服务端 sweeper 短，留余量给 daemon report failed。
	ExecTimeout time.Duration
	// PostHook 在 exec 成功后跑；返回错误则 report failed。
	PostHook func(ctx context.Context, d *Daemon) error
}

var componentUpgradeSpecs = map[string]componentUpgradeSpec{
	"claude": {
		Command:     func(_ string) []string { return []string{"claude", "update"} },
		ExecTimeout: 2 * time.Minute,
	},
	"codex": {
		Command:     func(_ string) []string { return []string{"codex", "update"} },
		ExecTimeout: 3 * time.Minute,
	},
	"hermes": {
		// hermes update: git pull + pip reinstall，可能几分钟
		Command:     func(_ string) []string { return []string{"hermes", "update"} },
		ExecTimeout: 14 * time.Minute,
	},
	"openclaw": {
		// pin 目标版本避免 runtime_latest_version 漂移导致升到非预期版本。
		// --yes 跳过交互确认；--timeout 600 让 openclaw 内部也有上限（不受 daemon ctx 支配的子进程）。
		Command: func(to string) []string {
			args := []string{"openclaw", "update", "--yes", "--timeout", "600"}
			if to != "" {
				args = append(args, "--tag", to)
			}
			return args
		},
		ExecTimeout: 11 * time.Minute,
		PostHook:    openclawPostUpgradeGatewayCheck,
	},
}

func openclawPostUpgradeGatewayCheck(_ context.Context, _ *Daemon) error {
	// openclaw update 默认会重启 gateway；给它 2s 启动时间再 probe。
	time.Sleep(2 * time.Second)
	bin, err := exec.LookPath("openclaw")
	if err != nil {
		return nil // 找不到就不 probe，enrich 阶段会用实际 bin 检测
	}
	if !isOpenclawGatewayRunning(bin) {
		return fmt.Errorf("openclaw gateway not running after update")
	}
	return nil
}

// handleComponentUpgrade 处理 provider 组件升级（claude/codex/hermes/openclaw）。
//
// 流程：
//  1. report installing（服务端 dispatched → installing）
//  2. exec <component> update，注入 CI=1 NO_COLOR=1 规避交互
//  3. PostHook（openclaw gateway probe）
//  4. 同步 enrichDetectAndRegister（带重试），让 register handler 走 provider 关单
//  5. 比对 pre/post 版本：如果 exec exit 0 但版本没变，主动 report failed，
//     避免服务端假 timeout
func (d *Daemon) handleComponentUpgrade(ctx context.Context, up *PendingUpgrade) error {
	spec, ok := componentUpgradeSpecs[up.Component]
	if !ok {
		return d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("unsupported component: %s", up.Component))
	}
	log.Printf("[INFO] component upgrade task: %s → %s (task=%s)", up.Component, up.TargetVersion, up.TaskID)

	// 记录升级前版本（用于事后校验）
	preVersion := d.getRuntimeVersion(up.Component)

	// progress — 失败 swallow, 后续 terminal 会覆盖
	_ = d.reportUpgrade(ctx, up.TaskID, "installing", "")

	argv := spec.Command(up.TargetVersion)
	if len(argv) == 0 {
		return d.reportUpgrade(ctx, up.TaskID, "failed", "empty update command")
	}

	execCtx, cancel := context.WithTimeout(ctx, spec.ExecTimeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, argv[0], argv[1:]...)
	cmd.Env = append(cmd.Environ(), "CI=1", "NO_COLOR=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := fmt.Sprintf("%s update failed: %v\noutput: %s", up.Component, err, truncateOutput(string(out), 2000))
		log.Printf("[ERROR] component upgrade failed: %s", msg)
		return d.reportUpgrade(ctx, up.TaskID, "failed", msg)
	}
	log.Printf("[INFO] %s update exited cleanly (task=%s)", up.Component, up.TaskID)

	if spec.PostHook != nil {
		if err := spec.PostHook(ctx, d); err != nil {
			return d.reportUpgrade(ctx, up.TaskID, "failed", err.Error())
		}
	}

	// 同步 enrich + register：新版本走 register handler 里的 provider 关单路径。
	// 失败指数退避重试（0/5/10s，总 ~15s），全部失败 requestSlowDetect 兜底。
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt*5) * time.Second
			select {
			case <-ctx.Done():
				log.Printf("[WARN] post-upgrade enrich register aborted: %v", ctx.Err())
				return nil
			case <-time.After(backoff):
			}
		}
		enrichCtx, enrichCancel := context.WithTimeout(context.Background(), 60*time.Second)
		_, lastErr = d.enrichDetectAndRegister(enrichCtx)
		enrichCancel()
		if lastErr == nil {
			break
		}
		log.Printf("[WARN] post-upgrade enrich register attempt %d/%d failed: %v", attempt+1, maxAttempts, lastErr)
	}
	if lastErr != nil {
		log.Printf("[WARN] post-upgrade enrich exhausted retries (last: %v), scheduling slow detect fallback", lastErr)
		d.requestSlowDetect(ctx)
		// register 失败时 d.lastRuntimes 还是旧版本，读出来做 pre/post
		// 会误报 failed。后续 slow detect 成功注册会让服务端 register
		// 关单；最坏情况走 sweeper timeout。这里直接返回。
		return nil
	}

	// 版本校验（仅 register 成功后执行，lastRuntimes 此时是权威新版本）：
	//   1) exit 0 但版本没变 → CLI 假成功
	//   2) 版本变了但没到 target → update 升到了 latest 之前的版本
	// 两种都主动 report failed，避免用户等到服务端 sweeper timeout。
	postVersion := d.getRuntimeVersion(up.Component)
	if postVersion == "" {
		log.Printf("[WARN] could not detect %s version post-upgrade, relying on server close-out", up.Component)
		return nil
	}
	if preVersion != "" && postVersion == preVersion {
		msg := fmt.Sprintf("%s update exit 0 but version did not change (still %s)", up.Component, postVersion)
		log.Printf("[WARN] %s", msg)
		return d.reportUpgrade(ctx, up.TaskID, "failed", msg)
	}
	if up.TargetVersion != "" && isVersionOlder(postVersion, up.TargetVersion) {
		msg := fmt.Sprintf("%s update reached version %s but target was %s", up.Component, postVersion, up.TargetVersion)
		log.Printf("[WARN] %s", msg)
		return d.reportUpgrade(ctx, up.TaskID, "failed", msg)
	}
	log.Printf("[INFO] %s post-upgrade version=%s (target=%s, pre=%s); server register will close task on match",
		up.Component, postVersion, up.TargetVersion, preVersion)
	return nil
}

// getRuntimeVersion 读 d.lastRuntimes 中指定 provider 的 Version。
func (d *Daemon) getRuntimeVersion(provider string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range d.lastRuntimes {
		if r.Provider == provider {
			return r.Version
		}
	}
	return ""
}
