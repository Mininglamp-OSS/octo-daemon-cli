package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestMergeAndWriteOctoConfigAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openclaw.json")

	seed := map[string]any{
		"channels": map[string]any{"octo": map[string]any{"accounts": map[string]any{
			"old-bot": map[string]any{"botToken": "x", "name": "old"},
		}}},
		"topLevel": "keep",
	}
	raw, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := mergeAndWriteOctoConfig(path, "ws-1", "bot-abc", "bf_tok", "https://api.x"); err != nil {
		t.Fatalf("mergeAndWriteOctoConfig: %v", err)
	}

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result not valid json: %v", err)
	}
	accs := got["channels"].(map[string]any)["octo"].(map[string]any)["accounts"].(map[string]any)
	if _, ok := accs["old-bot"]; !ok {
		t.Error("old-bot dropped")
	}
	if _, ok := accs["bot-abc"]; !ok {
		t.Error("bot-abc not written")
	}
	if got["topLevel"] != "keep" {
		t.Error("topLevel dropped")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestMergeAndWriteOctoConfigCreatesWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openclaw.json")
	if err := mergeAndWriteOctoConfig(path, "ws-1", "bot-abc", "bf_tok", "https://api.x"); err != nil {
		t.Fatalf("mergeAndWriteOctoConfig: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config not created: %v", err)
	}
}

func TestMergeAndWriteOctoConfigPreservesLargeNumbers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openclaw.json")
	if err := os.WriteFile(path, []byte(`{"bigCounter": 1234567890123456789}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mergeAndWriteOctoConfig(path, "ws-1", "bot-abc", "bf_tok", "https://api.x"); err != nil {
		t.Fatalf("mergeAndWriteOctoConfig: %v", err)
	}
	out, _ := os.ReadFile(path)
	if !strings.Contains(string(out), "1234567890123456789") {
		t.Errorf("large integer not preserved verbatim; got: %s", out)
	}
}

func TestMergeAndWriteOctoConfigConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openclaw.json")

	const n = 5
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("bot-%d", i)
			if err := mergeAndWriteOctoConfig(path, "ws-"+id, id, "bf_"+id, "https://api.x"); err != nil {
				t.Errorf("merge %s: %v", id, err)
			}
		}(i)
	}
	wg.Wait()

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result not valid json: %v", err)
	}
	accs := got["channels"].(map[string]any)["octo"].(map[string]any)["accounts"].(map[string]any)
	for i := 0; i < n; i++ {
		if _, ok := accs[fmt.Sprintf("bot-%d", i)]; !ok {
			t.Errorf("bot-%d lost under concurrency", i)
		}
	}
}

// configFileRunner returns a fixed path for `config file` and records calls.
type configFileRunner struct {
	calls    [][]string
	pathLine string
}

func (r *configFileRunner) Run(_ context.Context, name string, args []string, _ []byte) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if len(args) >= 2 && args[0] == "config" && args[1] == "file" {
		return []byte(r.pathLine + "\n"), nil
	}
	return nil, nil
}

func TestWriteOctoConfigResolvesPathAndWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openclaw.json")
	runner := &configFileRunner{pathLine: path}

	err := writeOctoConfig(context.Background(), runner, ProvisionRequest{
		WorkspaceID: "ws-1", BotUID: "bot-abc", BotToken: "bf_tok", APIURL: "https://api.x",
	})
	if err != nil {
		t.Fatalf("writeOctoConfig: %v", err)
	}
	if len(runner.calls) == 0 || runner.calls[0][1] != "config" || runner.calls[0][2] != "file" {
		t.Errorf("expected `config file` call, got %v", runner.calls)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	accs := got["channels"].(map[string]any)["octo"].(map[string]any)["accounts"].(map[string]any)
	if _, ok := accs["bot-abc"]; !ok {
		t.Error("bot not written")
	}
}

// A config file with trailing content after the first JSON value is corrupt and
// must fail closed, not be silently truncated on rewrite.
func TestMergeAndWriteOctoConfigRejectsTrailingGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openclaw.json")
	if err := os.WriteFile(path, []byte(`{"a":1} garbage`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mergeAndWriteOctoConfig(path, "ws-1", "bot-abc", "bf_tok", "https://api.x"); err == nil {
		t.Error("expected parse error on trailing garbage, got nil")
	}
}
