package internal

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"time"
)

// handlePluginUpgrade 执行 octo 插件升级
//   - npx -y create-openclaw-octo install
//   - create-openclaw-octo 是统一安装入口 CLI (跟 botfather /install 命令一致),
//     内部:
//       1. 检测当前状态 (老版 npm 包 / 已迁移到 ClawHub / 未装)
//       2. 老版本自动卸载 + 装 ClawHub 版本 (legacy → octo prefix 迁移)
//       3. 已是 ClawHub 版本 → 升级到 latest
//       4. 失败完整回滚 (cli/install.ts runMigration: pre-migration backup +
//          rollback closure: 清 partial install + 恢复 cfg + 重启 legacy plugin)
//       5. 自动重启 openclaw gateway
//   - daemon 不主动上报 completed, 靠 register handler 里的 plugin 关单路径关闭
//     (register 上报 metadata.plugins 含新版本 → 服务端
//      completeUpgradeIfMatchedWithRuntime 关单)
//
// Phase B (所有 daemon 都迁完到 ClawHub 后) 可以切到 `openclaw plugins update octo`
// 跳过 npx wrapper 直接 ClawHub native, 但**当前不行** — 还有装老版 npm 包的用户,
// `openclaw plugins update` 在他们机器上找不到注册的 ClawHub 插件会失败.
func (d *Daemon) handlePluginUpgrade(ctx context.Context, up *PendingUpgrade) error {
	log.Printf("[INFO] plugin upgrade task: %s → %s (task=%s)", up.Component, up.TargetVersion, up.TaskID)

	// dispatched → installing (progress — 失败 swallow)
	_ = d.reportUpgrade(ctx, up.TaskID, "installing", "")

	// 服务端插件 timeout 10 分钟；daemon 这里留 1 分钟 buffer 给上报，用 9 分钟
	installCtx, cancel := context.WithTimeout(ctx, 9*time.Minute)
	defer cancel()

	// up.Component 是 plugin id ("octo")，但 npm 包名是 "create-openclaw-octo"，
	// 必须 hardcode npm 包名（不能用 up.Component 当 npx target）。
	// 不带版本号 → 走 @latest. install 子命令内部走 ClawHub native, 兼容老环境.
	cmd := exec.CommandContext(installCtx, "npx", "-y", "create-openclaw-octo", "install")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := fmt.Sprintf("npx install failed: %v\noutput: %s", err, truncateOutput(string(out), 2000))
		log.Printf("[ERROR] plugin upgrade failed: %s", msg)
		return d.reportUpgrade(ctx, up.TaskID, "failed", msg)
	}
	log.Printf("[INFO] plugin upgrade npx exited cleanly (task=%s)", up.TaskID)

	// npx 退出只能保证 CLI 流程跑完，不能保证 openclaw gateway 已经带新插件起来。
	// octo 的 install 脚本里 gateway restart 失败只打 warning，
	// 会导致"磁盘插件升级+服务端关单 completed 但 gateway 还在跑旧插件"。
	// 这里显式探 gateway，不通则上报 failed。
	// 给 gateway 一点启动时间（用户 install 命令内部已经重启，但可能进程刚 spawn 还没 bind）。
	time.Sleep(2 * time.Second)
	if openclawBin, lookErr := exec.LookPath("openclaw"); lookErr == nil {
		if !isOpenclawGatewayRunning(openclawBin) {
			return d.reportUpgrade(ctx, up.TaskID, "failed", "plugin installed but openclaw gateway is not running after restart")
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
				return nil
			case <-time.After(backoff):
			}
		}
		enrichCtx, enrichCancel := context.WithTimeout(context.Background(), 60*time.Second)
		_, lastErr = d.enrichDetectAndRegister(enrichCtx)
		enrichCancel()
		if lastErr == nil {
			return nil
		}
		log.Printf("[WARN] post-upgrade enrich register attempt %d/%d failed: %v", attempt+1, maxAttempts, lastErr)
	}
	log.Printf("[WARN] post-upgrade enrich register exhausted retries (last: %v), scheduling slow detect fallback", lastErr)
	d.requestSlowDetect(ctx)
	return nil
}

func truncateOutput(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}
