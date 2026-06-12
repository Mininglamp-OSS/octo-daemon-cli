package adapter

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// KindHermes is the runtime_kind discriminator for the Hermes runtime.
const KindHermes = "hermes"

const hermesBin = "hermes"

// hermesMaxConcurrency mirrors openclaw: hermes is plugin-in-host, so a single
// host process serves many bots and tasks run concurrently against it.
const hermesMaxConcurrency = 5

// hermesRestartTimeout caps the `hermes gateway restart` subprocess.
const hermesRestartTimeout = 60 * time.Second

var _ RuntimeAdapter = (*HermesAdapter)(nil)

// HermesAdapter is a plugin-in-host adapter (same shape as OpenclawAdapter):
// one host process serves many bots; Provision mutates host config + reload,
// RunTask talks to the same host.
//
// Provision is implemented (edits ~/.hermes/.env + restarts the gateway).
// Deprovision/RunTask are TODO and currently return ErrUnsupported.
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

// Provision points the hermes host at one bot by upserting OCTO_API_URL and
// OCTO_BOT_TOKEN in ~/.hermes/.env, then restarts the gateway so the new values
// take effect. Existing keys are updated in place; missing keys are appended.
func (a *HermesAdapter) Provision(ctx context.Context, req ProvisionRequest) (ProvisionResult, error) {
	log.Printf("[DEBUG] [hermes] provision request: %s", debugDumpProvision(req))
	if req.APIURL == "" || req.BotToken == "" {
		return ProvisionResult{}, fmt.Errorf("%w: missing api_url/bot_token", ErrInvalidConfig)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("resolve home: %w", err)
	}
	envPath := filepath.Join(home, ".hermes", ".env")
	if err := upsertHermesEnv(envPath, [][2]string{
		{"OCTO_API_URL", req.APIURL},
		{"OCTO_BOT_TOKEN", req.BotToken},
	}); err != nil {
		return ProvisionResult{}, fmt.Errorf("hermes env update: %w", err)
	}
	if err := a.restartGateway(ctx); err != nil {
		return ProvisionResult{}, err
	}
	return ProvisionResult{WorkspaceID: req.WorkspaceID}, nil
}

// upsertHermesEnv writes the given key/value pairs into a dotenv file: an
// existing `KEY=...` line is replaced; an absent key is appended. The parent
// directory and file are created if missing. Pairs are applied in order so the
// output is deterministic.
func upsertHermesEnv(path string, pairs [][2]string) error {
	var lines []string
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err == nil {
		lines = strings.Split(string(data), "\n")
		// Drop the trailing empty element produced by a final newline so we
		// don't accumulate blank lines on every rewrite.
		if n := len(lines); n > 0 && lines[n-1] == "" {
			lines = lines[:n-1]
		}
	}
	for _, kv := range pairs {
		lines = setEnvLine(lines, kv[0], kv[1])
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	out := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// setEnvLine replaces the first `key=...` line in place, or appends `key=value`
// when the key is absent.
func setEnvLine(lines []string, key, value string) []string {
	prefix := key + "="
	newLine := key + "=" + value
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), prefix) {
			lines[i] = newLine
			return lines
		}
	}
	return append(lines, newLine)
}

func (a *HermesAdapter) restartGateway(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, hermesRestartTimeout)
	defer cancel()
	args := []string{"gateway", "restart"}
	log.Printf("[DEBUG] [hermes] exec: hermes %s", strings.Join(args, " "))
	out, err := a.runner.Run(cctx, hermesBin, args, nil)
	if err != nil {
		return fmt.Errorf("hermes gateway restart: %w (output: %s)", err, truncate(string(out), 800))
	}
	return nil
}

// Deprovision removes one bot's host config. TODO.
func (a *HermesAdapter) Deprovision(_ context.Context, _ string) error {
	return fmt.Errorf("hermes deprovision: %w", ErrUnsupported)
}

// RunTask is a reserved placeholder. The hermes runtime is not implemented yet;
// it returns ErrUnsupported so a task routed here fails cleanly. Will be
// implemented in a follow-up — ignore in code review until then.
func (a *HermesAdapter) RunTask(_ context.Context, _ RunTaskRequest) (RunTaskResult, error) {
	return RunTaskResult{}, fmt.Errorf("hermes run task: %w", ErrUnsupported)
}
