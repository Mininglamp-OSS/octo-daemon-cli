package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// KindClaude is the runtime_kind discriminator for the Claude Code runtime.
const KindClaude = "claude"

const claudeBin = "claude"

// claudeChannelBin is the host CLI that serves the provisioned bots; it is
// restarted after each provision to pick up the updated config.
const claudeChannelBin = "cc-channel-octo"

// claudeChannelDir is the per-host root (under $HOME) holding the shared
// config.json plus one config subdirectory per provisioned bot.
const claudeChannelDir = ".cc-channel-octo"

// claudeModel is the SDK model written into every bot's config.json.
const claudeModel = "vertexai/claude-opus-4-8"

// claudeRestartTimeout caps the `cc-channel-octo restart` subprocess.
const claudeRestartTimeout = 60 * time.Second

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

// Provision lays down one bot's on-disk config: ~/.cc-channel-octo/<bot_uid>/
// config.json carrying the bot's real token and the SDK model, then registers
// the bot in the shared ~/.cc-channel-octo/config.json bots list and restarts
// the channel host so the new bot takes effect. Spawning the per-bot sidecar
// service is still TODO.
func (a *ClaudeAdapter) Provision(ctx context.Context, req ProvisionRequest) (ProvisionResult, error) {
	if req.BotUID == "" {
		return ProvisionResult{}, fmt.Errorf("%w: missing bot_uid", ErrInvalidConfig)
	}
	if req.BotToken == "" {
		return ProvisionResult{}, fmt.Errorf("%w: missing bot_token", ErrInvalidConfig)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("resolve home: %w", err)
	}
	botDir := filepath.Join(home, claudeChannelDir, req.BotUID)
	if err := os.MkdirAll(botDir, 0o755); err != nil {
		return ProvisionResult{}, fmt.Errorf("mkdir %s: %w", botDir, err)
	}
	cfg := map[string]any{
		"botToken": req.BotToken,
		"sdk":      map[string]any{"model": claudeModel},
	}
	buf, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("marshal config: %w", err)
	}
	cfgPath := filepath.Join(botDir, "config.json")
	if err := os.WriteFile(cfgPath, append(buf, '\n'), 0o600); err != nil {
		return ProvisionResult{}, fmt.Errorf("write %s: %w", cfgPath, err)
	}
	log.Printf("[DEBUG] [claude] wrote bot config: %s", cfgPath)

	if err := registerClaudeBot(home, req.BotUID); err != nil {
		return ProvisionResult{}, err
	}

	// Best-effort: the config is the durable state; a failed restart only means
	// the running host hasn't picked it up yet, so log and continue rather than
	// failing the whole provision.
	if err := a.restart(ctx); err != nil {
		log.Printf("[WARN] [claude] cc-channel-octo restart failed: %v", err)
	}

	return ProvisionResult{WorkspaceID: req.WorkspaceID}, nil
}

// registerClaudeBot upserts {"id": botUID} into the bots array of the shared
// ~/.cc-channel-octo/config.json, preserving any other fields. A missing file
// is created. Re-provisioning the same bot is a no-op (idempotent).
func registerClaudeBot(home, botUID string) error {
	cfgPath := filepath.Join(home, claudeChannelDir, "config.json")
	root := map[string]any{}
	buf, err := os.ReadFile(cfgPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", cfgPath, err)
	}
	if err == nil {
		if err := json.Unmarshal(buf, &root); err != nil {
			return fmt.Errorf("parse %s: %w", cfgPath, err)
		}
	}
	bots, _ := root["bots"].([]any)
	for _, b := range bots {
		if m, ok := b.(map[string]any); ok {
			if id, _ := m["id"].(string); id == botUID {
				return nil
			}
		}
	}
	root["bots"] = append(bots, map[string]any{"id": botUID})
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", cfgPath, err)
	}
	if err := os.WriteFile(cfgPath, append(out, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", cfgPath, err)
	}
	log.Printf("[DEBUG] [claude] registered bot %s in %s", botUID, cfgPath)
	return nil
}

func (a *ClaudeAdapter) restart(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, claudeRestartTimeout)
	defer cancel()
	log.Printf("[DEBUG] [claude] exec: %s restart", claudeChannelBin)
	out, err := a.runner.Run(cctx, claudeChannelBin, []string{"restart"}, nil)
	if err != nil {
		return fmt.Errorf("cc-channel-octo restart: %w (output: %s)", err, truncate(string(out), 800))
	}
	return nil
}

// Deprovision tears down the per-bot sidecar service. TODO.
func (a *ClaudeAdapter) Deprovision(_ context.Context, _ string) error {
	return fmt.Errorf("claude deprovision: %w", ErrUnsupported)
}

// RunTask is a reserved placeholder. The claude runtime is not implemented yet;
// it returns ErrUnsupported so a task routed here fails cleanly. Will be
// implemented in a follow-up — ignore in code review until then.
func (a *ClaudeAdapter) RunTask(_ context.Context, _ RunTaskRequest) (RunTaskResult, error) {
	return RunTaskResult{}, fmt.Errorf("claude run task: %w", ErrUnsupported)
}
