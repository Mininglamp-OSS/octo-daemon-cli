package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// openclawPluginsListJSON is the shape returned by `openclaw plugins list --json`.
// We only use id/version/enabled; other fields parsed for potential future use.
type openclawPluginJSON struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Enabled bool   `json:"enabled"`
	Status  string `json:"status"`
	Origin  string `json:"origin"`
}

// openclawPluginsListJSON uses a pointer slice so we can distinguish
// "plugins: []" (valid, means no enabled plugins) from "no plugins field"
// (invalid schema — should trigger CLI fallback).
type openclawPluginsListJSON struct {
	Plugins *[]openclawPluginJSON `json:"plugins"`
}

// parseOpenclawPluginsJSON parses output of `openclaw plugins list --json`.
// Returns only enabled plugins as PluginInfo{Name: id, Version: version}.
// Name field on the wire is the npm/id string (not the human display name) because
// the server and frontend match plugins by name == "openclaw-channel-dmwork".
//
// Missing plugins field → error (so caller can fall back to directory scan).
// Empty plugins array → nil slice, no error (legitimate "nothing enabled").
// Noise before/after the JSON object is tolerated via candidate scanning.
func parseOpenclawPluginsJSON(data []byte) ([]PluginInfo, error) {
	// Fast path: whole input is the JSON object.
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		if plugins, err := decodePluginsObject(trimmed); err == nil {
			return plugins, nil
		}
	}

	// Slow path: scan for each '{' position, try to decode an object there
	// using json.Decoder. Skip past consumed bytes on success so we don't
	// re-scan inside nested braces.
	for i := 0; i < len(data); i++ {
		if data[i] != '{' {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(data[i:]))
		var probe map[string]json.RawMessage
		if err := dec.Decode(&probe); err != nil {
			continue
		}
		consumed := int(dec.InputOffset())
		candidate := data[i : i+consumed]
		if plugins, err := decodePluginsObject(candidate); err == nil {
			return plugins, nil
		}
		// Candidate parsed as JSON but wasn't the plugins shape; jump past
		// it rather than restarting one byte later (avoids re-entering a
		// sub-object that just got validated).
		if consumed > 1 {
			i += consumed - 1
		}
	}
	return nil, fmt.Errorf("no JSON object with 'plugins' field found in output")
}

func decodePluginsObject(data []byte) ([]PluginInfo, error) {
	var raw openclawPluginsListJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if raw.Plugins == nil {
		return nil, fmt.Errorf("no plugins field")
	}
	var out []PluginInfo
	for _, p := range *raw.Plugins {
		if !p.Enabled {
			continue
		}
		if p.ID == "" || p.Version == "" {
			continue
		}
		out = append(out, PluginInfo{Name: p.ID, Version: p.Version})
	}
	return out, nil
}

// detectOpenclawPluginsViaCLI runs `openclaw plugins list --json` and returns
// the enabled plugins. Callers should fall back to the directory scan on error.
func detectOpenclawPluginsViaCLI(binPath string) ([]PluginInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binPath, "plugins", "list", "--json")
	// Config warnings go to stderr and would otherwise pollute logs.
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("run openclaw plugins list --json: %w", err)
	}
	return parseOpenclawPluginsJSON(out)
}

// scanOpenclawExtensionsDir is the legacy directory-based detector kept as a
// fallback for older openclaw versions where `plugins list --json` is missing
// or broken. It only sees plugins installed under ~/.openclaw/extensions/ and
// misses npm/bundled sources — so it is strictly a safety net.
func scanOpenclawExtensionsDir() []PluginInfo {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	extDir := filepath.Join(home, ".openclaw", "extensions")
	entries, err := os.ReadDir(extDir)
	if err != nil {
		return nil
	}

	var plugins []PluginInfo
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasSuffix(entry.Name(), ".bak") || entry.Name() == "node_modules" {
			continue
		}
		pkgPath := filepath.Join(extDir, entry.Name(), "package.json")
		data, err := os.ReadFile(pkgPath)
		if err != nil {
			continue
		}
		var pkg struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if json.Unmarshal(data, &pkg) != nil || pkg.Name == "" {
			continue
		}
		plugins = append(plugins, PluginInfo{
			Name:    pkg.Name,
			Version: pkg.Version,
		})
	}
	return plugins
}

// DetectOpenclawPlugins is the authoritative plugin detector. It prefers the
// CLI (covers bundled + npm + global + extensions sources) and falls back to
// scanning ~/.openclaw/extensions/ when the CLI fails or is unsupported.
func DetectOpenclawPlugins(binPath string) []PluginInfo {
	if binPath != "" {
		plugins, err := detectOpenclawPluginsViaCLI(binPath)
		if err == nil {
			return plugins
		}
		log.Printf("[WARN] openclaw plugins list --json failed, falling back to dir scan: %v", err)
	}
	fallback := scanOpenclawExtensionsDir()
	if len(fallback) > 0 {
		log.Printf("[INFO] plugin detection fallback: found %d in ~/.openclaw/extensions/", len(fallback))
	}
	return fallback
}
