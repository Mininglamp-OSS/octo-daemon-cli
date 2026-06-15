// Package adapter defines the RuntimeAdapter abstraction that lets the daemon
// drive multiple agent runtimes (openclaw, claude) through one
// interface for the two daemon-owned lifecycles: bot provision/deprovision and
// matter task execution. IM private/group chat is each runtime's own concern
// and is intentionally absent from this interface.
//
// This package is self-contained — it must not import package internal (which
// imports it), so it defines its own data types and reply parsing rather than
// reusing the ones in the parent package.
package adapter

import (
	"context"
	"time"
)

// RuntimeAdapter is the contract every runtime implementation satisfies. A
// concrete adapter is a struct value (it may hold per-runtime state such as a
// workspace_id → sidecar-process map), registered once in a Registry keyed by
// Kind().
type RuntimeAdapter interface {
	// Kind returns the runtime_kind discriminator ("openclaw", "claude", ...).
	Kind() string

	// SupportedConfigVersions lists the RuntimeConfig schema versions this
	// adapter understands.
	SupportedConfigVersions() []int

	// MaxConcurrency caps how many tasks the daemon should run against this
	// runtime in parallel.
	MaxConcurrency() int

	// Detect probes the local machine for the runtime binary and basic info.
	Detect(ctx context.Context) (RuntimeInfo, error)

	// Enrich augments a RuntimeInfo with the more expensive details (agent
	// list, plugin list, gateway status). May be a no-op.
	Enrich(ctx context.Context, info RuntimeInfo) (RuntimeInfo, error)

	// Health reports whether the runtime is currently usable.
	Health(ctx context.Context) error

	// ValidateConfig checks a RuntimeConfig payload before it is acted on.
	ValidateConfig(cfg map[string]any) error

	// Provision creates/repairs the local resources for one bot. It must be
	// idempotent: a re-delivered command (lost ack) must not double-create.
	Provision(ctx context.Context, req ProvisionRequest) (ProvisionResult, error)

	// Deprovision tears down the local resources for one bot/workspace.
	Deprovision(ctx context.Context, workspaceID string) error

	// RunTask executes one matter prompt against an already-provisioned bot
	// and returns the agent's reply.
	RunTask(ctx context.Context, req RunTaskRequest) (RunTaskResult, error)
}

// RuntimeInfo is the adapter-local view of a detected runtime. It is distinct
// from the parent package's detect.RuntimeInfo on purpose (different ownership,
// no import cycle).
type RuntimeInfo struct {
	Provider  string         `json:"provider"`
	Version   string         `json:"version"`
	BinPath   string         `json:"bin_path"`
	Available bool           `json:"available"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// ProvisionRequest carries everything an adapter needs to stand up one bot.
// Token fetch, api_url resolution, enrich and ack are daemon responsibilities
// and happen outside the adapter.
type ProvisionRequest struct {
	WorkspaceID          string
	BotUID               string
	BotToken             string
	APIURL               string
	DisplayName          string
	RuntimeConfigVersion int
	RuntimeConfig        map[string]any
	TraceID              string
}

// ProvisionResult reports runtime-specific outcome metadata back to the daemon.
type ProvisionResult struct {
	WorkspaceID string
	Metadata    map[string]any
}

// RunTaskRequest is one matter task dispatched to a provisioned bot.
type RunTaskRequest struct {
	WorkspaceID          string
	BotUID               string
	Prompt               string
	TaskID               string
	MatterID             string
	RuntimeConfigVersion int
	RuntimeConfig        map[string]any
	TraceID              string
	Timeout              time.Duration
}

// RunTaskResult is the agent's reply plus timing/metadata.
type RunTaskResult struct {
	Reply     string
	ElapsedMs int64
	Metadata  map[string]any
}
