package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var versionRe = regexp.MustCompile(`v?(\d+\.\d+\.\d+)`)

// detectViaVersion is the shared "is this binary installed + what version"
// probe used by every adapter's Detect. It runs `<bin> --version` with a short
// timeout and parses a semver out of the output. A missing binary is reported
// as ErrNotInstalled (Available stays false).
func detectViaVersion(ctx context.Context, runner CLIRunner, bin, provider string) (RuntimeInfo, error) {
	info := RuntimeInfo{Provider: provider}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := runner.Run(cctx, bin, []string{"--version"}, nil)
	if err != nil {
		return info, fmt.Errorf("%w: %v", ErrNotInstalled, err)
	}
	info.Available = true
	info.BinPath = bin
	if m := versionRe.FindStringSubmatch(string(out)); m != nil {
		info.Version = m[1]
	}
	return info, nil
}

// ExtractReplyFromEnvelope pulls a human-readable reply out of an agent CLI's
// `--json` stdout. It is lifted verbatim (behaviour-preserving) from the
// daemon's original openclaw reply parsing so all adapters share one shape
// matcher. Returns "" when nothing recognizable is found; callers decide on a
// raw-stdout fallback.
//
// Handled shapes, in priority order:
//   - openclaw envelope { payloads: [ { text } ] }
//   - openclaw embedded-agent finalAssistantVisibleText / finalAssistantRawText
//   - generic flat keys reply/text/content/message/answer/output
//   - nested { result|data: { text|content|message|reply } }
//   - line-delimited JSONL (walked in reverse so the final answer wins)
func ExtractReplyFromEnvelope(stdout []byte) string {
	trimmed := strings.TrimSpace(string(stdout))
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
		return extractFromJSONL(trimmed)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(trimmed[jsonStart:]), &env); err != nil {
		return extractFromJSONL(trimmed)
	}
	return replyFromEnvelope(env)
}

// replyFromEnvelope walks the openclaw / generic-agent shapes. Order matters:
// payloads is the openclaw native shape; finalAssistantVisibleText is the
// embedded-agent shape; the flat keys cover claude-like CLIs.
func replyFromEnvelope(env map[string]any) string {
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
	if s, ok := env["finalAssistantVisibleText"].(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	if s, ok := env["finalAssistantRawText"].(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	for _, key := range []string{"reply", "text", "content", "message", "answer", "output"} {
		if v, ok := env[key]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
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

// truncate bounds CLI output captured into error messages.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}
