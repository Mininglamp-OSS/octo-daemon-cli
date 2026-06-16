package cmd

import (
	"fmt"
	"os"

	"github.com/Mininglamp-OSS/octo-daemon-cli/internal"
	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Configure a space profile",
	Long:  "Create or update one space profile and initialize its directory.\n\nIdempotent by space_id: creates ~/.octo-daemon/<space_id>/, generates the\nspace's daemon.id, and upserts the profile into ~/.octo-daemon/config.json.\nRun once per space, then `octo-daemon start`.",
	RunE:  runConfig,
}

var (
	flagCfgSpaceID   string
	flagCfgServerURL string
	flagCfgFleetURL  string
	flagCfgMatterURL string
	flagCfgAPIKey    string
)

func init() {
	configCmd.Flags().StringVar(&flagCfgSpaceID, "space-id", "", "Space ID (required; used as the profile key and directory name)")
	configCmd.Flags().StringVar(&flagCfgServerURL, "server-url", "", "Server URL (auth + bot_token endpoints)")
	configCmd.Flags().StringVar(&flagCfgFleetURL, "fleet-url", "", "Fleet URL (runtime/bot endpoints + SSE)")
	configCmd.Flags().StringVar(&flagCfgMatterURL, "matter-url", "", "Matter URL (optional; reserved for future use)")
	configCmd.Flags().StringVar(&flagCfgAPIKey, "api-key", "", "Space-scoped API key (required)")
}

func runConfig(cmd *cobra.Command, args []string) error {
	if err := internal.ValidateSpaceID(flagCfgSpaceID); err != nil {
		return &internal.ExitError{Code: 2, Message: err.Error()}
	}
	if flagCfgAPIKey == "" {
		return &internal.ExitError{Code: 2, Message: "--api-key is required"}
	}
	if flagCfgFleetURL == "" || flagCfgServerURL == "" {
		return &internal.ExitError{Code: 2, Message: "--fleet-url and --server-url are required"}
	}

	// Create the per-space directory and ensure its stable daemon.id.
	if _, err := internal.EnsureDaemonID(flagCfgSpaceID); err != nil {
		return &internal.ExitError{Code: 2, Message: fmt.Sprintf("init space: %v", err)}
	}

	// Load existing profiles (tolerate a missing file as "no profiles yet").
	cfgPath := internal.ConfigFilePath()
	var profiles []internal.Config
	if _, statErr := os.Stat(cfgPath); statErr == nil {
		loaded, err := internal.LoadProfiles(cfgPath)
		if err != nil {
			return &internal.ExitError{Code: 2, Message: fmt.Sprintf("load existing config: %v", err)}
		}
		profiles = loaded
	}

	// Upsert by space_id.
	next := internal.Config{
		SpaceID:   flagCfgSpaceID,
		APIKey:    flagCfgAPIKey,
		ServerURL: flagCfgServerURL,
		FleetURL:  flagCfgFleetURL,
		MatterURL: flagCfgMatterURL,
	}
	replaced := false
	for i := range profiles {
		if profiles[i].SpaceID == flagCfgSpaceID {
			// Preserve any per-profile device_name/heartbeat already set.
			next.DeviceName = profiles[i].DeviceName
			next.HeartbeatInterval = profiles[i].HeartbeatInterval
			profiles[i] = next
			replaced = true
			break
		}
	}
	if !replaced {
		profiles = append(profiles, next)
	}

	if err := internal.SaveProfiles(cfgPath, profiles); err != nil {
		return &internal.ExitError{Code: 2, Message: fmt.Sprintf("save config: %v", err)}
	}

	action := "added"
	if replaced {
		action = "updated"
	}
	fmt.Printf("Profile %s %s. Config: %s\n", flagCfgSpaceID, action, cfgPath)
	return nil
}
