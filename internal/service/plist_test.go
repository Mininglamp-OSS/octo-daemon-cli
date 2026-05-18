package service

import (
	"strings"
	"testing"
)

func TestXMLEscape(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"a&b", "a&amp;b"},
		{"<tag>", "&lt;tag&gt;"},
		{`"quote"`, "&quot;quote&quot;"},
		{"it's", "it&apos;s"},
		{"&<>\"'", "&amp;&lt;&gt;&quot;&apos;"},
	}
	for _, c := range cases {
		if got := xmlEscape(c.in); got != c.want {
			t.Errorf("xmlEscape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderPlist_Structure(t *testing.T) {
	out := RenderPlist(PlistRenderArgs{
		WrapperScript: "/Users/x/.octo-daemon/service-env/ai.octo.daemon-env-wrapper.sh",
		EnvFile:       "/Users/x/.octo-daemon/service-env/ai.octo.daemon.env",
		ExecPath:      "/usr/local/bin/octo-daemon",
		ConfigPath:    "/Users/x/.octo-daemon/config.json",
		DataDir:       "/Users/x/.octo-daemon",
		StdoutLog:     "/Users/x/.octo-daemon/logs/daemon.log",
		StderrLog:     "/Users/x/.octo-daemon/logs/daemon.err.log",
	})

	// Core invariants that must not drift.
	mustContain := []string{
		`<key>Label</key>`,
		`<string>ai.octo.daemon</string>`,
		`<key>RunAtLoad</key>`,
		`<key>KeepAlive</key>`,
		`<key>SuccessfulExit</key>`,
		`<false/>`,
		`<key>ThrottleInterval</key>`,
		`<integer>10</integer>`,
		`<string>/usr/local/bin/octo-daemon</string>`,
		`<string>start</string>`,
		`<string>--config</string>`,
		`<string>/Users/x/.octo-daemon/config.json</string>`,
		`<string>/Users/x/.octo-daemon/logs/daemon.log</string>`,
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("plist missing %q\nfull:\n%s", s, out)
		}
	}

	// Regressions to reject: Crashed=true inside KeepAlive confuses the restart
	// semantics we settled on (SuccessfulExit=false alone is sufficient).
	if strings.Contains(out, "<key>Crashed</key>") {
		t.Errorf("plist must not contain Crashed key; got:\n%s", out)
	}
}

func TestRenderPlist_EscapesSpecialChars(t *testing.T) {
	out := RenderPlist(PlistRenderArgs{
		WrapperScript: "/path with & chars/wrapper.sh",
		EnvFile:       "/path/env",
		ExecPath:      "/bin/<weird>",
		ConfigPath:    "/c",
		DataDir:       "/d",
		StdoutLog:     "/l",
		StderrLog:     "/e",
	})
	if !strings.Contains(out, `/path with &amp; chars/wrapper.sh`) {
		t.Errorf("& not escaped; got:\n%s", out)
	}
	if !strings.Contains(out, `&lt;weird&gt;`) {
		t.Errorf("<> not escaped; got:\n%s", out)
	}
}
