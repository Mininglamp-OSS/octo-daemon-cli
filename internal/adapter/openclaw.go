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

	// Per-step timeouts, identical to the original direct-exec daemon code.
	openclawAddTimeout     = 60 * time.Second
	openclawPatchTimeout   = 30 * time.Second
	openclawBindTimeout    = 30 * time.Second
	openclawRestartTimeout = 60 * time.Second

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

// Provision runs the three openclaw side-effects for one bot, identical in
// command shape, ordering, timeouts and error wrapping to the original daemon
// handleBotProvision:
//  1. openclaw agents add <workspace> --non-interactive --workspace <dir>
//  2. openclaw config patch --stdin   (channels.octo.accounts.<bot_uid>)
//  3. openclaw agents bind --agent <workspace> --bind octo:<bot_uid>
func (a *OpenclawAdapter) Provision(ctx context.Context, req ProvisionRequest) (ProvisionResult, error) {
	log.Printf("[DEBUG] [openclaw] provision request: %s", debugDumpProvision(req))
	if req.WorkspaceID == "" || req.BotUID == "" {
		return ProvisionResult{}, fmt.Errorf("%w: missing workspace_id/bot_uid", ErrInvalidConfig)
	}
	if err := a.addWorkspace(ctx, req); err != nil {
		return ProvisionResult{}, err
	}
	if err := a.patchOctoAccount(ctx, req); err != nil {
		return ProvisionResult{}, err
	}
	if err := a.bindBot(ctx, req); err != nil {
		return ProvisionResult{}, err
	}
	// TEMP: force a gateway restart so the new account is picked up. The
	// running gateway's hot-reload currently misses accounts because accountId
	// case is mixed (`config patch` writes one casing, the in-memory index
	// keys on another), so the patched config never takes effect without a
	// full restart. Remove this step once the accountId casing is normalized.
	if err := a.restartGateway(ctx); err != nil {
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
		return fmt.Errorf("openclaw agents add: %w (output: %s)", err, truncate(string(out), 800))
	}
	return nil
}

func (a *OpenclawAdapter) patchOctoAccount(ctx context.Context, req ProvisionRequest) error {
	patch := map[string]any{
		"channels": map[string]any{
			"octo": map[string]any{
				"accounts": map[string]any{
					req.BotUID: map[string]any{
						"botToken": req.BotToken,
						"apiUrl":   req.APIURL,
						// name is openclaw's routing key — must equal the agent
						// name (= workspace_id created by `agents add`), NOT the
						// user-facing DisplayName, or octo channel can't route
						// inbound IM to this agent and falls back to main.
						"name": req.WorkspaceID,
						// requireMention gates the agent to only respond when
						// explicitly @-mentioned in group chats.
						"requireMention": true,
					},
				},
			},
		},
	}
	buf, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, openclawPatchTimeout)
	defer cancel()
	dump := string(buf)
	if req.BotToken != "" {
		dump = strings.ReplaceAll(dump, req.BotToken, "***redacted***")
	}
	log.Printf("[DEBUG] [openclaw] exec: openclaw config patch --stdin payload=%s", dump)
	out, err := a.runner.Run(cctx, openclawBin, []string{"config", "patch", "--stdin"}, buf)
	if err != nil {
		return fmt.Errorf("openclaw config patch: %w (output: %s)", err, truncate(string(out), 800))
	}
	return nil
}

func (a *OpenclawAdapter) bindBot(ctx context.Context, req ProvisionRequest) error {
	cctx, cancel := context.WithTimeout(ctx, openclawBindTimeout)
	defer cancel()
	bind := fmt.Sprintf("octo:%s", req.BotUID)
	args := []string{
		"agents", "bind",
		"--agent", req.WorkspaceID,
		"--bind", bind,
	}
	log.Printf("[DEBUG] [openclaw] exec: openclaw %s", strings.Join(args, " "))
	out, err := a.runner.Run(cctx, openclawBin, args, nil)
	if err != nil {
		return fmt.Errorf("openclaw agents bind: %w (output: %s)", err, truncate(string(out), 800))
	}
	return nil
}

// Deprovision is a no-op for openclaw at this PoC stage (operator cleans up).
func (a *OpenclawAdapter) Deprovision(_ context.Context, _ string) error {
	return nil
}

// restartGateway runs `openclaw gateway restart`.
//
// TEMP: only needed to work around the accountId casing bug that breaks
// hot-reload of patched config. Remove together with the call site in
// Provision once that is fixed.
func (a *OpenclawAdapter) restartGateway(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, openclawRestartTimeout)
	defer cancel()
	args := []string{"gateway", "restart"}
	log.Printf("[DEBUG] [openclaw] exec: openclaw %s", strings.Join(args, " "))
	out, err := a.runner.Run(cctx, openclawBin, args, nil)
	if err != nil {
		return fmt.Errorf("openclaw gateway restart: %w (output: %s)", err, truncate(string(out), 800))
	}
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
