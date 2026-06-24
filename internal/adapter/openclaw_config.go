package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// openclawConfigTimeout bounds the `openclaw config file` lookup + local file
// rewrite.
const openclawConfigTimeout = 30 * time.Second

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

// childMap returns parent[key] as a map[string]any, creating it if absent. If
// the existing value is present but not an object (only possible from a
// hand-edited / corrupt config), it is replaced with a fresh map and a [WARN] is
// logged so the overwrite is traceable rather than silent.
func childMap(parent map[string]any, key string) map[string]any {
	if existing, ok := parent[key]; ok {
		if m, ok := existing.(map[string]any); ok {
			return m
		}
		log.Printf("[WARN] [openclaw] config key %q was not an object (%T); overwriting with a fresh map", key, existing)
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

// openclawConfigMu serializes the read-merge-write cycle against openclaw.json.
// The daemon provisions up to openclawMaxConcurrency (5) bots in parallel; two
// concurrent read-modify-write cycles on the same file would otherwise drop
// whichever account/binding the slower writer never saw. One process-wide mutex
// is correct because a single daemon process owns its openclaw.json.
var openclawConfigMu sync.Mutex

// mergeAndWriteOctoConfig reads openclaw.json at path (absent → empty config),
// upserts the bot via mergeOctoBot, and writes the result back atomically
// (unique temp file + rename) so the gateway's file watcher never observes a
// half-written file. A missing file is created. The whole cycle holds
// openclawConfigMu so concurrent provisions don't clobber each other.
//
// Known boundary (openclaw noop-reload): this writes account + binding together,
// so it never produces a "account present, binding missing" half-state itself.
// If such a half-state exists from outside (legacy daemon / hand-edited config)
// and the account bytes happen to be unchanged, the resulting write touches only
// bindings — which is a noop reload in openclaw and won't take routing effect
// without a manual `openclaw gateway restart`. We do not paper over this with a
// forced dummy change; see the design doc.
func mergeAndWriteOctoConfig(path, workspaceID, botUID, botToken, apiURL string) error {
	openclawConfigMu.Lock()
	defer openclawConfigMu.Unlock()

	cfg := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read openclaw config %s: %w", path, err)
	}
	if err == nil && len(data) > 0 {
		// UseNumber keeps large integers / exact numeric forms in the user's
		// other config intact through the round-trip (plain Unmarshal would
		// coerce every number to float64 and rewrite e.g. big ints as 1e+18).
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&cfg); err != nil {
			return fmt.Errorf("parse openclaw config %s: %w", path, err)
		}
		// Decode reads only the first JSON value; reject trailing garbage so a
		// corrupt config fails closed instead of being silently truncated when
		// we marshal cfg back (json.Unmarshal would have rejected it too).
		if err := dec.Decode(&struct{}{}); err != io.EOF {
			return fmt.Errorf("parse openclaw config %s: unexpected trailing content after JSON", path)
		}
	}

	cfg = mergeOctoBot(cfg, workspaceID, botUID, botToken, apiURL)

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal openclaw config: %w", err)
	}
	// Unique temp file in the same dir so parallel writers never share a tmp
	// path; rename within the dir is atomic.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".openclaw-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp openclaw config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp openclaw config: %w", err)
	}
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp openclaw config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp openclaw config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename openclaw config: %w", err)
	}
	return nil
}

// writeOctoConfig locates openclaw.json via `openclaw config file`, then merges
// the bot's account + binding into it atomically. It replaces the old
// `config patch` + `agents bind` CLI steps: writing account and binding in one
// file mutation lets openclaw hot-reload pick up the new routing without a full
// gateway restart.
func writeOctoConfig(ctx context.Context, runner CLIRunner, req ProvisionRequest) error {
	cctx, cancel := context.WithTimeout(ctx, openclawConfigTimeout)
	defer cancel()
	out, err := runner.Run(cctx, openclawBin, []string{"config", "file"}, nil)
	if err != nil {
		return fmt.Errorf("openclaw config file: %w (output: %s)", err, truncate(string(out), 800))
	}
	path, err := parseConfigFilePath(string(out))
	if err != nil {
		return err
	}
	if err := mergeAndWriteOctoConfig(path, req.WorkspaceID, req.BotUID, req.BotToken, req.APIURL); err != nil {
		return err
	}
	log.Printf("[DEBUG] [openclaw] wrote octo account+binding for bot_uid=%s into %s", req.BotUID, path)
	return nil
}
