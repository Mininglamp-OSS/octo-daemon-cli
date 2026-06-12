package adapter

import "errors"

// Sentinel errors adapters return so the daemon can classify failures without
// string matching. Wrap them with fmt.Errorf("...: %w", err) to add context.
var (
	// ErrNotInstalled — the runtime binary is not present on this machine.
	ErrNotInstalled = errors.New("runtime not installed")

	// ErrUnsupported — the requested operation is not supported by this runtime.
	ErrUnsupported = errors.New("operation not supported")

	// ErrUnsupportedVersion — the RuntimeConfig version is outside
	// SupportedConfigVersions.
	ErrUnsupportedVersion = errors.New("unsupported config version")

	// ErrInvalidConfig — the RuntimeConfig payload failed validation.
	ErrInvalidConfig = errors.New("invalid runtime config")

	// ErrUnhealthy — the runtime is installed but not currently usable.
	ErrUnhealthy = errors.New("runtime unhealthy")
)
