package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/Mininglamp-OSS/octo-daemon-cli/internal"
	"github.com/spf13/cobra"
)

// daemonWorkerEnv marks the re-exec'd background worker so it runs in the
// foreground instead of forking again. It is intentionally an env var, not a
// flag, so it never appears in `--help`.
const daemonWorkerEnv = "OCTO_DAEMON_WORKER"

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the daemon",
	Long:  "Start detecting local agent runtimes and reporting to Octo server.\n\nReads profiles from the config file (default ~/.octo-daemon/config.json) and\nsupervises one backend connection per space. Configure profiles first with\n`octo-daemon config`.",
	RunE:  runStart,
}

var (
	flagConfigFile string
	flagDaemon     bool
)

func init() {
	// --config is optional; it exists mainly so `service install` can bake an
	// absolute path into the launchd/systemd unit. Interactive/k8s runs use the
	// default ~/.octo-daemon/config.json.
	startCmd.Flags().StringVar(&flagConfigFile, "config", "", "Config file path (default ~/.octo-daemon/config.json)")
	startCmd.Flags().BoolVar(&flagDaemon, "daemon", false, "Run in the background (detached); logs to ~/.octo-daemon/daemon.log")
}

func runStart(cmd *cobra.Command, args []string) error {
	// Launcher role: --daemon set and we are not already the worker → re-exec
	// self detached, then exit. The worker (same binary with the env marker)
	// falls through to the foreground path below.
	if flagDaemon && os.Getenv(daemonWorkerEnv) == "" {
		return daemonize()
	}

	cfgPath := flagConfigFile
	if cfgPath == "" {
		cfgPath = internal.ConfigFilePath()
	}

	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		return &internal.ExitError{Code: 2, Message: fmt.Sprintf("no config at %s — run `octo-daemon config --space-id=... --server-url=... --fleet-url=... --api-key=...` first", cfgPath)}
	}

	profiles, err := internal.LoadProfiles(cfgPath)
	if err != nil {
		return &internal.ExitError{Code: 2, Message: fmt.Sprintf("load config: %v", err)}
	}
	if len(profiles) == 0 {
		return &internal.ExitError{Code: 2, Message: "no profiles configured — run `octo-daemon config --space-id=... --server-url=... --fleet-url=... --api-key=...` first"}
	}

	hostname, err := os.Hostname()
	if err != nil {
		return &internal.ExitError{Code: 2, Message: fmt.Sprintf("get hostname: %v", err)}
	}
	for i := range profiles {
		if profiles[i].DeviceName == "" {
			profiles[i].DeviceName = hostname
		}
		profiles[i].CLIVersion = version
	}

	sup, err := internal.NewSupervisor(profiles)
	if err != nil {
		return &internal.ExitError{Code: 2, Message: fmt.Sprintf("init supervisor: %v", err)}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		errCh <- sup.Run(ctx)
	}()

	select {
	case sig := <-sigCh:
		fmt.Printf("\nReceived %s, shutting down...\n", sig)
		cancel()
		return <-errCh
	case err := <-errCh:
		return err
	}
}

// daemonize re-execs the current binary as a detached background worker (new
// session, stdio redirected to a log file) and returns once the child is
// started. The single-instance lock is enforced by the worker itself; we
// pre-check here only to give a clean error instead of a silently-dead child.
func daemonize() error {
	if internal.IsLocked() {
		pid, _ := internal.ReadLockPID()
		return &internal.ExitError{Code: 2, Message: fmt.Sprintf("daemon already running (pid %d) — run `octo-daemon stop` first", pid)}
	}

	exe, err := os.Executable()
	if err != nil {
		return &internal.ExitError{Code: 2, Message: fmt.Sprintf("resolve executable: %v", err)}
	}

	logPath := filepath.Join(internal.DataDir(), "daemon.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return &internal.ExitError{Code: 2, Message: fmt.Sprintf("create data dir: %v", err)}
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return &internal.ExitError{Code: 2, Message: fmt.Sprintf("open log %s: %v", logPath, err)}
	}
	defer func() { _ = logFile.Close() }()

	// Re-exec with the same args (incl. --daemon, which the worker ignores via
	// the env marker), detached into its own session so it survives the shell.
	child := exec.Command(exe, os.Args[1:]...)
	child.Stdin = nil
	child.Stdout = logFile
	child.Stderr = logFile
	child.Env = append(os.Environ(), daemonWorkerEnv+"=1")
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := child.Start(); err != nil {
		return &internal.ExitError{Code: 2, Message: fmt.Sprintf("start background daemon: %v", err)}
	}
	pid := child.Process.Pid
	_ = child.Process.Release()

	fmt.Printf("octo-daemon started in background (pid %d). Logs: %s\n", pid, logPath)
	return nil
}
