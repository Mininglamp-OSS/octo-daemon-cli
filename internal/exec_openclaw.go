package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-daemon-cli/internal/adapter"
)

// handleBotProvision handles a "bot.provision" command by resolving the runtime
// adapter for cmd.RuntimeKind and delegating to its Provision (for openclaw:
// `agents add` + an atomic openclaw.json account/binding write).
//
// Then forces enrichDetectAndRegister BEFORE ack so server metadata
// reflects the new openclaw state when the Web UI re-fetches.
// Failure at any step → ack(failed, "<step>: <err>"); a partial state on
// disk is acceptable for PoC (operator can clean up manually).
func (d *Daemon) handleBotProvision(ctx context.Context, cmd *PendingAgentCommand) error {
	log.Printf("[INFO] [bot.provision] received id=%d workspace=%s bot=%s",
		cmd.ID, cmd.WorkspaceID, cmd.BotUID)
	log.Printf("[DEBUG] [bot.provision] command received: %s", debugDumpCommand(cmd))

	if err := validateProvisionID("workspace_id", cmd.WorkspaceID); err != nil {
		return d.ackBotProvision(ctx, cmd, "failed", err.Error())
	}
	if err := validateProvisionID("bot_uid", cmd.BotUID); err != nil {
		return d.ackBotProvision(ctx, cmd, "failed", err.Error())
	}

	// PR-A.2: fleet no longer carries bot_token in the heartbeat payload
	// — it lives only in server's robot table. Daemon fetches it here
	// using its daemon-scope JWT (server validates JWT.sub matches
	// robot.creator_uid). api_url is always sourced locally — fleet
	// no longer fills it (fleet's External.BaseURL is fleet's own URL,
	// not server's; fleet has no reliable IM-server URL to send).
	// Daemon uses the profile's server_url as the single source of truth.
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
		cmd.APIURL = d.cfg.ServerURL
	}

	ad, err := d.runtimeAdapter(cmd.RuntimeKind)
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
		msg := fmt.Sprintf("provision: %v", err)
		if cmd.BotToken != "" {
			msg = strings.ReplaceAll(msg, cmd.BotToken, "***redacted***")
		}
		log.Printf("[ERROR] [bot.provision] %s", msg)
		return d.ackBotProvision(ctx, cmd, "failed", msg)
	}

	if _, err := d.enrichDetectAndRegister(ctx); err != nil {
		log.Printf("[WARN] [bot.provision] post-action enrich+register failed: %v", err)
	}

	return d.ackBotProvision(ctx, cmd, "active", "")
}

// validateProvisionID rejects a server-supplied id that is unsafe to use as a
// filesystem path segment or a CLI argument. Allowed: ASCII letters, digits,
// '_' and '-', first char not '-', length 1..128. This blocks path traversal
// ("..", "/" would escape the intended dir in filepath.Join) and argument
// injection (a leading '-' would be parsed as a flag by the runtime CLIs).
// Validating once at this ingress means every adapter inherits the guard.
func validateProvisionID(field, value string) error {
	if value == "" {
		return fmt.Errorf("%s is empty", field)
	}
	if len(value) > 128 {
		return fmt.Errorf("%s too long (%d > 128)", field, len(value))
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return fmt.Errorf("%s has invalid character %q (allowed: A-Za-z0-9_-)", field, c)
		}
	}
	if value[0] == '-' {
		return fmt.Errorf("%s must not start with '-'", field)
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
