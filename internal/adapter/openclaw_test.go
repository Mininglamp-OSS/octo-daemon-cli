package adapter

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestOpenclawProvisionRunsAllStepsInOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	runner := &recordingRunner{}
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
	want := [][]string{
		{openclawBin, "agents", "add", "ws-1", "--non-interactive", "--workspace", workspace},
		{openclawBin, "config", "patch", "--stdin"},
		{openclawBin, "agents", "bind", "--agent", "ws-1", "--bind", "octo:bot-123"},
		{openclawBin, "gateway", "restart"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Errorf("calls =\n%v\nwant\n%v", runner.calls, want)
	}
}

// TestOpenclawProvisionReplayIsIdempotent pins the ack-failure replay contract.
// When a provision ack is lost, the fleet re-delivers the identical command and
// Provision runs a second time. The openclaw CLI treats a repeated `agents add`
// / `agents bind` as a no-op on existing state (verified manually 2026-06-12),
// so the daemon delegates idempotency to the CLI: a replay must re-run the full
// step sequence and succeed, never short-circuiting to a local "already
// provisioned -> failed" decision. This test fails if anyone introduces such a
// decision or changes the command shape between runs.
func TestOpenclawProvisionReplayIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	runner := &recordingRunner{}
	a := NewOpenclawAdapter(runner)
	req := ProvisionRequest{
		WorkspaceID: "ws-1",
		BotUID:      "bot-123",
		BotToken:    "bf_secret",
		APIURL:      "https://api.example",
	}

	for i := 1; i <= 2; i++ {
		if _, err := a.Provision(context.Background(), req); err != nil {
			t.Fatalf("Provision run %d: %v", i, err)
		}
	}

	const stepsPerRun = 4
	if len(runner.calls) != 2*stepsPerRun {
		t.Fatalf("calls = %d, want %d (replay must re-run all steps)", len(runner.calls), 2*stepsPerRun)
	}
	if first, second := runner.calls[:stepsPerRun], runner.calls[stepsPerRun:]; !reflect.DeepEqual(first, second) {
		t.Errorf("replay diverged:\nrun1=%v\nrun2=%v", first, second)
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
