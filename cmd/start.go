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
	Long:  "Start detecting local agent runtimes and reporting to Octo server.",
	RunE:  runStart,
}

var (
	flagAPIKey     string
	flagAPIURL     string
	flagDeviceName string
	flagForeground bool
	flagConfigFile string
)

func init() {
	startCmd.Flags().StringVar(&flagAPIKey, "api-key", "", "User API key for authentication")
	startCmd.Flags().StringVar(&flagAPIURL, "api-url", "", "Octo API server URL")
	startCmd.Flags().StringVar(&flagDeviceName, "device-name", "", "Device display name (defaults to hostname)")
	startCmd.Flags().BoolVar(&flagForeground, "foreground", true, "Run in foreground (default: true)")
	startCmd.Flags().StringVar(&flagConfigFile, "config", "", "Config file path (overrides api-key/api-url flags)")
}

func runStart(cmd *cobra.Command, args []string) error {
	var cfg internal.Config

	// 如果有 --config，从文件加载
	if flagConfigFile != "" {
		loaded, err := internal.LoadConfig(flagConfigFile)
		if err != nil {
			return &internal.ExitError{Code: 2, Message: fmt.Sprintf("load config: %v", err)}
		}
		cfg = loaded
	}

	// 命令行参数覆盖配置文件
	if flagAPIKey != "" {
		cfg.APIKey = flagAPIKey
	}
	if flagAPIURL != "" {
		cfg.APIURL = flagAPIURL
	}
	if flagDeviceName != "" {
		cfg.DeviceName = flagDeviceName
	}

	if cfg.APIKey == "" || cfg.APIURL == "" {
		return &internal.ExitError{Code: 2, Message: "api-key and api-url are required (via flags or --config)"}
	}

	if cfg.DeviceName == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return &internal.ExitError{Code: 2, Message: fmt.Sprintf("get hostname: %v", err)}
		}
		cfg.DeviceName = hostname
	}

	cfg.CLIVersion = version

	if err := internal.SaveConfig(cfg); err != nil {
		return &internal.ExitError{Code: 2, Message: fmt.Sprintf("failed to save config (required for remote upgrade): %v", err)}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	d, err := internal.NewDaemon(cfg)
	if err != nil {
		return &internal.ExitError{Code: 2, Message: fmt.Sprintf("init daemon: %v", err)}
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- d.Run(ctx)
	}()

	// Run() 返回 nil 或 ExitError；Signal 触发的正常关停也走 Run() 退出链路。
	select {
	case sig := <-sigCh:
		fmt.Printf("\nReceived %s, shutting down...\n", sig)
		cancel()
		return <-errCh
	case err := <-errCh:
		return err
	}
}
