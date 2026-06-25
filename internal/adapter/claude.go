package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// claudeGlobalConfigMu serializes read-modify-write of the shared
// ~/.cc-channel-octo/config.json. atomicWriteFile (temp+rename) prevents a
// reader from seeing a half-written file, but two concurrent Provisions
// registering different bots would still lost-update each other (both read the
// same base, each appends only its own bot, the later rename wins). This mutex
// makes the read→append→write sequence atomic across goroutines in this process.
var claudeGlobalConfigMu sync.Mutex

// KindClaude is the runtime_kind discriminator for the Claude Code runtime.
const KindClaude = "claude"

const claudeBin = "claude"

// claudeChannelDir is the per-host root (under $HOME) holding the shared
// config.json plus one config subdirectory per provisioned bot.
const claudeChannelDir = ".cc-channel-octo"

var _ RuntimeAdapter = (*ClaudeAdapter)(nil)

// ClaudeAdapter is a sidecar-process adapter: each bot is backed by its own
// long-lived process (managed via launchd/systemd), so Provision spawns/repairs
// that service and RunTask talks to the already-running sidecar rather than
// forking a fresh CLI per task.
//
// Detect/Health/Provision are implemented and live; Deprovision/RunTask still
// return ErrUnsupported. This is 吕思佳's implementation surface.
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
// config.json carrying the bot's real token (and its server url), then registers
// the bot in the shared ~/.cc-channel-octo/config.json bots list. Both writes
// are atomic (temp+rename); the cc-channel-octo gateway watches config.json and
// hot-loads the newly-registered bot itself, so Provision does NOT restart the
// gateway (#157). The model is NOT written here — it is gateway-level global
// config. Spawning the per-bot sidecar service is still TODO.
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
	if err := os.MkdirAll(botDir, 0o700); err != nil {
		return ProvisionResult{}, fmt.Errorf("mkdir %s: %w", botDir, err)
	}
	cfg := map[string]any{
		"botToken": req.BotToken,
	}
	// Model is intentionally NOT written here. It is a gateway-level attribute
	// (same for every bot on this runtime), set once in the global config at
	// install time (cc-channel-octo configure --model). Writing it per-bot would
	// override the global value and pin one gateway's model name onto all bots.
	// Pin this bot's server in its own per-bot config (mirrors openclaw's
	// accounts.<uid>.apiUrl). Without it cc-channel-octo falls back to the
	// shared global config.apiUrl, which can be stale/wrong-env and cause a
	// cross-env 401 — the exact failure that took the whole gateway offline.
	// Omit when empty so the shared fallback still applies.
	if req.APIURL != "" {
		cfg["apiUrl"] = req.APIURL
	}
	buf, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("marshal config: %w", err)
	}
	cfgPath := filepath.Join(botDir, "config.json")
	if err := atomicWriteFile(cfgPath, append(buf, '\n')); err != nil {
		return ProvisionResult{}, fmt.Errorf("write %s: %w", cfgPath, err)
	}
	log.Printf("[DEBUG] [claude] wrote bot config: %s", cfgPath)

	if err := registerClaudeBot(home, req.BotUID); err != nil {
		return ProvisionResult{}, err
	}

	// #157: no gateway restart. The cc-channel-octo gateway watches the global
	// config.json and hot-loads the newly-registered bot on its own, so adding a
	// bot no longer interrupts the bots already running. Both writes above are
	// atomic (temp+rename), so the watcher only ever observes a complete file.
	// Known limitation: re-provisioning an EXISTING bot with a changed
	// token/apiUrl rewrites only the per-bot config — the global bots[] is
	// unchanged, so the watcher sees no diff and the change does not take effect
	// until a manual `cc-channel-octo restart`. (Per-bot config hot-reload is a
	// follow-up; see cc-channel-octo #157.)

	return ProvisionResult{WorkspaceID: req.WorkspaceID}, nil
}

// registerClaudeBot upserts {"id": botUID} into the bots array of the shared
// ~/.cc-channel-octo/config.json, preserving any other fields. A missing file
// is created. Re-provisioning the same bot is a no-op (idempotent).
func registerClaudeBot(home, botUID string) error {
	// Serialize the whole read-modify-write against concurrent Provisions so two
	// bots registering at once don't lost-update the shared bots[] array.
	claudeGlobalConfigMu.Lock()
	defer claudeGlobalConfigMu.Unlock()
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
	if err := atomicWriteFile(cfgPath, append(out, '\n')); err != nil {
		return fmt.Errorf("write %s: %w", cfgPath, err)
	}
	log.Printf("[DEBUG] [claude] registered bot %s in %s", botUID, cfgPath)
	return nil
}

// atomicWriteFile writes data to path via a unique temp file + rename, so a
// concurrent reader (the cc-channel-octo gateway's config watcher, #157) never
// observes a half-written file. Preserves the existing file's mode (new files
// default to 0600). The temp file is created in the same directory so the
// rename is atomic on POSIX.
func atomicWriteFile(path string, data []byte) error {
	mode := os.FileMode(0o600)
	if fi, statErr := os.Stat(path); statErr == nil {
		mode = fi.Mode().Perm()
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".cc-octo-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp file to %s: %w", path, err)
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
