package cmd

import (
	"fmt"
	"os"

	"github.com/Mininglamp-OSS/octo-daemon-cli/internal"
	"github.com/spf13/cobra"
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running daemon",
	RunE:  runStop,
}

func runStop(cmd *cobra.Command, args []string) error {
	return stopRunningDaemon()
}

// stopRunningDaemon signals the running daemon (pid read from the lock file) to
// shut down gracefully. Shared by the `stop` and `upgrade` commands so the
// latter doesn't have to call the former's RunE handler inline. Returns an error
// if no daemon is running or the signal can't be delivered.
func stopRunningDaemon() error {
	if !internal.IsLocked() {
		return fmt.Errorf("daemon is not running")
	}

	pid, err := internal.ReadLockPID()
	if err != nil {
		return fmt.Errorf("cannot read daemon pid: %w", err)
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}

	// os.Interrupt works on both Unix (SIGINT) and Windows
	if err := proc.Signal(os.Interrupt); err != nil {
		if killErr := proc.Kill(); killErr != nil {
			return fmt.Errorf("kill process %d: %w", pid, killErr)
		}
	}

	fmt.Printf("Daemon (pid %d) stopped.\n", pid)
	return nil
}
