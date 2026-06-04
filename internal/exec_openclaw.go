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
)

// handleBotProvision runs the openclaw side-effects for a "bot.provision"
// command in one shot:
//   1. openclaw agents add <workspace> --non-interactive --workspace ...
//   2. openclaw config patch (channels.octo.accounts.<bot_uid>)
//   3. openclaw agents bind --agent <workspace> --bind octo:<bot_uid>
//
// Then forces enrichDetectAndRegister BEFORE ack so server metadata
// reflects the new openclaw state when the Web UI re-fetches.
// Failure at any step → ack(failed, "<step>: <err>"); a partial state on
// disk is acceptable for PoC (operator can clean up manually).
func (d *Daemon) handleBotProvision(ctx context.Context, cmd *PendingAgentCommand) {
	log.Printf("[INFO] [bot.provision] received id=%d workspace=%s bot=%s",
		cmd.ID, cmd.WorkspaceID, cmd.BotUID)

	if cmd.WorkspaceID == "" || cmd.BotUID == "" {
		d.ackBotProvision(ctx, cmd, "failed", "missing workspace_id/bot_uid")
		return
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
			d.ackBotProvision(ctx, cmd, "failed", fmt.Sprintf("fetch bot_token: %v", err))
			return
		}
		cmd.BotToken = tok
	}
	if cmd.APIURL == "" {
		cmd.APIURL = os.Getenv("OCTO_SERVER_URL")
		if cmd.APIURL == "" {
			cmd.APIURL = d.cfg.APIURL
		}
	}

	if err := addOpenclawWorkspace(ctx, cmd); err != nil {
		log.Printf("[ERROR] [bot.provision] add workspace failed: %v", err)
		d.ackBotProvision(ctx, cmd, "failed", fmt.Sprintf("agents add: %v", err))
		return
	}
	if err := patchOctoAccount(ctx, cmd); err != nil {
		log.Printf("[ERROR] [bot.provision] patch octo account failed: %v", err)
		d.ackBotProvision(ctx, cmd, "failed", fmt.Sprintf("config patch: %v", err))
		return
	}
	if err := bindBotToWorkspace(ctx, cmd); err != nil {
		log.Printf("[ERROR] [bot.provision] bind failed: %v", err)
		d.ackBotProvision(ctx, cmd, "failed", fmt.Sprintf("agents bind: %v", err))
		return
	}

	if _, err := d.enrichDetectAndRegister(ctx); err != nil {
		log.Printf("[WARN] [bot.provision] post-action enrich+register failed: %v", err)
	}

	d.ackBotProvision(ctx, cmd, "active", "")
}

func addOpenclawWorkspace(ctx context.Context, cmd *PendingAgentCommand) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home: %w", err)
	}
	workspace := filepath.Join(home, ".openclaw", "workspaces", cmd.WorkspaceID)
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	c := exec.CommandContext(cctx, "openclaw", "agents", "add", cmd.WorkspaceID,
		"--non-interactive",
		"--workspace", workspace,
	)
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("openclaw agents add: %w (output: %s)", err, truncateOutput(string(out), 800))
	}
	return nil
}

func patchOctoAccount(ctx context.Context, cmd *PendingAgentCommand) error {
	patch := map[string]any{
		"channels": map[string]any{
			"octo": map[string]any{
				"accounts": map[string]any{
					cmd.BotUID: map[string]any{
						"botToken": cmd.BotToken,
						"apiUrl":   cmd.APIURL,
						"name":     cmd.DisplayName,
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
	c := exec.CommandContext(cctx, "openclaw", "config", "patch", "--stdin")
	c.Stdin = strings.NewReader(string(buf))
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("openclaw config patch: %w (output: %s)", err, truncateOutput(string(out), 800))
	}
	return nil
}

func bindBotToWorkspace(ctx context.Context, cmd *PendingAgentCommand) error {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	bind := fmt.Sprintf("octo:%s", cmd.BotUID)
	c := exec.CommandContext(cctx, "openclaw", "agents", "bind",
		"--agent", cmd.WorkspaceID,
		"--bind", bind,
	)
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("openclaw agents bind: %w (output: %s)", err, truncateOutput(string(out), 800))
	}
	return nil
}

func (d *Daemon) ackBotProvision(ctx context.Context, cmd *PendingAgentCommand, status, errMsg string) {
	ackCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := d.client.AckBot(ackCtx, cmd.ID, cmd.ClaimToken, status, errMsg); err != nil {
		log.Printf("[ERROR] [bot.provision] ack id=%d status=%s failed: %v", cmd.ID, status, err)
		return
	}
	log.Printf("[INFO] [bot.provision] acked id=%d status=%s", cmd.ID, status)
}
