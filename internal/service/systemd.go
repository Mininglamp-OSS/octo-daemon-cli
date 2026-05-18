package service

import (
	"fmt"
	"strings"
)

// systemdQuote wraps s in systemd's double-quoted form for ExecStart argv.
// Rules:
//   - escape " and \ by prepending \
//   - escape literal % as %% (systemd specifier expansion)
//   - reject newline / CR / tab / other control chars (not a legal path)
//
// See plan §三.linux for rationale.
func systemdQuote(s string) (string, error) {
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			return "", fmt.Errorf("control character in systemd argv value: %q", s)
		}
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, `%`, `%%`)
	return `"` + s + `"`, nil
}

// SystemdUnitRenderArgs holds fields consumed by the systemd unit template.
type SystemdUnitRenderArgs struct {
	WrapperScript string
	EnvFile       string
	ExecPath      string
	ConfigPath    string
}

// RenderSystemdUnit produces the systemd --user unit body.
// Restart=on-failure + RestartSec=10s matches plan §一; exit 0 / mapped 2/78→0
// are considered success so no restart, 75 / 1 trigger restart.
func RenderSystemdUnit(a SystemdUnitRenderArgs) (string, error) {
	qWrapper, err := systemdQuote(a.WrapperScript)
	if err != nil {
		return "", fmt.Errorf("wrapper path: %w", err)
	}
	qEnv, err := systemdQuote(a.EnvFile)
	if err != nil {
		return "", fmt.Errorf("env file path: %w", err)
	}
	qExec, err := systemdQuote(a.ExecPath)
	if err != nil {
		return "", fmt.Errorf("exec path: %w", err)
	}
	qConfig, err := systemdQuote(a.ConfigPath)
	if err != nil {
		return "", fmt.Errorf("config path: %w", err)
	}

	return fmt.Sprintf(`[Unit]
Description=Octo agent runtime monitor
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s %s %s start --config %s
Restart=on-failure
RestartSec=10s
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=default.target
`, qWrapper, qEnv, qExec, qConfig), nil
}
