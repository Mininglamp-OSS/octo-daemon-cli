package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/Mininglamp-OSS/octo-daemon-cli/internal"
	"github.com/Mininglamp-OSS/octo-daemon-cli/internal/service"
	"github.com/spf13/cobra"
)

var serviceCmd = &cobra.Command{
	Use:   "service",
	Short: "Manage octo-daemon as a user-level service (launchd / systemd --user)",
	Long:  "Install, uninstall, and inspect the octo-daemon auto-start service. See `service --help`.",
}

var (
	flagServiceInstallForce bool
	flagServiceLogsFollow   bool
	flagServiceLogsLines    int
)

var serviceInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install the octo-daemon service (auto-start + auto-restart)",
	RunE:  runServiceInstall,
}

var serviceUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Uninstall the octo-daemon service",
	RunE:  runServiceUninstall,
}

var serviceStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show octo-daemon service status",
	RunE:  runServiceStatus,
}

var serviceRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the octo-daemon service (kill + start)",
	RunE:  runServiceRestart,
}

var serviceLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Tail the daemon service log",
	RunE:  runServiceLogs,
}

func init() {
	serviceInstallCmd.Flags().BoolVar(&flagServiceInstallForce, "force", false, "Reinstall if already installed (uninstall first)")
	serviceLogsCmd.Flags().BoolVarP(&flagServiceLogsFollow, "follow", "f", false, "Follow the log (tail -f / journalctl -f)")
	serviceLogsCmd.Flags().IntVarP(&flagServiceLogsLines, "lines", "n", 100, "Number of recent lines to show")

	serviceCmd.AddCommand(serviceInstallCmd)
	serviceCmd.AddCommand(serviceUninstallCmd)
	serviceCmd.AddCommand(serviceStatusCmd)
	serviceCmd.AddCommand(serviceRestartCmd)
	serviceCmd.AddCommand(serviceLogsCmd)
	rootCmd.AddCommand(serviceCmd)
}

func runServiceInstall(cmd *cobra.Command, args []string) error {
	svc, err := service.New()
	if err != nil {
		return err
	}

	// Precheck 1: config must exist and have at least one usable profile.
	// We don't just check the file exists — an empty `{"profiles":[]}` would
	// let install succeed, then the daemon exits 2, under-service maps 2→0,
	// launchd treats as success → "installed but never ran" UX. So require a
	// profile with a non-empty api_key and fleet/server URLs.
	cfgPath := internal.ConfigFilePath()
	if _, err := os.Stat(cfgPath); err != nil {
		return fmt.Errorf("daemon config not found at %s — run `octo-daemon config --server-url=... --api-key=... [--fleet-url=...]` first, then retry", cfgPath)
	}
	profiles, err := internal.LoadProfiles(cfgPath)
	if err != nil {
		return fmt.Errorf("daemon config at %s is invalid: %w", cfgPath, err)
	}
	usable := false
	for _, p := range profiles {
		if internal.ValidateSpaceID(p.SpaceID) == nil && p.APIKey != "" && p.FleetURL != "" && p.ServerURL != "" {
			usable = true
			break
		}
	}
	if !usable {
		return fmt.Errorf("daemon config at %s has no usable profile (need a valid space_id + api_key + fleet_url + server_url) — run `octo-daemon config --server-url=... --api-key=... [--fleet-url=...]`, then retry", cfgPath)
	}

	// Precheck 2: refuse double-install without --force.
	status, _ := svc.Status()
	if status.Installed && !flagServiceInstallForce {
		return fmt.Errorf("service already installed (running pid %d) — use --force to reinstall", status.PID)
	}

	// --force path: stop the current service first so its lock is released
	// before the next precheck. Wait up to 3s for the lock to drop.
	if status.Installed && flagServiceInstallForce {
		if err := svc.Uninstall(); err != nil {
			return fmt.Errorf("force-uninstall before reinstall failed: %w", err)
		}
		for i := 0; i < 30 && internal.IsLocked(); i++ {
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Precheck 3: no OTHER daemon (handcrafted `start`) currently running.
	// If we just uninstalled above, the lock should be clear. If a user
	// separately ran `start`, refuse — otherwise the service's first spawn
	// loses the lock race, exits 2, under-service main maps 2→0, launchd
	// treats that as success and never restarts → "installed but dead" UX.
	if internal.IsLocked() {
		pid, _ := internal.ReadLockPID()
		return fmt.Errorf("another daemon is running (pid %d) — run `octo-daemon stop` before installing the service", pid)
	}

	// Resolve absolute exec path (service should launch the specific binary
	// that was invoked, not whatever PATH resolves later).
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return fmt.Errorf("eval symlinks: %w", err)
	}

	// Minimal env needed at spawn. Keep PATH since many agent CLIs live in
	// homebrew/local bins that launchd doesn't inherit. HOME is mandatory.
	home, _ := os.UserHomeDir()
	env := map[string]string{
		"HOME": home,
		"PATH": os.Getenv("PATH"),
	}
	// force is now always false here — we handled the "already installed"
	// case above by uninstalling first.
	if err := svc.Install(service.InstallConfig{
		ExecPath:   exePath,
		ConfigPath: cfgPath,
		Env:        env,
	}, false); err != nil {
		return err
	}

	fmt.Println("Service installed and started.")
	if lp := svc.LogPath(); lp != "" {
		fmt.Printf("Logs: %s\n", lp)
	} else if runtime.GOOS == "linux" {
		fmt.Println("Logs: journalctl --user -u octo-daemon.service [-f]")
	}
	return nil
}

func runServiceUninstall(cmd *cobra.Command, args []string) error {
	svc, err := service.New()
	if err != nil {
		return err
	}
	if err := svc.Uninstall(); err != nil {
		return err
	}
	fmt.Println("Service uninstalled. Config and data preserved.")
	return nil
}

func runServiceStatus(cmd *cobra.Command, args []string) error {
	svc, err := service.New()
	if err != nil {
		return err
	}
	info, err := svc.Status()
	if err != nil {
		return err
	}
	installed := "no"
	if info.Installed {
		installed = "yes"
	}
	fmt.Printf("Installed: %s\n", installed)
	if info.Running {
		fmt.Printf("Running:   yes (pid %d)\n", info.PID)
	} else {
		fmt.Println("Running:   no")
	}
	if lp := svc.LogPath(); lp != "" {
		fmt.Printf("Logs:      %s\n", lp)
	} else if runtime.GOOS == "linux" {
		fmt.Println("Logs:      journalctl --user -u octo-daemon.service [-f]")
	}
	return nil
}

func runServiceRestart(cmd *cobra.Command, args []string) error {
	svc, err := service.New()
	if err != nil {
		return err
	}
	if err := svc.Restart(); err != nil {
		return err
	}
	fmt.Println("Service restart requested.")
	return nil
}

func runServiceLogs(cmd *cobra.Command, args []string) error {
	svc, err := service.New()
	if err != nil {
		return err
	}

	// macOS: tail the file written by launchd's StandardOutPath.
	if lp := svc.LogPath(); lp != "" {
		tailArgs := []string{"-n", fmt.Sprintf("%d", flagServiceLogsLines)}
		if flagServiceLogsFollow {
			tailArgs = append(tailArgs, "-f")
		}
		tailArgs = append(tailArgs, lp)
		c := exec.Command("tail", tailArgs...)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		return c.Run()
	}

	// Linux: journald
	if runtime.GOOS == "linux" {
		jArgs := []string{"--user", "-u", "octo-daemon.service", "-n", fmt.Sprintf("%d", flagServiceLogsLines)}
		if flagServiceLogsFollow {
			jArgs = append(jArgs, "-f")
		}
		c := exec.Command("journalctl", jArgs...)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		return c.Run()
	}

	return fmt.Errorf("no log source for platform %s", runtime.GOOS)
}
