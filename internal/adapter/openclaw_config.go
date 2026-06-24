package adapter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// parseConfigFilePath extracts the openclaw.json path from `openclaw config
// file` output. openclaw may prepend a banner or plugin log lines to stdout,
// so scan for the line ending in openclaw.json rather than trusting line count.
// The returned path is normalized (~ expanded; a non-absolute path resolved
// against home) so os.ReadFile / os.Rename act on the same file the gateway
// watches.
func parseConfigFilePath(out string) (string, error) {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasSuffix(l, "openclaw.json") {
			return normalizeConfigPath(l), nil
		}
	}
	return "", fmt.Errorf("openclaw config file: no openclaw.json path in output: %q", truncate(out, 200))
}

// normalizeConfigPath expands a leading ~ to the user's home dir, and resolves a
// non-absolute path against home (openclaw runs unix-side here; Windows ~\ is
// out of scope, tracked separately under daemon Windows support). A path that
// can't be resolved (no home) is returned unchanged.
func normalizeConfigPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
		return p
	}
	if !filepath.IsAbs(p) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p)
		}
	}
	return p
}

// mergeOctoBot upserts one bot's octo account and routing binding into cfg,
// leaving every other key untouched. It returns cfg (mutated in place; a nil
// cfg is treated as a fresh map). account + binding land in the SAME write so
// openclaw's reload plan sees both channels.octo.* and bindings change at once
// and swaps the routing snapshot — a binding written alone is a noop reload and
// never takes effect without a full gateway restart. It deliberately does NOT
// write session.dmScope: that is a global setting owned by create-openclaw-octo
// / the user and unrelated to this routing fix.
func mergeOctoBot(cfg map[string]any, workspaceID, botUID, botToken, apiURL string) map[string]any {
	if cfg == nil {
		cfg = map[string]any{}
	}

	channels := childMap(cfg, "channels")
	octo := childMap(channels, "octo")
	accounts := childMap(octo, "accounts")
	accounts[botUID] = map[string]any{
		"botToken": botToken,
		"apiUrl":   apiURL,
		// name is octo's routing key and must equal the agent name created by
		// `agents add` (= workspaceID), not the user-facing display name, or
		// inbound IM can't route to this agent and falls back to main.
		"name":           workspaceID,
		"requireMention": true,
	}

	cfg["bindings"] = upsertBinding(cfg["bindings"], workspaceID, botUID)

	return cfg
}

// childMap returns parent[key] as a map[string]any, creating it if absent or if
// the existing value is not an object (defensive against hand-edited config).
func childMap(parent map[string]any, key string) map[string]any {
	if m, ok := parent[key].(map[string]any); ok {
		return m
	}
	m := map[string]any{}
	parent[key] = m
	return m
}

// upsertBinding finds the octo binding for botUID and updates its agentId, or
// appends a new one. Existing fields on a matched binding (e.g. "type") are
// preserved. raw is the current cfg["bindings"] value (may be nil or non-slice).
func upsertBinding(raw any, workspaceID, botUID string) []any {
	binds, _ := raw.([]any)
	for _, item := range binds {
		b, ok := item.(map[string]any)
		if !ok {
			continue
		}
		m, ok := b["match"].(map[string]any)
		if ok && m["channel"] == "octo" && m["accountId"] == botUID {
			b["agentId"] = workspaceID
			return binds
		}
	}
	return append(binds, map[string]any{
		"agentId": workspaceID,
		"match":   map[string]any{"channel": "octo", "accountId": botUID},
	})
}

