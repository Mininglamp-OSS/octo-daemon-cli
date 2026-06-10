package adapter

import (
	"context"
	"fmt"
)

// KindClaude is the runtime_kind discriminator for the Claude Code runtime.
const KindClaude = "claude"

const claudeBin = "claude"

var _ RuntimeAdapter = (*ClaudeAdapter)(nil)

// ClaudeAdapter is a sidecar-process adapter: each bot is backed by its own
// long-lived process (managed via launchd/systemd), so Provision spawns/repairs
// that service and RunTask talks to the already-running sidecar rather than
// forking a fresh CLI per task.
//
// SKELETON: Detect/Health are wired; Provision/Deprovision/RunTask are TODO and
// currently return ErrUnsupported. This is 吕思佳's implementation surface.
type ClaudeAdapter struct {
	runner CLIRunner
}

// NewClaudeAdapter builds a ClaudeAdapter. A nil runner defaults to ExecRunner.
func NewClaudeAdapter(runner CLIRunner) *ClaudeAdapter {
	if runner == nil {
		runner = ExecRunner{}
	}
	return &ClaudeAdapter{runner: runner}
}

func (a *ClaudeAdapter) Kind() string                        { return KindClaude }
func (a *ClaudeAdapter) SupportedConfigVersions() []int      { return []int{1} }
func (a *ClaudeAdapter) MaxConcurrency() int                 { return 1 }
func (a *ClaudeAdapter) ValidateConfig(map[string]any) error { return nil }

// Detect probes for the claude binary and its version.
func (a *ClaudeAdapter) Detect(ctx context.Context) (RuntimeInfo, error) {
	return detectViaVersion(ctx, a.runner, claudeBin, KindClaude)
}

// Enrich is a no-op until per-bot sidecar discovery lands.
func (a *ClaudeAdapter) Enrich(_ context.Context, info RuntimeInfo) (RuntimeInfo, error) {
	return info, nil
}

// Health treats a successful Detect as healthy. Per-bot sidecar health is TODO.
func (a *ClaudeAdapter) Health(ctx context.Context) error {
	if _, err := a.Detect(ctx); err != nil {
		return fmt.Errorf("%w: %v", ErrUnhealthy, err)
	}
	return nil
}

// Provision spawns/repairs the per-bot sidecar service. TODO.
func (a *ClaudeAdapter) Provision(_ context.Context, _ ProvisionRequest) (ProvisionResult, error) {
	return ProvisionResult{}, fmt.Errorf("claude provision: %w", ErrUnsupported)
}

// Deprovision tears down the per-bot sidecar service. TODO.
func (a *ClaudeAdapter) Deprovision(_ context.Context, _ string) error {
	return fmt.Errorf("claude deprovision: %w", ErrUnsupported)
}

// RunTask dispatches a prompt to the bot's running sidecar. TODO.
func (a *ClaudeAdapter) RunTask(_ context.Context, _ RunTaskRequest) (RunTaskResult, error) {
	return RunTaskResult{}, fmt.Errorf("claude run task: %w", ErrUnsupported)
}
