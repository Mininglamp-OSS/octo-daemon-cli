package internal

import (
	"context"
	"encoding/json"
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
// valid — so we parse stdout and ignore the exit code. Packages absent from the
// output (including unscoped local `npm link` entries, which key on a different
// unscoped name) are reported with Version "" so the server treats them as
// not-installed.
func DetectDeviceComponents() []DeviceComponent {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "npm", "ls", "-g", "--depth=0", "--json")
	cmd.Stderr = nil
	out, _ := cmd.Output() // exit code ignored; stdout JSON valid even on non-zero

	return parseDeviceComponents(out)
}

// parseDeviceComponents maps `npm ls -g --json` stdout onto the fixed target
// whitelist. Invalid/empty input yields all targets with Version "".
func parseDeviceComponents(out []byte) []DeviceComponent {
	var parsed npmLsOutput
	_ = json.Unmarshal(out, &parsed) // parse failure → all versions ""

	components := make([]DeviceComponent, 0, len(deviceComponentTargets))
	for _, t := range deviceComponentTargets {
		components = append(components, DeviceComponent{
			Type:         "nodejs",
			Name:         t.name,
			ComponentKey: t.key,
			Version:      parsed.Dependencies[t.key].Version,
		})
	}
	return components
}
