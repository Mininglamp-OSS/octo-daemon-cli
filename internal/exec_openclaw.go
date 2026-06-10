package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-daemon-cli/internal/adapter"
)

// handleBotProvision runs the openclaw side-effects for a "bot.provision"
// command in one shot:
//  1. openclaw agents add <workspace> --non-interactive --workspace ...
//  2. openclaw config patch (channels.octo.accounts.<bot_uid>)
//  3. openclaw agents bind --agent <workspace> --bind octo:<bot_uid>
//
// Then forces enrichDetectAndRegister BEFORE ack so server metadata
// reflects the new openclaw state when the Web UI re-fetches.
// Failure at any step → ack(failed, "<step>: <err>"); a partial state on
// disk is acceptable for PoC (operator can clean up manually).
func (d *Daemon) handleBotProvision(ctx context.Context, cmd *PendingAgentCommand) error {
	log.Printf("[INFO] [bot.provision] received id=%d workspace=%s bot=%s",
		cmd.ID, cmd.WorkspaceID, cmd.BotUID)
	log.Printf("[DEBUG] [bot.provision] command received: %s", debugDumpCommand(cmd))

	if cmd.WorkspaceID == "" || cmd.BotUID == "" {
		return d.ackBotProvision(ctx, cmd, "failed", "missing workspace_id/bot_uid")
	}

	// PR-A.2: fleet no longer carries bot_token in the heartbeat payload
	// — it lives only in server's robot table. Daemon fetches it here
	// using its daemon-scope JWT (server validates JWT.sub matches
	// robot.creator_uid). api_url is always sourced locally — fleet
	// no longer fills it (fleet's External.BaseURL is fleet's own URL,
	// not server's; fleet has no reliable IM-server URL to send).
	// Daemon uses OCTO_SERVER_URL env or its own --api-url flag as
	// single source of truth.
	if cmd.BotToken == "" {
		tokCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		tok, err := d.client.GetBotToken(tokCtx, cmd.BotUID)
		cancel()
		if err != nil {
			return d.ackBotProvision(ctx, cmd, "failed", fmt.Sprintf("fetch bot_token: %v", err))
		}
		cmd.BotToken = tok
	}
	if cmd.APIURL == "" {
		cmd.APIURL = os.Getenv("OCTO_SERVER_URL")
		if cmd.APIURL == "" {
			cmd.APIURL = d.cfg.APIURL
		}
	}

	ad, err := d.runtimeAdapter("")
	if err != nil {
		return d.ackBotProvision(ctx, cmd, "failed", fmt.Sprintf("resolve adapter: %v", err))
	}
	if _, err := ad.Provision(ctx, adapter.ProvisionRequest{
		WorkspaceID: cmd.WorkspaceID,
		BotUID:      cmd.BotUID,
		BotToken:    cmd.BotToken,
		APIURL:      cmd.APIURL,
		DisplayName: cmd.DisplayName,
	}); err != nil {
		log.Printf("[ERROR] [bot.provision] provision failed: %v", err)
		return d.ackBotProvision(ctx, cmd, "failed", fmt.Sprintf("provision: %v", err))
	}

	if _, err := d.enrichDetectAndRegister(ctx); err != nil {
		log.Printf("[WARN] [bot.provision] post-action enrich+register failed: %v", err)
	}

	return d.ackBotProvision(ctx, cmd, "active", "")
}

// Deprecated: superseded by adapter.OpenclawAdapter.Provision. Retained
// temporarily during the runtime-adapter migration for reference/rollback; no
// longer called. Remove once the adapter path is proven.
//
//nolint:unused
func addOpenclawWorkspace(ctx context.Context, cmd *PendingAgentCommand) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home: %w", err)
	}
	workspace := filepath.Join(home, ".openclaw", "workspaces", cmd.WorkspaceID)
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	args := []string{"agents", "add", cmd.WorkspaceID,
		"--non-interactive",
		"--workspace", workspace,
	}
	log.Printf("[DEBUG] [bot.provision] exec: openclaw %s", strings.Join(args, " "))
	c := exec.CommandContext(cctx, "openclaw", args...)
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("openclaw agents add: %w (output: %s)", err, truncateOutput(string(out), 800))
	}
	return nil
}

// Deprecated: superseded by adapter.OpenclawAdapter.Provision. Retained
// temporarily during the runtime-adapter migration for reference/rollback; no
// longer called. Remove once the adapter path is proven.
//
//nolint:unused
func patchOctoAccount(ctx context.Context, cmd *PendingAgentCommand) error {
	patch := map[string]any{
		"channels": map[string]any{
			"octo": map[string]any{
				"accounts": map[string]any{
					cmd.BotUID: map[string]any{
						"botToken": cmd.BotToken,
						"apiUrl":   cmd.APIURL,
						// name 是 openclaw routing key, 必须等于 agent name
						// (= workspace_id, 由 `agents add` 创建). 不能用
						// DisplayName (那是用户给 bot 起的显示名, 跟 agent
						// 路由无关) — 否则 octo channel 收到 IM 消息时
						// 找不到对应 agent, fallback 到默认 main agent.
						"name": cmd.WorkspaceID,
					},
				},
			},
		},
	}
	buf, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dump := string(buf)
	if cmd.BotToken != "" {
		dump = strings.ReplaceAll(dump, cmd.BotToken, "***redacted***")
	}
	log.Printf("[DEBUG] [bot.provision] exec: openclaw config patch --stdin payload=%s", dump)
	c := exec.CommandContext(cctx, "openclaw", "config", "patch", "--stdin")
	c.Stdin = strings.NewReader(string(buf))
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("openclaw config patch: %w (output: %s)", err, truncateOutput(string(out), 800))
	}
	return nil
}

// Deprecated: superseded by adapter.OpenclawAdapter.Provision. Retained
// temporarily during the runtime-adapter migration for reference/rollback; no
// longer called. Remove once the adapter path is proven.
//
//nolint:unused
func bindBotToWorkspace(ctx context.Context, cmd *PendingAgentCommand) error {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	bind := fmt.Sprintf("octo:%s", cmd.BotUID)
	args := []string{"agents", "bind",
		"--agent", cmd.WorkspaceID,
		"--bind", bind,
	}
	log.Printf("[DEBUG] [bot.provision] exec: openclaw %s", strings.Join(args, " "))
	c := exec.CommandContext(cctx, "openclaw", args...)
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("openclaw agents bind: %w (output: %s)", err, truncateOutput(string(out), 800))
	}
	return nil
}

// ackBotProvision 把 status 发回 fleet. 返回 AckBot 的实际 err.
//
// caller 决定 swallow vs propagate (Jerry-Xin Critical fix):
// 现在所有调用点都是 terminal (failed/active), 必须 `return d.ackBotProvision(...)`
// 让 handler 把 ack 失败往上抛 → adapter (HandleBotProvision) 透传 →
// dispatcher 不 markDone → SSE replay / heartbeat 兜底重试. 不传则
// fleet 端 row 永远停在 bot_minted, daemon 本地已 markDone 不再处理,
// 直到 sweeper timeout 误判.
func (d *Daemon) ackBotProvision(ctx context.Context, cmd *PendingAgentCommand, status, errMsg string) error {
	ackCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := d.client.AckBot(ackCtx, cmd.ID, cmd.ClaimToken, status, errMsg); err != nil {
		log.Printf("[ERROR] [bot.provision] ack id=%d status=%s failed: %v", cmd.ID, status, err)
		return err
	}
	log.Printf("[INFO] [bot.provision] acked id=%d status=%s", cmd.ID, status)
	return nil
}

// debugDumpCommand marshals a received command to JSON for [DEBUG] logging,
// with token-bearing fields masked so secrets never reach the logs.
func debugDumpCommand(cmd *PendingAgentCommand) string {
	redacted := *cmd
	if redacted.BotToken != "" {
		redacted.BotToken = "***redacted***"
	}
	if redacted.ClaimToken != "" {
		redacted.ClaimToken = "***redacted***"
	}
	b, err := json.Marshal(redacted)
	if err != nil {
		return fmt.Sprintf("<marshal err: %v>", err)
	}
	return string(b)
}
