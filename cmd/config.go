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
	Long:  "Verify an api_key against fleet and persist the resulting space profile.\n\nThe space_id is resolved from fleet's verify endpoint, not supplied by the\noperator: config POSTs to <fleet-url>/v1/runtimes/verify with the api_key and\nonly persists a profile when verification succeeds. fleet-url defaults to\n<server-url>/fleet/api; pass --fleet-url to override.\n\nIdempotent by the resolved space_id: creates ~/.octo-daemon/<space_id>/,\ngenerates the space's daemon.id, and upserts the profile into\n~/.octo-daemon/config.json. Then run `octo-daemon run` in the foreground, or\n`octo-daemon start` from the npm package to manage the pm2 service.",
	RunE:  runConfig,
}

var (
	flagCfgServerURL string
	flagCfgFleetURL  string
	flagCfgMatterURL string
	flagCfgAPIKey    string
)

func init() {
	configCmd.Flags().StringVar(&flagCfgServerURL, "server-url", "", "Server URL (auth + bot_token endpoints; required)")
	configCmd.Flags().StringVar(&flagCfgFleetURL, "fleet-url", "", "Fleet URL (optional; defaults to <server-url>/fleet/api)")
	configCmd.Flags().StringVar(&flagCfgMatterURL, "matter-url", "", "Matter URL (optional; reserved for future use)")
	configCmd.Flags().StringVar(&flagCfgAPIKey, "api-key", "", "Space-scoped API key (required)")
}

func runConfig(cmd *cobra.Command, args []string) error {
	if flagCfgAPIKey == "" {
		return &internal.ExitError{Code: 2, Message: "--api-key is required"}
	}
	if flagCfgServerURL == "" {
		return &internal.ExitError{Code: 2, Message: "--server-url is required"}
	}

	// fleet-url is optional: explicit value wins, otherwise derive from server-url.
	fleetURL := internal.ResolveFleetURL(flagCfgServerURL, flagCfgFleetURL)

	// Verify the api_key against fleet and learn its bound space_id. No profile
	// is written unless this succeeds — verification is the setup gate.
	client := internal.NewClient(fleetURL, flagCfgAPIKey, version)
	verified, err := client.Verify(cmd.Context())
	if err != nil {
		return &internal.ExitError{Code: 2, Message: fmt.Sprintf("credential verification failed (fleet-url=%s): %v", fleetURL, err)}
	}
	spaceID := verified.SpaceID
	if err := internal.ValidateSpaceID(spaceID); err != nil {
		return &internal.ExitError{Code: 2, Message: fmt.Sprintf("fleet returned an unusable space_id: %v", err)}
	}

	// Create the per-space directory and ensure its stable daemon.id.
	if _, err := internal.EnsureDaemonID(spaceID); err != nil {
		return &internal.ExitError{Code: 2, Message: fmt.Sprintf("init space: %v", err)}
	}

	// Load existing profiles (tolerate a missing file as "no profiles yet").
	cfgPath := internal.ConfigFilePath()
	// Move a pre-multi-profile single-object config aside before writing the
	// new format, preserving the operator's old values instead of overwriting.
	if backup, err := internal.BackupLegacyConfig(cfgPath); err != nil {
		return &internal.ExitError{Code: 2, Message: fmt.Sprintf("back up legacy config: %v", err)}
	} else if backup != "" {
		fmt.Printf("legacy config moved to %s\n", backup)
	}
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
		SpaceID:   spaceID,
		APIKey:    flagCfgAPIKey,
		ServerURL: flagCfgServerURL,
		FleetURL:  fleetURL,
		MatterURL: flagCfgMatterURL,
	}
	replaced := false
	for i := range profiles {
		if profiles[i].SpaceID == spaceID {
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

	if err := internal.SaveProfiles(cfgPath, profiles, version); err != nil {
		return &internal.ExitError{Code: 2, Message: fmt.Sprintf("save config: %v", err)}
	}

	action := "added"
	if replaced {
		action = "updated"
	}
	fmt.Printf("Profile %s %s. Config: %s\n", spaceID, action, cfgPath)
	return nil
}
