package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// handlePluginUpgrade 执行 octo 插件升级
//   - npx -y create-openclaw-octo install
//   - create-openclaw-octo 是统一安装入口 CLI (跟 botfather /install 命令一致),
//     内部:
//     1. 检测当前状态 (老版 npm 包 / 已迁移到 ClawHub / 未装)
//     2. 老版本自动卸载 + 装 ClawHub 版本 (legacy → octo prefix 迁移)
//     3. 已是 ClawHub 版本 → 升级到 latest
//     4. 失败完整回滚 (cli/install.ts runMigration: pre-migration backup +
//     rollback closure: 清 partial install + 恢复 cfg + 重启 legacy plugin)
//     5. 自动重启 openclaw gateway
//   - daemon 不主动上报 completed, 靠 register handler 里的 plugin 关单路径关闭
//     (register 上报 metadata.plugins 含新版本 → 服务端
//     completeUpgradeIfMatchedWithRuntime 关单)
//
// Phase B (所有 daemon 都迁完到 ClawHub 后) 可以切到 `openclaw plugins update octo`
// 跳过 npx wrapper 直接 ClawHub native, 但**当前不行** — 还有装老版 npm 包的用户,
// `openclaw plugins update` 在他们机器上找不到注册的 ClawHub 插件会失败.
func (d *Daemon) handlePluginUpgrade(ctx context.Context, up *PendingUpgrade) error {
	log.Printf("[INFO] plugin upgrade task: %s → %s (task=%s)", up.Component, up.TargetVersion, up.TaskID)

	// cc-octo 一键安装:先尝试取 install secret(LLM 网关+key)。取到 → 走 install
	// 路径(npm install + configure),取不到(404)→ 落回下方普通 upgrade 路径。
	if up.Component == ccOctoPluginName {
		runtimeID := parseUpgradeRuntimeID(up.Metadata)
		if runtimeID > 0 {
			cfg, ferr := d.client.FetchCcOctoConfig(ctx, runtimeID, up.TaskID)
			if ferr != nil {
				if errors.Is(ferr, ErrCcOctoConfigUnavailable) {
					// Non-retryable: terminal/stale task, in-flight secret gone, or a 4xx
					// rejection. Skip without reporting failed — a terminal task would
					// reject the failed-transition and loop on replay; a stuck in-flight
					// install is reclaimed by fleet's sweeper timeout. Only transient
					// 5xx/network errors fall through to retry.
					log.Printf("[INFO] cc-octo install config unavailable (task terminal or secret gone), skipping (task=%s): %v", up.TaskID, ferr)
					return nil
				}
				// Transient (5xx / network): return error so SSE/heartbeat retry kicks in.
				return ferr
			}
			if cfg != nil {
				return d.handleCcOctoInstall(ctx, up, cfg)
			}
		} else if runtimeID <= 0 {
			log.Printf("[WARN] cc-octo install task %s: missing runtime_id in metadata, cannot fetch install config — falling back to plain upgrade", up.TaskID)
		}
		// cfg==nil / runtimeID==0 → 普通 upgrade,继续往下。
	}

	bin, args, ok := pluginUpgradeCommand(up.Component, up.TargetVersion)
	if !ok {
		msg := "unsupported plugin component: " + up.Component
		log.Printf("[ERROR] %s", msg)
		return d.reportUpgrade(ctx, up.TaskID, "failed", msg)
	}

	// dispatched → installing (progress — 失败 swallow)
	_ = d.reportUpgrade(ctx, up.TaskID, "installing", "")

	// 服务端插件 timeout 10 分钟；daemon 这里留 1 分钟 buffer 给上报，用 9 分钟
	installCtx, cancel := context.WithTimeout(ctx, 9*time.Minute)
	defer cancel()

	// octo (openclaw): `npx -y create-openclaw-octo install` — npm 包名硬编码，
	// 走 ClawHub native + 自动重启 gateway，不带版本号走 @latest。
	// cc-octo (claude): 首选 `cc-channel-octo upgrade <target>`(子命令内封装
	// npm 全局安装 + 重启)。但现网旧版 cc-channel-octo 可能没有 upgrade 子命令
	// (首次 rollout 的 chicken-egg),失败则回退 daemon 直接 `npm install -g` +
	// `cc-channel-octo restart`(restart 子命令旧版也有)。
	cmd := exec.CommandContext(installCtx, bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if up.Component == ccOctoPluginName {
			log.Printf("[WARN] cc-channel-octo upgrade subcommand failed (%v), falling back to npm install: %s",
				err, truncateOutput(string(out), 800))
			if ferr := d.ccOctoNpmFallback(installCtx, up.TargetVersion); ferr != nil {
				msg := fmt.Sprintf("cc-octo upgrade failed (subcommand: %v; npm fallback: %v)", err, ferr)
				log.Printf("[ERROR] %s", msg)
				return d.reportUpgrade(ctx, up.TaskID, "failed", msg)
			}
			log.Printf("[INFO] cc-octo npm fallback install + restart succeeded (task=%s)", up.TaskID)
		} else {
			msg := fmt.Sprintf("%s install failed: %v\noutput: %s", bin, err, truncateOutput(string(out), 2000))
			log.Printf("[ERROR] plugin upgrade failed: %s", msg)
			return d.reportUpgrade(ctx, up.TaskID, "failed", msg)
		}
	} else {
		log.Printf("[INFO] plugin upgrade install exited cleanly (task=%s)", up.TaskID)
	}

	// 安装命令退出只能保证 CLI 流程跑完，不能保证 gateway 已经带新代码起来。
	// 这里显式探 gateway，不通则上报 failed。给 gateway 一点启动时间。
	time.Sleep(2 * time.Second)
	if err := d.probePluginGateway(up.Component); err != nil {
		return d.reportUpgrade(ctx, up.TaskID, "failed", err.Error())
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

// ccOctoConfigureArgs builds `cc-channel-octo configure --gateway-url <url>`,
// optionally appending --model (gateway-level model id) and --api-url (the Octo
// IM server url, so the freshly-installed zero-bot gateway can boot — cc
// loadConfig requires apiUrl). The API key is passed via the
// CC_OCTO_CONFIGURE_API_KEY environment variable (not argv) to avoid leaking it
// in ps output. Pure for unit testing.
func ccOctoConfigureArgs(gatewayURL, model, apiURL string) []string {
	args := []string{"configure", "--gateway-url", gatewayURL}
	if model != "" {
		args = append(args, "--model", model)
	}
	if apiURL != "" {
		args = append(args, "--api-url", apiURL)
	}
	return args
}

// ccOctoStartArgs starts the gateway after a fresh install so the claude runtime
// comes online (idle, zero bots) without waiting for the first bot. Pure for
// unit testing.
func ccOctoStartArgs() []string { return []string{"start"} }

// parseUpgradeRuntimeID extracts the target runtime_id that fleet stamps into
// the upgrade task metadata (dispatchUpgrade forwards it). 0 if absent/bad.
func parseUpgradeRuntimeID(metadata string) int64 {
	if metadata == "" {
		return 0
	}
	var m struct {
		RuntimeID int64 `json:"runtime_id"`
	}
	if json.Unmarshal([]byte(metadata), &m) != nil {
		return 0
	}
	return m.RuntimeID
}

// redactSecret removes the api key from a string before logging.
func redactSecret(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "***redacted***")
}

// handleCcOctoInstall installs cc-channel-octo and writes the operator's LLM
// gateway + key into its global config. Unlike a plain upgrade it does NOT
// probe for a running gateway: a fresh install has no bound bot, so the gateway
// is not expected to be up. "Completion" is the daemon re-registering with the
// cc-octo plugin version (read from the installed binary by EnrichClaudeRuntime),
// which closes the task server-side.
func (d *Daemon) handleCcOctoInstall(ctx context.Context, up *PendingUpgrade, cfg *CcOctoConfig) error {
	log.Printf("[INFO] cc-octo install task: %s (task=%s)", up.TargetVersion, up.TaskID)
	_ = d.reportUpgrade(ctx, up.TaskID, "installing", "")

	installCtx, cancel := context.WithTimeout(ctx, 9*time.Minute)
	defer cancel()

	// 1. npm install -g the gateway so the `cc-channel-octo` bin exists.
	if out, err := exec.CommandContext(installCtx, "npm", ccOctoNpmInstallArgs(up.TargetVersion)...).CombinedOutput(); err != nil {
		msg := fmt.Sprintf("cc-octo npm install failed: %v\noutput: %s", err, truncateOutput(string(out), 1200))
		log.Printf("[ERROR] %s", msg)
		return d.reportUpgrade(ctx, up.TaskID, "failed", msg)
	}

	// 2. configure: write LLM gateway + key into the global config via env var
	// (CC_OCTO_CONFIGURE_API_KEY) to avoid leaking the key in ps output.
	configureCmd := exec.CommandContext(installCtx, "cc-channel-octo", ccOctoConfigureArgs(cfg.GatewayURL, cfg.Model, d.cfg.ServerURL)...)
	configureCmd.Env = append(os.Environ(), "CC_OCTO_CONFIGURE_API_KEY="+cfg.APIKey)
	cout, cerr := configureCmd.CombinedOutput()
	if cerr != nil {
		msg := truncateOutput(redactSecret(fmt.Sprintf("cc-channel-octo configure failed: %v\noutput: %s", cerr, string(cout)), cfg.APIKey), 800)
		log.Printf("[ERROR] %s", msg)
		return d.reportUpgrade(ctx, up.TaskID, "failed", msg)
	}
	log.Printf("[INFO] cc-octo configured (task=%s)", up.TaskID)

	// 2b. Start the gateway now so the claude runtime comes online immediately
	// (idle, zero bots) instead of waiting for the first bot. A start failure is
	// non-fatal: the first bot.provision runs `cc-channel-octo restart`, which
	// starts it anyway — so we log and continue to enrich/register.
	if sout, serr := exec.CommandContext(installCtx, "cc-channel-octo", ccOctoStartArgs()...).CombinedOutput(); serr != nil {
		log.Printf("[WARN] cc-octo post-install start failed (provision restart will retry): %v\noutput: %s", serr, truncateOutput(string(sout), 400))
	} else {
		log.Printf("[INFO] cc-octo gateway started idle (task=%s)", up.TaskID)
	}

	// 3. enrich + register so the new cc-octo version reaches fleet and closes
	// the task (completeUpgradeIfMatchedWithRuntime). No running-gateway probe.
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				log.Printf("[WARN] cc-octo install enrich aborted: %v", ctx.Err())
				return nil
			case <-time.After(time.Duration(attempt*5) * time.Second):
			}
		}
		enrichCtx, enrichCancel := context.WithTimeout(context.Background(), 60*time.Second)
		_, lastErr = d.enrichDetectAndRegister(enrichCtx)
		enrichCancel()
		if lastErr == nil {
			return nil
		}
		log.Printf("[WARN] cc-octo install enrich register attempt %d/%d failed: %v", attempt+1, maxAttempts, lastErr)
	}
	log.Printf("[WARN] cc-octo install enrich exhausted retries (last: %v), scheduling slow detect", lastErr)
	d.requestSlowDetect(ctx)
	return nil
}

// pluginUpgradeCommand maps an octo-adapter plugin component to the install
// command that upgrades it. Pure (no I/O) so the per-component routing is unit
// testable. Returns ok=false for non-plugin components (octo-daemon / provider
// CLIs go through other handlers).
//
//   - "octo"    → openclaw's installer; always pulls @latest (target ignored).
//   - "cc-octo" → cc-channel-octo's own `upgrade <target>` subcommand (bare
//     `upgrade` when target is empty → @latest).
func pluginUpgradeCommand(component, targetVersion string) (bin string, args []string, ok bool) {
	switch component {
	case "octo":
		return "npx", []string{"-y", "create-openclaw-octo", "install"}, true
	case ccOctoPluginName:
		args = []string{"upgrade"}
		if targetVersion != "" {
			args = append(args, targetVersion)
		}
		return "cc-channel-octo", args, true
	default:
		return "", nil, false
	}
}

// ccOctoNpmInstallArgs builds the `npm install -g @mininglamp-oss/cc-channel-octo@<v>`
// argument vector for the chicken-egg fallback (deployed cc-channel-octo predates
// the `upgrade` subcommand). Empty target → @latest. Pure for unit testing.
func ccOctoNpmInstallArgs(targetVersion string) []string {
	v := targetVersion
	if v == "" {
		v = "latest"
	}
	return []string{"install", "-g", "@mininglamp-oss/cc-channel-octo@" + v}
}

// ccOctoNpmFallback drives the upgrade directly via npm + restart when the
// cc-channel-octo `upgrade` subcommand is unavailable. npm install does NOT
// restart, so we explicitly run `cc-channel-octo restart` (which old versions
// already have) afterwards.
func (d *Daemon) ccOctoNpmFallback(ctx context.Context, targetVersion string) error {
	out, err := exec.CommandContext(ctx, "npm", ccOctoNpmInstallArgs(targetVersion)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("npm install: %v (output: %s)", err, truncateOutput(string(out), 800))
	}
	rout, rerr := exec.CommandContext(ctx, "cc-channel-octo", "restart").CombinedOutput()
	if rerr != nil {
		return fmt.Errorf("restart after npm install: %v (output: %s)", rerr, truncateOutput(string(rout), 800))
	}
	return nil
}

// probePluginGateway verifies the relevant gateway came back up after an
// install. A clean install exit doesn't guarantee the gateway restarted with
// the new code, so we probe and fail the task if it's down — otherwise the
// order would close as completed while the gateway still runs the old code.
func (d *Daemon) probePluginGateway(component string) error {
	switch component {
	case ccOctoPluginName:
		if !isCcChannelOctoRunning() {
			return fmt.Errorf("cc-octo installed but cc-channel-octo gateway is not running after restart")
		}
	case "octo":
		if openclawBin, lookErr := exec.LookPath("openclaw"); lookErr == nil {
			if !isOpenclawGatewayRunning(openclawBin) {
				return fmt.Errorf("plugin installed but openclaw gateway is not running after restart")
			}
		}
	}
	return nil
}
