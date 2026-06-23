package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

	if len(runner.calls) != 1 || runner.calls[0][0] != claudeChannelBin || runner.calls[0][1] != "restart" {
		t.Errorf("restart not invoked, calls = %v", runner.calls)
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

func TestClaudeProvisionSwallowsRestartFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a := NewClaudeAdapter(&recordingRunner{err: errors.New("boom")})

	if _, err := a.Provision(context.Background(), ProvisionRequest{
		BotUID: "bot-1", BotToken: "bf_x",
	}); err != nil {
		t.Fatalf("Provision should swallow restart failure, got %v", err)
	}
	if ids := readBotIDs(t, home); len(ids) != 1 || ids[0] != "bot-1" {
		t.Errorf("bots = %v, want [bot-1]", ids)
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
