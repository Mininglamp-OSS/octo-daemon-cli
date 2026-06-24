package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// KindOpenclaw is the runtime_kind discriminator for openclaw.
const KindOpenclaw = "openclaw"

var _ RuntimeAdapter = (*OpenclawAdapter)(nil)

const (
	openclawBin = "openclaw"

	// openclawMaxConcurrency caps parallel tasks against the single shared
	// openclaw host process (spec §10.4).
	openclawMaxConcurrency = 5

	// openclawAddTimeout bounds `openclaw agents add`. The account+binding
	// write is bounded separately by openclawConfigTimeout (openclaw_config.go).
	openclawAddTimeout = 60 * time.Second

	// openclawTaskTimeout is the daemon's hard ceiling for one agent run when
	// RunTaskRequest.Timeout is unset.
	openclawTaskTimeout = 10 * time.Minute
)

// OpenclawAdapter is the plugin-in-host adapter: one openclaw host process
// serves many bots, each a workspace/agent inside it. Provision changes host
// config and reloads; RunTask talks to the same host.
type OpenclawAdapter struct {
	runner CLIRunner
}

// NewOpenclawAdapter builds an OpenclawAdapter. A nil runner defaults to
// ExecRunner (real subprocesses).
func NewOpenclawAdapter(runner CLIRunner) *OpenclawAdapter {
	if runner == nil {
		runner = ExecRunner{}
	}
	return &OpenclawAdapter{runner: runner}
}

func (a *OpenclawAdapter) Kind() string                        { return KindOpenclaw }
func (a *OpenclawAdapter) SupportedConfigVersions() []int      { return []int{1} }
func (a *OpenclawAdapter) MaxConcurrency() int                 { return openclawMaxConcurrency }
func (a *OpenclawAdapter) ValidateConfig(map[string]any) error { return nil }

// Detect probes for the openclaw binary and its version.
func (a *OpenclawAdapter) Detect(ctx context.Context) (RuntimeInfo, error) {
	return detectViaVersion(ctx, a.runner, openclawBin, KindOpenclaw)
}

// Enrich is a no-op for openclaw at this stage.
func (a *OpenclawAdapter) Enrich(_ context.Context, info RuntimeInfo) (RuntimeInfo, error) {
	return info, nil
}

// Health treats a successful Detect as healthy.
func (a *OpenclawAdapter) Health(ctx context.Context) error {
	if _, err := a.Detect(ctx); err != nil {
		return fmt.Errorf("%w: %v", ErrUnhealthy, err)
	}
	return nil
}

// Provision runs the openclaw side-effects for one bot:
//  1. openclaw agents add <workspace> --non-interactive --workspace <dir>
//     (creates the agent workspace dir + agents.* entry)
//  2. writeOctoConfig: atomically merge channels.octo.accounts.<bot_uid> and the
//     routing binding into openclaw.json in a single write, so the gateway
//     hot-reloads the new route without a full restart.
func (a *OpenclawAdapter) Provision(ctx context.Context, req ProvisionRequest) (ProvisionResult, error) {
	log.Printf("[DEBUG] [openclaw] provision request: %s", debugDumpProvision(req))
	if req.WorkspaceID == "" || req.BotUID == "" {
		return ProvisionResult{}, fmt.Errorf("%w: missing workspace_id/bot_uid", ErrInvalidConfig)
	}
	// Hold openclawConfigMu across the whole openclaw mutation sequence. Both
	// `agents add` (a subprocess that writes agents.* into openclaw.json) and the
	// account+binding rewrite touch the same file; serializing the pair prevents a
	// concurrent provision's `agents add` from interleaving between our read and
	// rename and clobbering the freshly-written agents entry.
	openclawConfigMu.Lock()
	defer openclawConfigMu.Unlock()
	if err := a.addWorkspace(ctx, req); err != nil {
		return ProvisionResult{}, err
	}
	// Write account + routing binding in a single atomic config mutation so the
	// gateway hot-reloads the new route. Writing the binding separately is a
	// noop reload in openclaw and never takes effect without a full restart;
	// folding both into one write is what lets us avoid restarting the gateway
	// (and dropping every other bot's session) on each new bot.
	if err := writeOctoConfig(ctx, a.runner, req); err != nil {
		return ProvisionResult{}, err
	}
	return ProvisionResult{WorkspaceID: req.WorkspaceID}, nil
}

func (a *OpenclawAdapter) addWorkspace(ctx context.Context, req ProvisionRequest) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home: %w", err)
	}
	workspace := filepath.Join(home, ".openclaw", "workspaces", req.WorkspaceID)
	cctx, cancel := context.WithTimeout(ctx, openclawAddTimeout)
	defer cancel()
	args := []string{
		"agents", "add", req.WorkspaceID,
		"--non-interactive",
		"--workspace", workspace,
	}
	log.Printf("[DEBUG] [openclaw] exec: openclaw %s", strings.Join(args, " "))
	out, err := a.runner.Run(cctx, openclawBin, args, nil)
	if err != nil {
		if isAlreadyExists(out, err) {
			log.Printf("[DEBUG] [openclaw] agents add: workspace %q already exists, treating as success (replay)", req.WorkspaceID)
			return nil
		}
		return fmt.Errorf("openclaw agents add: %w (output: %s)", err, truncate(string(out), 800))
	}
	return nil
}

// isAlreadyExists reports whether a failed openclaw subprocess failed only
// because the workspace/agent/binding it tried to create already exists. The
// daemon re-runs Provision when a provision ack is lost, so a replay hits this
// against an already-provisioned bot; it must be treated as success, otherwise
// an already-provisioned bot is acked failed and daemon/server state drifts.
func isAlreadyExists(out []byte, err error) bool {
	hay := strings.ToLower(string(out))
	if err != nil {
		hay += " " + strings.ToLower(err.Error())
	}
	return strings.Contains(hay, "already exists") ||
		strings.Contains(hay, "already registered") ||
		strings.Contains(hay, "already bound")
}

// Deprovision is a no-op for openclaw at this PoC stage (operator cleans up).
func (a *OpenclawAdapter) Deprovision(_ context.Context, _ string) error {
	return nil
}

// debugDumpProvision marshals a provision request to JSON for [DEBUG] logging,
// with the bot token masked so secrets never reach the logs.
func debugDumpProvision(req ProvisionRequest) string {
	redacted := req
	if redacted.BotToken != "" {
		redacted.BotToken = "***redacted***"
	}
	b, err := json.Marshal(redacted)
	if err != nil {
		return fmt.Sprintf("<marshal err: %v>", err)
	}
	return string(b)
}

// RunTask invokes `openclaw agent --agent <workspace> --json -m <prompt>` and
// parses the reply, preserving the original runOpenclawAgent behaviour
// (combined output, raw-stdout fallback). req.Timeout, when >0, overrides the
// default 10-minute ceiling.
func (a *OpenclawAdapter) RunTask(ctx context.Context, req RunTaskRequest) (RunTaskResult, error) {
	if req.WorkspaceID == "" {
		return RunTaskResult{}, fmt.Errorf("%w: missing workspace_id", ErrInvalidConfig)
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return RunTaskResult{}, fmt.Errorf("%w: empty prompt", ErrInvalidConfig)
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = openclawTaskTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	out, err := a.runner.Run(cctx, openclawBin, []string{
		"agent",
		"--agent", req.WorkspaceID,
		"--json",
		"-m", req.Prompt,
	}, nil)
	if err != nil {
		return RunTaskResult{}, fmt.Errorf("openclaw agent: %w (output: %s)", err, truncate(string(out), 800))
	}

	reply := ExtractReplyFromEnvelope(out)
	if strings.TrimSpace(reply) == "" {
		reply = strings.TrimSpace(string(out))
	}
	if reply == "" {
		return RunTaskResult{}, fmt.Errorf("openclaw agent produced no output")
	}
	return RunTaskResult{Reply: reply, ElapsedMs: time.Since(start).Milliseconds()}, nil
}
