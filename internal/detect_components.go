package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

// deviceComponentTargets is the fixed whitelist of npm global packages reported
// as machine-level components. Order is preserved in the output. Hard-coded for
// now; may later be driven by a server-side provider snapshot.
var deviceComponentTargets = []struct {
	name string // short name without scope
	key  string // full npm package name
}{
	{"octo-daemon", "@mininglamp-oss/octo-daemon"},
	{"octo-cli", "@mininglamp-oss/octo-cli"},
	{"cc-channel-octo", "@mininglamp-oss/cc-channel-octo"},
	{"claude-agent-sdk", "@anthropic-ai/claude-agent-sdk"},
}

// npmLsOutput is the subset of `npm ls -g --json` we parse.
type npmLsOutput struct {
	Dependencies map[string]struct {
		Version string `json:"version"`
	} `json:"dependencies"`
}

// DetectDeviceComponents reports installed versions of the whitelisted npm
// global packages via a single `npm ls -g --depth=0 --json`. npm exits non-zero
// when the global tree has extraneous/missing deps, but stdout JSON is still
// valid — so we don't treat the exit code as failure; we validate by parsing.
// Packages absent from the output (including unscoped local `npm link` entries,
// which key on a different unscoped name) are omitted entirely — the server
// treats the reported list as the full inventory, so a not-installed package
// must not appear as a phantom empty-version record.
//
// Returns an error when the probe itself failed (no output at all, or
// unparseable stdout) so the caller can distinguish a genuine empty inventory
// (success → empty slice) from a transient/structural failure. The caller must
// NOT report an empty slice as an authoritative inventory on error, or a flaky
// `npm ls` would tell the server every component was uninstalled.
func DetectDeviceComponents() ([]DeviceComponent, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "npm", "ls", "-g", "--depth=0", "--json")
	cmd.Stderr = nil
	out, err := cmd.Output() // exit code ignored (non-zero with valid JSON is normal); validated by parse
	if len(out) == 0 {
		if err == nil {
			err = fmt.Errorf("no output")
		}
		return nil, fmt.Errorf("npm ls -g failed: %w", err)
	}

	return parseDeviceComponents(out)
}

// parseDeviceComponents maps `npm ls -g --json` stdout onto the fixed target
// whitelist, omitting targets that aren't installed (absent from the npm tree →
// empty version). Returns an error on unparseable input so the caller can tell a
// real empty inventory from a malformed/failed probe.
func parseDeviceComponents(out []byte) ([]DeviceComponent, error) {
	var parsed npmLsOutput
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("parse npm ls output: %w", err)
	}

	components := make([]DeviceComponent, 0, len(deviceComponentTargets))
	for _, t := range deviceComponentTargets {
		version := parsed.Dependencies[t.key].Version
		if version == "" {
			continue // not installed — don't report a phantom record
		}
		components = append(components, DeviceComponent{
			Type:         "nodejs",
			Name:         t.name,
			ComponentKey: t.key,
			Version:      version,
		})
	}
	return components, nil
}
