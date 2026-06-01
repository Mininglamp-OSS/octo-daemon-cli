package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"
)

// agentTaskTimeout caps how long a single openclaw agent run may take. The
// openclaw CLI itself defaults to 600s (--timeout), but the daemon enforces
// its own hard ceiling so a stuck CLI subprocess can't pin a task forever.
const agentTaskTimeout = 10 * time.Minute

// maxResultSummaryBytes bounds what we send back to the server. The bot's
// reply is typically a few KB; the cap is just guard rail against pathological
// runaway output (e.g. agent dumping a large file).
const maxResultSummaryBytes = 64 * 1024

// handleBotTask runs the agent prompt under `openclaw agent` and acks back.
// Always acks — failure path sends status=failed + error_msg so the server
// can surface the failure to the matter timeline.
func (d *Daemon) handleBotTask(ctx context.Context, task *PendingBotTask) {
	log.Printf("[INFO] [bot-task] received id=%d agent=%s matter=%s bot=%s",
		task.ID, task.AgentID, task.MatterID, task.BotUID)

	if task.AgentID == "" {
		d.ackBotTask(ctx, task, "failed", "", "missing agent_id in task")
		return
	}
	if strings.TrimSpace(task.Prompt) == "" {
		d.ackBotTask(ctx, task, "failed", "", "empty prompt")
		return
	}

	result, err := runOpenclawAgent(ctx, task.AgentID, task.Prompt)
	if err != nil {
		log.Printf("[ERROR] [bot-task] id=%d openclaw agent failed: %v", task.ID, err)
		d.ackBotTask(ctx, task, "failed", "", err.Error())
		return
	}
	summary := truncateOutput(result, maxResultSummaryBytes)
	log.Printf("[INFO] [bot-task] id=%d succeeded, %d bytes result", task.ID, len(summary))
	d.ackBotTask(ctx, task, "succeeded", summary, "")
}

// runOpenclawAgent invokes `openclaw agent --agent <id> --json -m <prompt>`
// and parses the agent's text reply out of the JSON envelope. The local-vs-
// gateway choice is left to openclaw's own resolution (we don't pass --local)
// so the bot's already-configured channels.octo binding stays in effect.
func runOpenclawAgent(parent context.Context, agentID, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, agentTaskTimeout)
	defer cancel()

	c := exec.CommandContext(ctx, "openclaw", "agent",
		"--agent", agentID,
		"--json",
		"-m", prompt,
	)
	out, err := c.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("openclaw agent: %w (output: %s)", err, truncateOutput(string(out), 800))
	}

	reply := extractAgentReply(out)
	if strings.TrimSpace(reply) == "" {
		// JSON parse failed or no recognizable reply field; fall back to raw
		// stdout so the matter timeline still gets *something* useful.
		reply = strings.TrimSpace(string(out))
	}
	if reply == "" {
		return "", fmt.Errorf("openclaw agent produced no output")
	}
	return reply, nil
}

// extractAgentReply pulls a human-readable reply out of `openclaw agent --json`
// output. The CLI emits a single multi-line JSON object with a `payloads` array
// (and `meta`), prefixed by some stderr-like banner lines from the gateway. We
// locate the start of the JSON envelope and decode the rest, then walk a few
// known shapes (openclaw `payloads`, finalAssistantVisibleText, plus simpler
// claude/codex-style flat fields) in priority order.
func extractAgentReply(out []byte) string {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return ""
	}
	// Locate the start of the JSON envelope. The CLI emits diagnostic banner
	// lines (gateway errors, plugin notices) before the actual JSON object;
	// find the first `{` at the start of a line.
	jsonStart := -1
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] == '{' && (i == 0 || trimmed[i-1] == '\n') {
			jsonStart = i
			break
		}
	}
	if jsonStart < 0 {
		// Fall back to per-line JSONL parsing for non-openclaw agent CLIs.
		return extractFromJSONL(trimmed)
	}
	jsonStr := trimmed[jsonStart:]
	var env map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &env); err != nil {
		return extractFromJSONL(trimmed)
	}
	if reply := replyFromEnvelope(env); reply != "" {
		return reply
	}
	return ""
}

// replyFromEnvelope walks the openclaw / generic-agent shapes. Order matters:
// payloads is the openclaw native shape; finalAssistantVisibleText is the
// embedded-agent shape; the flat keys cover claude/codex-like CLIs.
func replyFromEnvelope(env map[string]any) string {
	// openclaw shape: { payloads: [ { text: "..." } ] }
	if payloads, ok := env["payloads"].([]any); ok {
		var parts []string
		for _, p := range payloads {
			if obj, ok := p.(map[string]any); ok {
				if s, ok := obj["text"].(string); ok && strings.TrimSpace(s) != "" {
					parts = append(parts, s)
				}
			}
		}
		if joined := strings.TrimSpace(strings.Join(parts, "\n")); joined != "" {
			return joined
		}
	}
	// openclaw embedded agent meta shape
	if s, ok := env["finalAssistantVisibleText"].(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	if s, ok := env["finalAssistantRawText"].(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	// Generic flat keys for claude/codex-style envelopes
	for _, key := range []string{"reply", "text", "content", "message", "answer", "output"} {
		if v, ok := env[key]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	// Nested {result|data: {text|message|reply: "..."}}
	for _, key := range []string{"result", "data"} {
		if v, ok := env[key]; ok {
			if obj, ok := v.(map[string]any); ok {
				for _, sub := range []string{"text", "content", "message", "reply"} {
					if s, ok := obj[sub].(string); ok && strings.TrimSpace(s) != "" {
						return s
					}
				}
			}
		}
	}
	return ""
}

// extractFromJSONL handles the line-delimited JSON case (some agent CLIs emit
// one event per line). Walk lines in reverse so the final answer wins over
// intermediate thinking events.
func extractFromJSONL(trimmed string) string {
	lines := strings.Split(trimmed, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var env map[string]any
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			continue
		}
		if reply := replyFromEnvelope(env); reply != "" {
			return reply
		}
	}
	return ""
}

func (d *Daemon) ackBotTask(ctx context.Context, task *PendingBotTask, status, resultSummary, errMsg string) {
	ackCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := d.client.AckBotTask(ackCtx, task.ID, task.ClaimToken, status, resultSummary, errMsg); err != nil {
		log.Printf("[ERROR] [bot-task] ack id=%d status=%s failed: %v", task.ID, status, err)
		return
	}
	log.Printf("[INFO] [bot-task] acked id=%d status=%s", task.ID, status)
}
