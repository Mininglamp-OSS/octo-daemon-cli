package adapter

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseConfigFilePath(t *testing.T) {
	home, _ := os.UserHomeDir()
	cases := []struct {
		name string
		out  string
		want string
	}{
		{"clean", "/Users/x/.openclaw/openclaw.json\n", "/Users/x/.openclaw/openclaw.json"},
		{"with banner", "🦞 OpenClaw 2026.6.6\n[plugins] octo loaded\n/home/u/.openclaw/openclaw.json\n", "/home/u/.openclaw/openclaw.json"},
		{"trailing spaces", "  /tmp/cfg/openclaw.json  \n", "/tmp/cfg/openclaw.json"},
		{"tilde expands to home", "~/.openclaw/openclaw.json\n", filepath.Join(home, ".openclaw", "openclaw.json")},
		{"relative resolves against home", "openclaw.json\n", filepath.Join(home, "openclaw.json")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseConfigFilePath(c.out)
			if err != nil {
				t.Fatalf("parseConfigFilePath: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	if _, err := parseConfigFilePath("no path here\n[plugins] noise\n"); err == nil {
		t.Error("expected error when no openclaw.json line present")
	}
}

func TestMergeOctoBotFresh(t *testing.T) {
	cfg := mergeOctoBot(nil, "ws-1", "bot-abc", "bf_tok", "https://api.x")

	ch := cfg["channels"].(map[string]any)["octo"].(map[string]any)
	acc := ch["accounts"].(map[string]any)["bot-abc"].(map[string]any)
	if acc["botToken"] != "bf_tok" || acc["apiUrl"] != "https://api.x" ||
		acc["name"] != "ws-1" || acc["requireMention"] != true {
		t.Errorf("account fields wrong: %#v", acc)
	}

	binds := cfg["bindings"].([]any)
	if len(binds) != 1 {
		t.Fatalf("bindings len = %d, want 1", len(binds))
	}
	b := binds[0].(map[string]any)
	m := b["match"].(map[string]any)
	if b["agentId"] != "ws-1" || m["channel"] != "octo" || m["accountId"] != "bot-abc" {
		t.Errorf("binding wrong: %#v", b)
	}

	// daemon must NOT introduce session.dmScope (it's a global config owned by
	// create-openclaw-octo / the user, unrelated to #27 routing).
	if _, ok := cfg["session"]; ok {
		t.Errorf("mergeOctoBot must not create session/dmScope, got %#v", cfg["session"])
	}
}

func TestMergeOctoBotPreservesOthersAndUpserts(t *testing.T) {
	cfg := map[string]any{
		"channels": map[string]any{"octo": map[string]any{"accounts": map[string]any{
			"other-bot": map[string]any{"botToken": "x", "name": "other"},
		}}},
		"bindings": []any{
			map[string]any{"agentId": "other", "type": "route",
				"match": map[string]any{"channel": "octo", "accountId": "other-bot"}},
			map[string]any{"agentId": "stale", "type": "route",
				"match": map[string]any{"channel": "octo", "accountId": "bot-abc"}},
		},
		"session":  map[string]any{"dmScope": "some-preexisting-value"},
		"topLevel": "keep-me",
	}

	cfg = mergeOctoBot(cfg, "ws-1", "bot-abc", "bf_tok", "https://api.x")

	accs := cfg["channels"].(map[string]any)["octo"].(map[string]any)["accounts"].(map[string]any)
	if _, ok := accs["other-bot"]; !ok {
		t.Error("other-bot account dropped")
	}
	if cfg["topLevel"] != "keep-me" {
		t.Error("topLevel field dropped")
	}
	if cfg["session"].(map[string]any)["dmScope"] != "some-preexisting-value" {
		t.Error("existing dmScope altered")
	}
	binds := cfg["bindings"].([]any)
	count, updated := 0, false
	for _, raw := range binds {
		b := raw.(map[string]any)
		m := b["match"].(map[string]any)
		if m["channel"] == "octo" && m["accountId"] == "bot-abc" {
			count++
			if b["agentId"] == "ws-1" && b["type"] == "route" {
				updated = true
			}
		}
	}
	if count != 1 || !updated {
		t.Errorf("upsert wrong: count=%d updated=%v binds=%#v", count, updated, binds)
	}
}
