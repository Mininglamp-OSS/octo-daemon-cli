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

// handleManagedAgentCommand runs openclaw side-effects for an agent.create
// or bot.add command pushed via heartbeat, then ack's back to the server.
// Always acks — failure cases ack status=failed with an error_msg so the
// Web UI can show why.
//
// Two flavors (PoC):
//
//	"agent.create" — create a bare openclaw agent (no bot, no binding):
//	  openclaw agents add <agent_id> --non-interactive --workspace ...
//
//	"bot.add" — mint a new bot bound to an existing agent:
//	  1. openclaw config patch --stdin (channels.octo.accounts.<bot_uid>)
//	  2. openclaw agents bind --agent <agent_id> --bind octo:<bot_uid>
//
// After a successful action we synchronously trigger an enrich+register so
// the server's agent_runtime.metadata reflects the new openclaw state BEFORE
// we ack — that way the next Web poll (immediately after seeing "active") is
// guaranteed to include the new agent/bot.
func (d *Daemon) handleManagedAgentCommand(ctx context.Context, cmd *PendingAgentCommand) {
	log.Printf("[INFO] [managed-agent] command received: id=%d action=%s agent_id=%s bot_uid=%s",
		cmd.ID, cmd.Action, cmd.AgentID, cmd.BotUID)

	if cmd.AgentID == "" {
		d.ackManagedAgent(ctx, cmd, "failed", "missing agent_id in command")
		return
	}

	switch cmd.Action {
	case "agent.create":
		// PoC: agent.create no longer carries a bot. Workspace-only.
		if err := addOpenclawAgent(ctx, cmd); err != nil {
			log.Printf("[ERROR] [managed-agent] openclaw agents add failed: %v", err)
			d.ackManagedAgent(ctx, cmd, "failed", fmt.Sprintf("agents add: %v", err))
			return
		}
		log.Printf("[INFO] [managed-agent] openclaw agent %s created", cmd.AgentID)
	case "bot.add":
		if cmd.BotUID == "" || cmd.BotToken == "" || cmd.APIURL == "" {
			d.ackManagedAgent(ctx, cmd, "failed", "bot.add requires bot_uid/bot_token/api_url")
			return
		}
		if err := patchOctoAccount(ctx, cmd); err != nil {
			log.Printf("[ERROR] [managed-agent] patch octo account failed: %v", err)
			d.ackManagedAgent(ctx, cmd, "failed", fmt.Sprintf("patch octo account: %v", err))
			return
		}
		log.Printf("[INFO] [managed-agent] octo account %s patched into openclaw config", cmd.BotUID)
		if err := bindBotToOpenclawAgent(ctx, cmd); err != nil {
			log.Printf("[ERROR] [managed-agent] openclaw agents bind failed: %v", err)
			d.ackManagedAgent(ctx, cmd, "failed", fmt.Sprintf("agents bind: %v", err))
			return
		}
		log.Printf("[INFO] [managed-agent] octo:%s bound to existing agent %s", cmd.BotUID, cmd.AgentID)
	default:
		d.ackManagedAgent(ctx, cmd, "failed", fmt.Sprintf("unsupported action %q", cmd.Action))
		return
	}

	// Force enrich+re-register so server has the new openclaw state in its
	// agent_runtime.metadata BEFORE we ack. Best-effort: a failed re-register
	// is logged but does NOT downgrade the action's outcome — openclaw side-
	// effects already succeeded; metadata staleness self-heals on the next
	// detect cycle.
	if _, err := d.enrichDetectAndRegister(ctx); err != nil {
		log.Printf("[WARN] [managed-agent] post-action enrich+register failed (will self-heal): %v", err)
	} else {
		log.Printf("[INFO] [managed-agent] server metadata synced before ack")
	}

	d.ackManagedAgent(ctx, cmd, "active", "")
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

func addOpenclawAgent(ctx context.Context, cmd *PendingAgentCommand) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home: %w", err)
	}
	workspace := filepath.Join(home, ".openclaw", "workspaces", cmd.AgentID)

	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// PoC: agent.create no longer comes with a bot, so no --bind. Bots are
	// attached separately via bot.add (which calls bindBotToOpenclawAgent).
	c := exec.CommandContext(cctx, "openclaw", "agents", "add", cmd.AgentID,
		"--non-interactive",
		"--workspace", workspace,
	)
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("openclaw agents add: %w (output: %s)", err, truncateOutput(string(out), 800))
	}
	return nil
}

// bindBotToOpenclawAgent adds an octo:<bot_uid> binding to an EXISTING
// openclaw agent. Used by the "bot.add" command. Unlike `agents add`, this
// does not create a workspace — the agent must already exist.
func bindBotToOpenclawAgent(ctx context.Context, cmd *PendingAgentCommand) error {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	bind := fmt.Sprintf("octo:%s", cmd.BotUID)
	c := exec.CommandContext(cctx, "openclaw", "agents", "bind",
		"--agent", cmd.AgentID,
		"--bind", bind,
	)
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("openclaw agents bind: %w (output: %s)", err, truncateOutput(string(out), 800))
	}
	return nil
}

func (d *Daemon) ackManagedAgent(ctx context.Context, cmd *PendingAgentCommand, status, errMsg string) {
	ackCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := d.client.AckManagedAgent(ackCtx, cmd.ID, cmd.ClaimToken, status, errMsg); err != nil {
		log.Printf("[ERROR] [managed-agent] ack id=%d status=%s failed: %v", cmd.ID, status, err)
		return
	}
	log.Printf("[INFO] [managed-agent] acked id=%d status=%s", cmd.ID, status)
}
