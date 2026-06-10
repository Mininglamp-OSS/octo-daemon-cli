package adapter

import (
	"context"
	"fmt"
)

// KindHermes is the runtime_kind discriminator for the Hermes runtime.
const KindHermes = "hermes"

const hermesBin = "hermes"

// hermesMaxConcurrency mirrors openclaw: hermes is plugin-in-host, so a single
// host process serves many bots and tasks run concurrently against it.
const hermesMaxConcurrency = 5

var _ RuntimeAdapter = (*HermesAdapter)(nil)

// HermesAdapter is a plugin-in-host adapter (same shape as OpenclawAdapter):
// one host process serves many bots; Provision mutates host config + reload,
// RunTask talks to the same host.
//
// SKELETON: Detect/Health are wired; Provision/Deprovision/RunTask are TODO and
// currently return ErrUnsupported. This is 吕思佳's implementation surface.
type HermesAdapter struct {
	runner CLIRunner
}

// NewHermesAdapter builds a HermesAdapter. A nil runner defaults to ExecRunner.
func NewHermesAdapter(runner CLIRunner) *HermesAdapter {
	if runner == nil {
		runner = ExecRunner{}
	}
	return &HermesAdapter{runner: runner}
}

func (a *HermesAdapter) Kind() string                        { return KindHermes }
func (a *HermesAdapter) SupportedConfigVersions() []int      { return []int{1} }
func (a *HermesAdapter) MaxConcurrency() int                 { return hermesMaxConcurrency }
func (a *HermesAdapter) ValidateConfig(map[string]any) error { return nil }

// Detect probes for the hermes binary and its version.
func (a *HermesAdapter) Detect(ctx context.Context) (RuntimeInfo, error) {
	return detectViaVersion(ctx, a.runner, hermesBin, KindHermes)
}

// Enrich is a no-op for hermes at this stage.
func (a *HermesAdapter) Enrich(_ context.Context, info RuntimeInfo) (RuntimeInfo, error) {
	return info, nil
}

// Health treats a successful Detect as healthy.
func (a *HermesAdapter) Health(ctx context.Context) error {
	if _, err := a.Detect(ctx); err != nil {
		return fmt.Errorf("%w: %v", ErrUnhealthy, err)
	}
	return nil
}

// Provision mutates host config for one bot and reloads. TODO.
func (a *HermesAdapter) Provision(_ context.Context, _ ProvisionRequest) (ProvisionResult, error) {
	return ProvisionResult{}, fmt.Errorf("hermes provision: %w", ErrUnsupported)
}

// Deprovision removes one bot's host config. TODO.
func (a *HermesAdapter) Deprovision(_ context.Context, _ string) error {
	return fmt.Errorf("hermes deprovision: %w", ErrUnsupported)
}

// RunTask dispatches a prompt to the hermes host for one bot. TODO.
func (a *HermesAdapter) RunTask(_ context.Context, _ RunTaskRequest) (RunTaskResult, error) {
	return RunTaskResult{}, fmt.Errorf("hermes run task: %w", ErrUnsupported)
}
