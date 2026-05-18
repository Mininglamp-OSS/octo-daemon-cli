package service

import (
	"strings"
	"testing"
)

func TestSystemdQuote_Basic(t *testing.T) {
	got, err := systemdQuote("/usr/bin/octo-daemon")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != `"/usr/bin/octo-daemon"` {
		t.Errorf("got %q", got)
	}
}

func TestSystemdQuote_EscapesBackslashQuotePercent(t *testing.T) {
	// backslash first, then quote, then percent
	got, err := systemdQuote(`a\b"c%d`)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// expected: backslash doubled, quote escaped, percent doubled
	if got != `"a\\b\"c%%d"` {
		t.Errorf("got %q", got)
	}
}

func TestSystemdQuote_RejectsControl(t *testing.T) {
	for _, bad := range []string{"a\nb", "a\rb", "a\tb"} {
		if _, err := systemdQuote(bad); err == nil {
			t.Errorf("systemdQuote(%q) should error", bad)
		}
	}
}

func TestRenderSystemdUnit_Structure(t *testing.T) {
	out, err := RenderSystemdUnit(SystemdUnitRenderArgs{
		WrapperScript: "/home/x/.octo-daemon/service-env/ai.octo.daemon-env-wrapper.sh",
		EnvFile:       "/home/x/.octo-daemon/service-env/ai.octo.daemon.env",
		ExecPath:      "/usr/local/bin/octo-daemon",
		ConfigPath:    "/home/x/.octo-daemon/config.json",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	mustContain := []string{
		"Description=Octo agent runtime monitor",
		"Type=simple",
		"Restart=on-failure",
		"RestartSec=10s",
		`ExecStart="/home/x/.octo-daemon/service-env/ai.octo.daemon-env-wrapper.sh" "/home/x/.octo-daemon/service-env/ai.octo.daemon.env" "/usr/local/bin/octo-daemon" start --config "/home/x/.octo-daemon/config.json"`,
		"WantedBy=default.target",
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("unit missing %q\nfull:\n%s", s, out)
		}
	}
	// Must NOT use EnvironmentFile= since we wrap via shell source instead.
	if strings.Contains(out, "EnvironmentFile") {
		t.Errorf("unit must not use EnvironmentFile; got:\n%s", out)
	}
	// Must NOT have RestartPreventExitStatus since main.go handles the 2/78→0 mapping.
	if strings.Contains(out, "RestartPreventExitStatus") {
		t.Errorf("unit must not use RestartPreventExitStatus; got:\n%s", out)
	}
}

func TestRenderSystemdUnit_RejectsBadPaths(t *testing.T) {
	_, err := RenderSystemdUnit(SystemdUnitRenderArgs{
		WrapperScript: "/ok/w",
		EnvFile:       "/ok/e",
		ExecPath:      "/bin/daemon\nwith-newline",
		ConfigPath:    "/ok/c",
	})
	if err == nil {
		t.Error("expected error from control char in exec path")
	}
}
