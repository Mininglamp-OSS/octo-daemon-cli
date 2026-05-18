// Package service installs octo-daemon as a user-level service (macOS launchd /
// Linux systemd --user). See plan file §三 and the Service interface in service.go.
package service

import (
	"os"
	"path/filepath"

	"github.com/Mininglamp-OSS/octo-daemon-cli/internal"
)

const (
	// ServiceLabel is the launchd Label / systemd unit prefix. Reverse-DNS style.
	ServiceLabel = "ai.octo.daemon"

	// systemdUnitName is the unit filename in ~/.config/systemd/user/.
	systemdUnitName = "octo-daemon.service"

	// OctoDaemonUnderServiceEnv marks a daemon process as launched by a service
	// manager. main.go reads this to decide whether to map exit 2/78 to 0.
	OctoDaemonUnderServiceEnv = "OCTO_DAEMON_UNDER_SERVICE"
)

// ServiceEnvDir is where wrapper.sh and the env file live.
func ServiceEnvDir() string { return filepath.Join(internal.DataDir(), "service-env") }

// LogDir is where launchd writes stdout/stderr (Linux uses journald instead).
func LogDir() string { return filepath.Join(internal.DataDir(), "logs") }

func WrapperScriptPath() string {
	return filepath.Join(ServiceEnvDir(), ServiceLabel+"-env-wrapper.sh")
}

func EnvFilePath() string {
	return filepath.Join(ServiceEnvDir(), ServiceLabel+".env")
}

// PlistPath returns ~/Library/LaunchAgents/<label>.plist on darwin.
func PlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", ServiceLabel+".plist"), nil
}

// SystemdUnitPath returns ~/.config/systemd/user/octo-daemon.service on linux.
func SystemdUnitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", systemdUnitName), nil
}

// StdoutLogPath / StderrLogPath only apply to macOS (systemd uses journal).
func StdoutLogPath() string { return filepath.Join(LogDir(), "daemon.log") }
func StderrLogPath() string { return filepath.Join(LogDir(), "daemon.err.log") }
