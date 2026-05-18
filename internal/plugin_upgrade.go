package internal

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"time"
)

// handlePluginUpgrade 执行 openclaw-channel-dmwork 插件升级
//   - npx -y openclaw-channel-dmwork install --force
//   - CLI 自身会：下载最新 npm 版本 → 安装到 openclaw npm node_modules → 自动重启 openclaw gateway
//   - daemon 不主动上报 completed，靠 register handler 里的 plugin 关单路径关闭
//     (register 上报 metadata.plugins 含新版本 → 服务端 completeUpgradeIfMatchedWithRuntime 关单)
func (d *Daemon) handlePluginUpgrade(ctx context.Context, up *PendingUpgrade) {
	log.Printf("[INFO] plugin upgrade task: %s → %s (task=%s)", up.Component, up.TargetVersion, up.TaskID)

	// dispatched → installing
	d.reportUpgrade(ctx, up.TaskID, "installing", "")

	// 服务端插件 timeout 10 分钟；daemon 这里留 1 分钟 buffer 给上报，用 9 分钟
	installCtx, cancel := context.WithTimeout(ctx, 9*time.Minute)
	defer cancel()

	// v1 只装 latest，不传版本号；--force 避免 CLI 交互式确认
	cmd := exec.CommandContext(installCtx, "npx", "-y", up.Component, "install", "--force")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := fmt.Sprintf("npx install failed: %v\noutput: %s", err, truncateOutput(string(out), 2000))
		log.Printf("[ERROR] plugin upgrade failed: %s", msg)
		d.reportUpgrade(ctx, up.TaskID, "failed", msg)
		return
	}
	log.Printf("[INFO] plugin upgrade npx exited cleanly (task=%s)", up.TaskID)

	// npx 退出只能保证 CLI 流程跑完，不能保证 openclaw gateway 已经带新插件起来。
	// openclaw-channel-dmwork 的 install 脚本里 gateway restart 失败只打 warning，
	// 会导致"磁盘插件升级+服务端关单 completed 但 gateway 还在跑旧插件"。
	// 这里显式探 gateway，不通则上报 failed。
	// 给 gateway 一点启动时间（用户 install 命令内部已经重启，但可能进程刚 spawn 还没 bind）。
	time.Sleep(2 * time.Second)
	if openclawBin, lookErr := exec.LookPath("openclaw"); lookErr == nil {
		if !isOpenclawGatewayRunning(openclawBin) {
			d.reportUpgrade(ctx, up.TaskID, "failed", "plugin installed but openclaw gateway is not running after restart")
			return
		}
	}

	// 同步跑 enrich detect + register：必须带上新插件版本去服务端，才能走
	// completeUpgradeIfMatchedWithRuntime 关单。不同步做的话：
	//   - 用户会先看到 10 min timeout 再看到 15s 心跳周期的 completed，体验断裂
	//   - 更糟：runtimesChanged 判定可能 false（如果插件 name/version 其他字段稳定），
	//     register 根本不发，任务永远 timeout。
	// enrich 内部要跑 openclaw plugins list --json，给 60s 上限。
	//
	// 失败做有限次指数退避重试（0/5/10s，总共 ~15s），全部失败再调度一次 slow detect
	// 让心跳周期兜底，避免"插件实际已升级但任务走 10min timeout"。
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt*5) * time.Second
			select {
			case <-ctx.Done():
				log.Printf("[WARN] post-upgrade enrich register aborted: %v", ctx.Err())
				return
			case <-time.After(backoff):
			}
		}
		enrichCtx, enrichCancel := context.WithTimeout(context.Background(), 60*time.Second)
		_, lastErr = d.enrichDetectAndRegister(enrichCtx)
		enrichCancel()
		if lastErr == nil {
			return
		}
		log.Printf("[WARN] post-upgrade enrich register attempt %d/%d failed: %v", attempt+1, maxAttempts, lastErr)
	}
	log.Printf("[WARN] post-upgrade enrich register exhausted retries (last: %v), scheduling slow detect fallback", lastErr)
	d.requestSlowDetect(ctx)
}

func truncateOutput(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}
