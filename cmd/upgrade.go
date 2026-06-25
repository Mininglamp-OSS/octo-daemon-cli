package cmd

import (
	"errors"
	"fmt"

	"github.com/Mininglamp-OSS/octo-daemon-cli/internal"
	"github.com/spf13/cobra"
)

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade the daemon to the latest npm release and stop it for the supervisor to restart",
	Long:  "Runs `npm install -g " + internal.DaemonNpmPackage + "@latest` to replace the on-disk\nbinary, then stops the running daemon (`octo-daemon stop`) so your process\nsupervisor (pm2 / systemd / supervisord / k8s) re-execs the new version.",
	RunE:  runUpgrade,
}

func runUpgrade(cmd *cobra.Command, args []string) error {
	fmt.Printf("Upgrading %s...\n", internal.DaemonNpmPackage)
	if err := internal.InstallDaemonNpm(cmd.Context(), "latest"); err != nil {
		if errors.Is(err, internal.ErrNpmNotFound) {
			return &internal.ExitError{Code: 2, Message: "npm not found — install Node.js (which provides npm) to upgrade"}
		}
		return &internal.ExitError{Code: 1, Message: fmt.Sprintf("npm install failed: %v", err)}
	}

	// The new binary is on disk, but the running daemon is still the old one.
	// Stop it (same logic as `octo-daemon stop`: read the pidfile, signal the
	// process) and let the supervisor re-exec the new binary. We deliberately
	// don't restart it ourselves — that keeps upgrade supervisor-agnostic
	// instead of hard-wiring pm2.
	if !internal.IsLocked() {
		fmt.Println("Upgrade complete. Daemon is not running — start it to use the new version.")
		return nil
	}
	if err := runStop(cmd, args); err != nil {
		return &internal.ExitError{Code: 1, Message: fmt.Sprintf("upgrade installed but stopping the daemon failed: %v — restart it manually", err)}
	}
	fmt.Println("Upgrade complete — daemon stopped; your process supervisor will restart it on the new binary.")
	return nil
}
