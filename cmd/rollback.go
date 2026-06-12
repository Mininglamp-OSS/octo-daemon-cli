package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Mininglamp-OSS/octo-daemon-cli/internal"
	"github.com/spf13/cobra"
)

var rollbackCmd = &cobra.Command{
	Use:   "rollback",
	Short: "Rollback to previous version",
	Long:  "Restore the previous daemon binary from backup after a failed upgrade.",
	RunE:  runRollback,
}

func init() {
	rootCmd.AddCommand(rollbackCmd)
}

func runRollback(cmd *cobra.Command, args []string) error {
	bakPath := filepath.Join(internal.DataDir(), "octo-daemon.bak")

	if _, err := os.Stat(bakPath); os.IsNotExist(err) {
		return fmt.Errorf("no backup found at %s", bakPath)
	}

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine executable path: %w", err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return fmt.Errorf("cannot resolve symlinks: %w", err)
	}

	// 先复制到目标同目录的临时文件，再原子 rename
	tmpPath := exePath + ".rollback-tmp"
	bakFile, err := os.Open(bakPath)
	if err != nil {
		return fmt.Errorf("open backup: %w", err)
	}
	defer func() { _ = bakFile.Close() }()

	tmpFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	if _, err := io.Copy(tmpFile, bakFile); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("copy backup: %w", err)
	}
	_ = tmpFile.Close()

	if err := os.Rename(tmpPath, exePath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename failed: %w", err)
	}

	fmt.Printf("Rolled back to previous version.\nBackup: %s → %s\nPlease restart the daemon.\n", bakPath, exePath)
	return nil
}
