package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// recordingRunner captures CLIRunner calls and returns a canned result so
// Provision tests don't spawn the real cc-channel-octo binary.
type recordingRunner struct {
	calls [][]string
	err   error
}

func (r *recordingRunner) Run(_ context.Context, name string, args []string, _ []byte) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	return nil, r.err
}

func readBotIDs(t *testing.T, home string) []string {
	t.Helper()
	buf, err := os.ReadFile(filepath.Join(home, claudeChannelDir, "config.json"))
	if err != nil {
		t.Fatalf("read shared config.json: %v", err)
	}
	var root struct {
		Bots []struct {
			ID string `json:"id"`
		} `json:"bots"`
	}
	if err := json.Unmarshal(buf, &root); err != nil {
		t.Fatalf("unmarshal shared config.json: %v", err)
	}
	ids := make([]string, len(root.Bots))
	for i, b := range root.Bots {
		ids[i] = b.ID
	}
	return ids
}

func TestClaudeProvisionWritesConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	runner := &recordingRunner{}
	a := NewClaudeAdapter(runner)
	res, err := a.Provision(context.Background(), ProvisionRequest{
		WorkspaceID: "ws-1",
		BotUID:      "bot-123",
		BotToken:    "bf_secret",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.WorkspaceID != "ws-1" {
		t.Errorf("WorkspaceID = %q, want ws-1", res.WorkspaceID)
	}

	cfgPath := filepath.Join(home, claudeChannelDir, "bot-123", "config.json")
	buf, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var got struct {
		BotToken string `json:"botToken"`
		SDK      struct {
			Model string `json:"model"`
		} `json:"sdk"`
	}
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatalf("unmarshal config.json: %v", err)
	}
	if got.BotToken != "bf_secret" {
		t.Errorf("botToken = %q, want bf_secret", got.BotToken)
	}
	// Model is gateway-level global config now — Provision must NOT pin it per-bot.
	if got.SDK.Model != "" {
		t.Errorf("sdk.model must not be written per-bot, got %q", got.SDK.Model)
	}

	if ids := readBotIDs(t, home); len(ids) != 1 || ids[0] != "bot-123" {
		t.Errorf("bots = %v, want [bot-123]", ids)
	}

	// #157: provision no longer restarts the gateway — it only writes config and
	// the gateway hot-loads the bot via its config watcher. No CLI subcommand
	// should be invoked.
	if len(runner.calls) != 0 {
		t.Errorf("provision must not invoke any CLI command (#157 hot-reload), got %v", runner.calls)
	}
}

func TestClaudeProvisionRegistersBotsIdempotently(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a := NewClaudeAdapter(&recordingRunner{})

	for _, uid := range []string{"bot-a", "bot-b", "bot-a"} {
		if _, err := a.Provision(context.Background(), ProvisionRequest{
			BotUID: uid, BotToken: "bf_x",
		}); err != nil {
			t.Fatalf("Provision %s: %v", uid, err)
		}
	}

	ids := readBotIDs(t, home)
	if len(ids) != 2 || ids[0] != "bot-a" || ids[1] != "bot-b" {
		t.Errorf("bots = %v, want [bot-a bot-b]", ids)
	}
}

// #157: concurrent Provisions of different bots must all survive in the shared
// bots[] — the global config mutex prevents a lost update where each goroutine
// reads the same base and the last rename drops the others' entries.
func TestClaudeProvisionConcurrentNoLostUpdate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a := NewClaudeAdapter(&recordingRunner{})

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			uid := fmt.Sprintf("bot-%02d", i)
			if _, err := a.Provision(context.Background(), ProvisionRequest{
				BotUID: uid, BotToken: "bf_x",
			}); err != nil {
				t.Errorf("Provision %s: %v", uid, err)
			}
		}(i)
	}
	wg.Wait()

	ids := readBotIDs(t, home)
	got := make(map[string]bool, len(ids))
	for _, id := range ids {
		got[id] = true
	}
	if len(ids) != n || len(got) != n {
		t.Fatalf("expected %d distinct bots, got %d (ids=%v)", n, len(ids), ids)
	}
	for i := 0; i < n; i++ {
		uid := fmt.Sprintf("bot-%02d", i)
		if !got[uid] {
			t.Errorf("bot %s lost from shared config (lost update)", uid)
		}
	}
}

// #157: provision succeeds purely by writing config (no gateway subprocess), so
// a CLIRunner that would fail on any exec must not affect the result — provision
// never calls it.
func TestClaudeProvisionDoesNotInvokeGateway(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	runner := &recordingRunner{err: errors.New("boom")}
	a := NewClaudeAdapter(runner)

	if _, err := a.Provision(context.Background(), ProvisionRequest{
		BotUID: "bot-1", BotToken: "bf_x",
	}); err != nil {
		t.Fatalf("Provision must not depend on any subprocess, got %v", err)
	}
	if ids := readBotIDs(t, home); len(ids) != 1 || ids[0] != "bot-1" {
		t.Errorf("bots = %v, want [bot-1]", ids)
	}
	if len(runner.calls) != 0 {
		t.Errorf("provision must not invoke any CLI command, got %v", runner.calls)
	}
}

func TestClaudeProvisionRejectsMissingFields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := NewClaudeAdapter(&recordingRunner{})

	tests := []struct {
		name string
		req  ProvisionRequest
	}{
		{"missing bot_uid", ProvisionRequest{BotToken: "bf_x"}},
		{"missing bot_token", ProvisionRequest{BotUID: "bot-1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := a.Provision(context.Background(), tt.req); !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("err = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

// readBotConfig reads a per-bot config.json into a generic map for field checks.
func readBotConfig(t *testing.T, home, botUID string) map[string]any {
	t.Helper()
	buf, err := os.ReadFile(filepath.Join(home, claudeChannelDir, botUID, "config.json"))
	if err != nil {
		t.Fatalf("read per-bot config.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(buf, &m); err != nil {
		t.Fatalf("unmarshal per-bot config.json: %v", err)
	}
	return m
}

func TestClaudeProvisionWritesApiUrl(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a := NewClaudeAdapter(&recordingRunner{})
	if _, err := a.Provision(context.Background(), ProvisionRequest{
		BotUID:   "bot-x",
		BotToken: "bf_x",
		APIURL:   "https://test.example.com",
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	cfg := readBotConfig(t, home, "bot-x")
	if cfg["apiUrl"] != "https://test.example.com" {
		t.Errorf("apiUrl = %v, want https://test.example.com", cfg["apiUrl"])
	}
}

func TestClaudeProvisionOmitsEmptyApiUrl(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a := NewClaudeAdapter(&recordingRunner{})
	if _, err := a.Provision(context.Background(), ProvisionRequest{
		BotUID:   "bot-y",
		BotToken: "bf_y",
		// APIURL empty → cc-channel-octo falls back to the shared global apiUrl;
		// don't write a blank key.
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	cfg := readBotConfig(t, home, "bot-y")
	if _, present := cfg["apiUrl"]; present {
		t.Errorf("apiUrl should be omitted when empty, got %v", cfg["apiUrl"])
	}
}

// #157: the config watcher reads config.json concurrently with the daemon's
// writes, so writes must be atomic (temp+rename) — a reader never sees a
// partial file, and the final content is exactly what was written.
func TestAtomicWriteFileWritesCompleteContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	payload := []byte(`{"bots":[{"id":"a"},{"id":"b"}]}` + "\n")
	if err := atomicWriteFile(path, payload); err != nil {
		t.Fatalf("atomicWriteFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("content = %q, want %q", got, payload)
	}
	// No leftover temp files in the directory (rename consumed it).
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "config.json" {
			t.Errorf("unexpected leftover file: %s", e.Name())
		}
	}
}

func TestAtomicWriteFileOverwritesAndPreservesMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// Seed an existing file with a non-default mode. os.WriteFile's mode is
	// masked by the process umask, so Chmod explicitly afterwards to pin the
	// real on-disk mode regardless of the umask the test runs under.
	if err := os.WriteFile(path, []byte("old"), 0o640); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatalf("chmod seed: %v", err)
	}
	if err := atomicWriteFile(path, []byte("new")); err != nil {
		t.Fatalf("atomicWriteFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("content = %q, want new", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640 (preserved)", fi.Mode().Perm())
	}
}
