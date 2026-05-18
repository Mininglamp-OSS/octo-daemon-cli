package service

// Service wraps a user-level service manager integration (launchd on macOS,
// systemd --user on Linux). Implementations are constructed via New(),
// defined per-platform in service_<goos>.go.
type Service interface {
	// Install generates config files, registers with the service manager,
	// and starts the service. Idempotent when force=true (uninstall first).
	Install(cfg InstallConfig, force bool) error

	// Uninstall stops the service, unregisters it, and removes generated
	// files. Does not touch config.json, daemon.id, or logs.
	Uninstall() error

	// Status reports whether the service is installed and running.
	Status() (StatusInfo, error)

	// Restart asks the service manager to restart the service (kill + start).
	// Used only by `service restart` CLI, never from the upgrade path.
	Restart() error

	// LogPath returns a path suitable for `tail -f` (macOS) or an empty
	// string when logs go to journald (Linux).
	LogPath() string
}

// InstallConfig captures everything needed to render the plist / unit file.
type InstallConfig struct {
	// ExecPath is the absolute path to the octo-daemon binary that the
	// service manager should spawn. Caller is expected to os.Executable()
	// + filepath.EvalSymlinks.
	ExecPath string

	// ConfigPath is the daemon config.json path. v1 only supports
	// ConfigFilePath() default; the field is kept for future flexibility.
	ConfigPath string

	// Env is merged into the env file written alongside wrapper.sh. The
	// Install flow always injects OctoDaemonUnderServiceEnv=1 even if
	// the caller did not include it.
	Env map[string]string
}

// StatusInfo is what `service status` prints.
type StatusInfo struct {
	Installed bool
	Running   bool
	PID       int
}

// New() is defined per-platform in service_darwin.go / service_linux.go /
// service_other.go so each build only references types that exist.
