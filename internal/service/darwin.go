//go:build darwin

package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-daemon-cli/internal"
)

type launchdService struct{}

// userTarget returns "gui/<uid>" for launchctl bootstrap / bootout.
func (*launchdService) userTarget() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("get current user: %w", err)
	}
	return "gui/" + u.Uid, nil
}

// serviceTarget returns "gui/<uid>/<label>" for launchctl print / kickstart.
func (s *launchdService) serviceTarget() (string, error) {
	t, err := s.userTarget()
	if err != nil {
		return "", err
	}
	return t + "/" + ServiceLabel, nil
}

func (s *launchdService) Install(cfg InstallConfig, force bool) error {
	plistPath, err := PlistPath()
	if err != nil {
		return err
	}

	// Force mode: try to uninstall first (ignore errors — state may be partial).
	if force {
		_ = s.Uninstall()
	} else if _, statErr := os.Stat(plistPath); statErr == nil {
		return fmt.Errorf("service already installed at %s (use --force to reinstall)", plistPath)
	}

	// Materialize directories.
	for _, d := range []string{ServiceEnvDir(), LogDir(), filepath.Dir(plistPath)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	// Write wrapper.sh (rw-r--r-- + exec bit).
	if err := os.WriteFile(WrapperScriptPath(), []byte(WrapperScriptContent()), 0o755); err != nil {
		return fmt.Errorf("write wrapper: %w", err)
	}

	// Build env: caller's map + mandatory UNDER_SERVICE flag.
	env := map[string]string{}
	for k, v := range cfg.Env {
		env[k] = v
	}
	env[OctoDaemonUnderServiceEnv] = "1"
	envBody, err := RenderEnvFile(env)
	if err != nil {
		return fmt.Errorf("render env: %w", err)
	}
	if err := os.WriteFile(EnvFilePath(), []byte(envBody), 0o600); err != nil {
		return fmt.Errorf("write env: %w", err)
	}

	// Render and write plist.
	plistBody := RenderPlist(PlistRenderArgs{
		WrapperScript: WrapperScriptPath(),
		EnvFile:       EnvFilePath(),
		ExecPath:      cfg.ExecPath,
		ConfigPath:    cfg.ConfigPath,
		DataDir:       internal.DataDir(),
		StdoutLog:     StdoutLogPath(),
		StderrLog:     StderrLogPath(),
	})
	if err := os.WriteFile(plistPath, []byte(plistBody), 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}

	// Bootstrap into launchd.
	target, err := s.userTarget()
	if err != nil {
		return err
	}
	if out, err := runLaunchctl("bootstrap", target, plistPath); err != nil {
		return fmt.Errorf("launchctl bootstrap failed: %w\n%s", err, out)
	}
	return nil
}

func (s *launchdService) Uninstall() error {
	plistPath, err := PlistPath()
	if err != nil {
		return err
	}
	target, err := s.userTarget()
	if err != nil {
		return err
	}
	// bootout: best-effort — if service wasn't bootstrapped, ignore.
	_, _ = runLaunchctl("bootout", target+"/"+ServiceLabel)

	// Remove generated files. Keep daemon.id, config.json, logs/ so user data
	// survives uninstall → reinstall cycle.
	for _, p := range []string{plistPath, WrapperScriptPath(), EnvFilePath()} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	// Prune service-env dir if empty.
	_ = os.Remove(ServiceEnvDir())
	return nil
}

func (s *launchdService) Status() (StatusInfo, error) {
	plistPath, err := PlistPath()
	if err != nil {
		return StatusInfo{}, err
	}
	info := StatusInfo{}
	if _, err := os.Stat(plistPath); err == nil {
		info.Installed = true
	}
	target, err := s.serviceTarget()
	if err != nil {
		return info, err
	}
	out, err := runLaunchctl("print", target)
	if err != nil {
		// print fails when the service is not bootstrapped at all. Installed
		// is already set from the plist stat check; Running stays false.
		return info, nil
	}
	// `launchctl print` succeeds for both "loaded + running" and "loaded +
	// exited". PID line is only present when actually running. Treat
	// "no pid line → PID 0" as Running=false, not "running but pid unknown".
	pid := parseLaunchctlPrintPID(out)
	if pid > 0 {
		info.Running = true
		info.PID = pid
	}
	return info, nil
}

func (s *launchdService) Restart() error {
	target, err := s.serviceTarget()
	if err != nil {
		return err
	}
	if out, err := runLaunchctl("kickstart", "-k", target); err != nil {
		return fmt.Errorf("launchctl kickstart failed: %w\n%s", err, out)
	}
	return nil
}

func (*launchdService) LogPath() string { return StdoutLogPath() }

// runLaunchctl is a thin wrapper to keep error messages uniform.
func runLaunchctl(args ...string) (string, error) {
	cmd := exec.Command("launchctl", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// parseLaunchctlPrintPID pulls "pid = <N>" out of `launchctl print` output.
// Returns 0 if not present (e.g. service is loaded but not currently running).
func parseLaunchctlPrintPID(out string) int {
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		// Format: "pid = 12345"
		if strings.HasPrefix(trimmed, "pid") {
			if i := strings.Index(trimmed, "="); i >= 0 {
				numStr := strings.TrimSpace(trimmed[i+1:])
				if n, err := strconv.Atoi(numStr); err == nil {
					return n
				}
			}
		}
	}
	return 0
}
