package adapter

import (
	"context"
	"fmt"
)

// KindCodex is the runtime_kind discriminator for the Codex CLI runtime.
const KindCodex = "codex"

const codexBin = "codex"

var _ RuntimeAdapter = (*CodexAdapter)(nil)

// CodexAdapter is a sidecar-process adapter (same lifecycle model as
// ClaudeAdapter): one persistent process per bot, RunTask communicates with the
// existing sidecar instead of forking a CLI per task.
//
// SKELETON: Detect/Health are wired; Provision/Deprovision/RunTask are TODO and
// currently return ErrUnsupported. This is 吕思佳's implementation surface.
type CodexAdapter struct {
	runner CLIRunner
}

// NewCodexAdapter builds a CodexAdapter. A nil runner defaults to ExecRunner.
func NewCodexAdapter(runner CLIRunner) *CodexAdapter {
	if runner == nil {
		runner = ExecRunner{}
	}
	return &CodexAdapter{runner: runner}
}

func (a *CodexAdapter) Kind() string                        { return KindCodex }
func (a *CodexAdapter) SupportedConfigVersions() []int      { return []int{1} }
func (a *CodexAdapter) MaxConcurrency() int                 { return 1 }
func (a *CodexAdapter) ValidateConfig(map[string]any) error { return nil }

// Detect probes for the codex binary and its version.
func (a *CodexAdapter) Detect(ctx context.Context) (RuntimeInfo, error) {
	return detectViaVersion(ctx, a.runner, codexBin, KindCodex)
}

// Enrich is a no-op until per-bot sidecar discovery lands.
func (a *CodexAdapter) Enrich(_ context.Context, info RuntimeInfo) (RuntimeInfo, error) {
	return info, nil
}

// Health treats a successful Detect as healthy. Per-bot sidecar health is TODO.
func (a *CodexAdapter) Health(ctx context.Context) error {
	if _, err := a.Detect(ctx); err != nil {
		return fmt.Errorf("%w: %v", ErrUnhealthy, err)
	}
	return nil
}

// Provision spawns/repairs the per-bot sidecar service. TODO.
func (a *CodexAdapter) Provision(_ context.Context, _ ProvisionRequest) (ProvisionResult, error) {
	return ProvisionResult{}, fmt.Errorf("codex provision: %w", ErrUnsupported)
}

// Deprovision tears down the per-bot sidecar service. TODO.
func (a *CodexAdapter) Deprovision(_ context.Context, _ string) error {
	return fmt.Errorf("codex deprovision: %w", ErrUnsupported)
}

// RunTask dispatches a prompt to the bot's running sidecar. TODO.
func (a *CodexAdapter) RunTask(_ context.Context, _ RunTaskRequest) (RunTaskResult, error) {
	return RunTaskResult{}, fmt.Errorf("codex run task: %w", ErrUnsupported)
}
