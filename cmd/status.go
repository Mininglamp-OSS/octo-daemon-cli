package cmd

import (
	"fmt"

	"github.com/Mininglamp-OSS/octo-daemon-cli/internal"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show daemon status",
	RunE:  runStatus,
}

func runStatus(cmd *cobra.Command, args []string) error {
	if !internal.IsLocked() {
		fmt.Println("Status: stopped")
		return nil
	}

	pid, err := internal.ReadLockPID()
	if err != nil {
		fmt.Println("Status: running (pid unknown)")
	} else {
		fmt.Printf("Status: running (pid %d)\n", pid)
	}

	daemonID, err := internal.LoadDaemonID()
	if err == nil {
		fmt.Printf("Daemon ID: %s\n", daemonID)
	}

	runtimes := internal.DetectRuntimes()
	if len(runtimes) == 0 {
		fmt.Println("Runtimes: none detected")
	} else {
		fmt.Println("Runtimes:")
		for _, r := range runtimes {
			fmt.Printf("  - %s %s (%s)\n", r.Provider, r.Version, r.Path)
		}
	}

	return nil
}
