package cmd

import (
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "octo-daemon",
	Short: "Octo Agent Runtime Daemon",
	Long:  "Detects local AI agent runtimes (Claude Code, OpenClaw, Hermes, Codex) and reports status to Octo server.",
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(statusCmd)
}
