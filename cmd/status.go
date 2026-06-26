package cmd

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/Mininglamp-OSS/octo-daemon-cli/internal"
	"github.com/spf13/cobra"
)

var statusJSON bool

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show daemon status",
	RunE:  runStatus,
}

type statusReport struct {
	Status       string `json:"status"`
	Locked       bool   `json:"locked"`
	PID          int    `json:"pid"`
	PIDFileStale bool   `json:"pid_file_stale"`
}

func init() {
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Print machine-readable daemon lock status")
}

func runStatus(cmd *cobra.Command, args []string) error {
	return writeStatus(cmd.OutOrStdout(), statusJSON)
}

func currentStatusReport() statusReport {
	locked := internal.IsLocked()
	pid := 0
	pidFileStale := false
	if p, err := internal.ReadLockPID(); err == nil && p > 0 {
		pid = p
		pidFileStale = !locked
	}
	status := "stopped"
	if locked {
		status = "running"
		pidFileStale = false
	}
	return statusReport{
		Status:       status,
		Locked:       locked,
		PID:          pid,
		PIDFileStale: pidFileStale,
	}
}

func writeStatus(w io.Writer, asJSON bool) error {
	report := currentStatusReport()
	if asJSON {
		return json.NewEncoder(w).Encode(report)
	}

	if !report.Locked {
		_, err := fmt.Fprintln(w, "Status: stopped")
		return err
	}

	if report.PID == 0 {
		if _, err := fmt.Fprintln(w, "Status: running (pid unknown)"); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(w, "Status: running (pid %d)\n", report.PID); err != nil {
			return err
		}
	}

	if profiles, err := internal.LoadProfiles(internal.ConfigFilePath()); err == nil && len(profiles) > 0 {
		if _, err := fmt.Fprintln(w, "Profiles:"); err != nil {
			return err
		}
		for _, p := range profiles {
			id, err := internal.LoadDaemonID(p.SpaceID)
			if err != nil {
				id = "(no daemon.id)"
			}
			if _, err := fmt.Fprintf(w, "  - space=%s daemon_id=%s\n", p.SpaceID, id); err != nil {
				return err
			}
		}
	}

	runtimes := internal.DetectRuntimes()
	if len(runtimes) == 0 {
		if _, err := fmt.Fprintln(w, "Runtimes: none detected"); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintln(w, "Runtimes:"); err != nil {
			return err
		}
		for _, r := range runtimes {
			if _, err := fmt.Fprintf(w, "  - %s %s (%s)\n", r.Provider, r.Version, r.Path); err != nil {
				return err
			}
		}
	}

	return nil
}
