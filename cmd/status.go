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

	if profiles, err := internal.LoadProfiles(internal.ConfigFilePath()); err == nil && len(profiles) > 0 {
		fmt.Println("Profiles:")
		for _, p := range profiles {
			id, err := internal.LoadDaemonID(p.SpaceID)
			if err != nil {
				id = "(no daemon.id)"
			}
			fmt.Printf("  - space=%s daemon_id=%s\n", p.SpaceID, id)
		}
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
