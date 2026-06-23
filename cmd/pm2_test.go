package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteEcosystem(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	goBin := "/opt/homebrew/lib/node_modules/@mininglamp-oss/octo-daemon/bin/octo-daemon"
	cfgPath := "/Users/me/with space/.octo-daemon/config.json"

	ecoPath, err := writeEcosystem(goBin, cfgPath)
	if err != nil {
		t.Fatalf("writeEcosystem: %v", err)
	}
	if want := filepath.Join(home, ".octo-daemon", "ecosystem.config.js"); ecoPath != want {
		t.Fatalf("ecoPath = %q, want %q", ecoPath, want)
	}

	data, err := os.ReadFile(ecoPath)
	if err != nil {
		t.Fatalf("read ecosystem: %v", err)
	}
	got := string(data)

	mustContain := []string{
		`name: "octo-daemon"`,
		`interpreter: "none"`,
		// args is a JSON array so a path with spaces stays one argv entry.
		`args: ["start","--config","/Users/me/with space/.octo-daemon/config.json"]`,
		`script: "/opt/homebrew/lib/node_modules/@mininglamp-oss/octo-daemon/bin/octo-daemon"`,
		`stop_exit_codes: [2, 78]`,
	}
	for _, sub := range mustContain {
		if !strings.Contains(got, sub) {
			t.Errorf("ecosystem missing %q\n--- got ---\n%s", sub, got)
		}
	}

	// The launched command must never carry --daemon, or pm2 would re-run the
	// bootstrapper on every restart (infinite recursion). Check the args line
	// specifically (the header comment legitimately mentions --daemon).
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "args:") && strings.Contains(line, "--daemon") {
			t.Errorf("ecosystem args must not contain --daemon: %q", line)
		}
	}
}
