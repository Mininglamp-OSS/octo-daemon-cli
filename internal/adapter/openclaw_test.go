package adapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestOpenclawProvisionRunsAllStepsInOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgPath := filepath.Join(home, "openclaw.json")

	runner := &configFileRunner{pathLine: cfgPath}
	a := NewOpenclawAdapter(runner)
	res, err := a.Provision(context.Background(), ProvisionRequest{
		WorkspaceID: "ws-1",
		BotUID:      "bot-123",
		BotToken:    "bf_secret",
		APIURL:      "https://api.example",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.WorkspaceID != "ws-1" {
		t.Errorf("WorkspaceID = %q, want ws-1", res.WorkspaceID)
	}

	workspace := filepath.Join(home, ".openclaw", "workspaces", "ws-1")
	// No legacy CLI side-effects remain.
	for _, c := range runner.calls {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "config patch") ||
			strings.Contains(joined, "agents bind") ||
			strings.Contains(joined, "gateway restart") {
			t.Errorf("unexpected legacy CLI step: %v", c)
		}
	}
	// Exact CLI sequence: agents add (build workspace) THEN config file (locate
	// openclaw.json for the atomic write). No other openclaw subprocesses.
	want := [][]string{
		{openclawBin, "agents", "add", "ws-1", "--non-interactive", "--workspace", workspace},
		{openclawBin, "config", "file"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Errorf("calls =\n%v\nwant\n%v", runner.calls, want)
	}
	// config landed on disk
	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf("openclaw.json not written: %v", err)
	}
}

// alreadyExistsRunner records calls and, once armed, returns an "already exists"
// error for `agents add` — simulating a replayed Provision against an
// already-provisioned bot after a lost ack. It also answers `config file` so the
// atomic config write can locate openclaw.json.
type alreadyExistsRunner struct {
	calls    [][]string
	armed    bool
	pathLine string
}

func (r *alreadyExistsRunner) Run(_ context.Context, name string, args []string, _ []byte) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if len(args) >= 2 && args[0] == "config" && args[1] == "file" {
		return []byte(r.pathLine + "\n"), nil
	}
	if r.armed && len(args) >= 2 && args[0] == "agents" && args[1] == "add" {
		return []byte("Error: agent already exists"), errors.New("exit status 1")
	}
	return nil, nil
}

// TestOpenclawProvisionToleratesAlreadyExistsOnReplay exercises the ack-failure
// replay path: after a successful provision, a lost ack makes the daemon re-run
// Provision against the now-existing bot, so `agents add` fails with "already
// exists". Provision must treat that as success (the config write is an
// idempotent upsert) rather than ack the bot failed and drift state.
func TestOpenclawProvisionToleratesAlreadyExistsOnReplay(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	runner := &alreadyExistsRunner{pathLine: filepath.Join(home, "openclaw.json")}
	a := NewOpenclawAdapter(runner)
	req := ProvisionRequest{
		WorkspaceID: "ws-1",
		BotUID:      "bot-123",
		BotToken:    "bf_secret",
		APIURL:      "https://api.example",
	}

	if _, err := a.Provision(context.Background(), req); err != nil {
		t.Fatalf("first Provision: %v", err)
	}

	runner.armed = true // bot now exists; replayed `agents add` returns "already exists"
	if _, err := a.Provision(context.Background(), req); err != nil {
		t.Fatalf("replay Provision must tolerate already-exists, got %v", err)
	}
}

// TestOpenclawProvisionPropagatesRealError guards the tolerance from swallowing
// genuine failures: a non-already-exists error must still fail Provision.
func TestOpenclawProvisionPropagatesRealError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := NewOpenclawAdapter(&recordingRunner{err: errors.New("permission denied")})

	if _, err := a.Provision(context.Background(), ProvisionRequest{
		WorkspaceID: "ws-1",
		BotUID:      "bot-1",
		BotToken:    "bf_x",
		APIURL:      "https://api.example",
	}); err == nil {
		t.Fatal("Provision should propagate a non-already-exists error")
	}
}

func TestOpenclawProvisionRejectsMissingFields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := NewOpenclawAdapter(&recordingRunner{})

	tests := []struct {
		name string
		req  ProvisionRequest
	}{
		{"missing workspace_id", ProvisionRequest{BotUID: "bot-1"}},
		{"missing bot_uid", ProvisionRequest{WorkspaceID: "ws-1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := a.Provision(context.Background(), tt.req); !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("err = %v, want ErrInvalidConfig", err)
			}
		})
	}
}
