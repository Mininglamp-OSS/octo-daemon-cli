package adapter

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseConfigFilePath(t *testing.T) {
	home, _ := os.UserHomeDir()
	cases := []struct {
		name string
		out  string
		want string
	}{
		{"clean", "/Users/x/.openclaw/openclaw.json\n", "/Users/x/.openclaw/openclaw.json"},
		{"with banner", "🦞 OpenClaw 2026.6.6\n[plugins] octo loaded\n/home/u/.openclaw/openclaw.json\n", "/home/u/.openclaw/openclaw.json"},
		{"trailing spaces", "  /tmp/cfg/openclaw.json  \n", "/tmp/cfg/openclaw.json"},
		{"tilde expands to home", "~/.openclaw/openclaw.json\n", filepath.Join(home, ".openclaw", "openclaw.json")},
		{"relative resolves against home", "openclaw.json\n", filepath.Join(home, "openclaw.json")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseConfigFilePath(c.out)
			if err != nil {
				t.Fatalf("parseConfigFilePath: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	if _, err := parseConfigFilePath("no path here\n[plugins] noise\n"); err == nil {
		t.Error("expected error when no openclaw.json line present")
	}
}
