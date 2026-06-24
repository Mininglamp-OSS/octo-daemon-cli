package adapter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// parseConfigFilePath extracts the openclaw.json path from `openclaw config
// file` output. openclaw may prepend a banner or plugin log lines to stdout,
// so scan for the line ending in openclaw.json rather than trusting line count.
// The returned path is normalized (~ expanded; a non-absolute path resolved
// against home) so os.ReadFile / os.Rename act on the same file the gateway
// watches.
func parseConfigFilePath(out string) (string, error) {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasSuffix(l, "openclaw.json") {
			return normalizeConfigPath(l), nil
		}
	}
	return "", fmt.Errorf("openclaw config file: no openclaw.json path in output: %q", truncate(out, 200))
}

// normalizeConfigPath expands a leading ~ to the user's home dir, and resolves a
// non-absolute path against home (openclaw runs unix-side here; Windows ~\ is
// out of scope, tracked separately under daemon Windows support). A path that
// can't be resolved (no home) is returned unchanged.
func normalizeConfigPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
		return p
	}
	if !filepath.IsAbs(p) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p)
		}
	}
	return p
}
