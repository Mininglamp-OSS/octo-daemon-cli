package cmd

import (
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "octo-daemon",
	Short: "Octo Agent Runtime Daemon",
	Long:  "Native daemon binary commands: configure spaces, run the foreground daemon, inspect status, and upgrade the installed binary.\n\nWhen installed from npm, the Node.js shim adds pm2 service lifecycle commands (`start`, `stop`, `restart`, `logs`, `service ...`) around this binary.",
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(upgradeCmd)
}
