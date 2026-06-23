package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/Mininglamp-OSS/octo-daemon-cli/internal"
	"github.com/spf13/cobra"
)

const npmPackage = "@mininglamp-oss/octo-daemon"

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade the daemon to the latest npm release and restart it under pm2",
	Long:  "Runs `npm install -g " + npmPackage + "@latest` to replace the on-disk binary,\nthen `pm2 restart " + pm2AppName + "` so pm2 re-execs the new version.",
	RunE:  runUpgrade,
}

func runUpgrade(cmd *cobra.Command, args []string) error {
	npm, err := exec.LookPath("npm")
	if err != nil {
		return &internal.ExitError{Code: 2, Message: "npm not found — install Node.js (which provides npm) to upgrade"}
	}

	fmt.Printf("Upgrading %s...\n", npmPackage)
	install := exec.Command(npm, "install", "-g", npmPackage+"@latest")
	install.Stdout = os.Stdout
	install.Stderr = os.Stderr
	if err := install.Run(); err != nil {
		return &internal.ExitError{Code: 1, Message: fmt.Sprintf("npm install failed: %v", err)}
	}

	pm2, err := exec.LookPath("pm2")
	if err != nil {
		return &internal.ExitError{Code: 2, Message: "pm2 not found — run `octo-daemon start --daemon` to set up pm2 supervision first"}
	}
	if err := runPM2(pm2, "restart", pm2AppName); err != nil {
		return &internal.ExitError{Code: 1, Message: fmt.Sprintf("pm2 restart failed: %v — is the daemon registered? run `octo-daemon start --daemon`", err)}
	}

	fmt.Println("Upgrade complete — pm2 restarted the daemon on the new binary.")
	return nil
}
