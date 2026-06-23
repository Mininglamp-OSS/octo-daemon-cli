package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Mininglamp-OSS/octo-daemon-cli/internal"
	"github.com/spf13/cobra"
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the daemon",
	Long:  "Start detecting local agent runtimes and reporting to Octo server.\n\nReads profiles from the config file (default ~/.octo-daemon/config.json) and\nsupervises one backend connection per space. Configure profiles first with\n`octo-daemon config`.\n\nRuns in the foreground (pm2 / a service manager keeps it alive). Use\n`octo-daemon start --daemon` to register it as a pm2-managed service.",
	RunE:  runStart,
}

var (
	flagConfigFile string
	flagDaemon     bool
)

func init() {
	// --config is optional; the pm2 bootstrapper bakes an absolute path into
	// ecosystem.config.js. Interactive/k8s runs use the default
	// ~/.octo-daemon/config.json.
	startCmd.Flags().StringVar(&flagConfigFile, "config", "", "Config file path (default ~/.octo-daemon/config.json)")
	startCmd.Flags().BoolVar(&flagDaemon, "daemon", false, "Bootstrap pm2 to supervise the daemon (installs pm2, writes ecosystem.config.js, then exits)")
}

func runStart(cmd *cobra.Command, args []string) error {
	// Bootstrapper role: --daemon installs/registers the daemon under pm2 and
	// exits. The long-running server is the foreground path below, which pm2
	// (re-)execs from the generated ecosystem.config.js.
	if flagDaemon {
		return bootstrapPM2()
	}

	cfgPath := flagConfigFile
	if cfgPath == "" {
		cfgPath = internal.ConfigFilePath()
	}

	// A pre-multi-profile single-object config can't run under the new binary;
	// move it aside so this becomes a clean "no config" → "run config" error
	// instead of a silent zero-profile start.
	if backup, err := internal.BackupLegacyConfig(cfgPath); err != nil {
		return &internal.ExitError{Code: 2, Message: fmt.Sprintf("back up legacy config: %v", err)}
	} else if backup != "" {
		fmt.Printf("legacy config moved to %s — run `octo-daemon config --server-url=... --api-key=... [--fleet-url=...]` to reconfigure\n", backup)
	}

	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		return &internal.ExitError{Code: 2, Message: fmt.Sprintf("no config at %s — run `octo-daemon config --server-url=... --api-key=... [--fleet-url=...]` first", cfgPath)}
	}

	profiles, err := internal.LoadProfiles(cfgPath)
	if err != nil {
		return &internal.ExitError{Code: 2, Message: fmt.Sprintf("load config: %v", err)}
	}
	if len(profiles) == 0 {
		return &internal.ExitError{Code: 2, Message: "no profiles configured — run `octo-daemon config --server-url=... --api-key=... [--fleet-url=...]` first"}
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
