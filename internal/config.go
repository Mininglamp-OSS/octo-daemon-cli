package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// fleetURLPrefix is appended to server_url to derive the default fleet base
// when --fleet-url is not supplied:
// http://host:3000 -> http://host:3000/fleet/api.
const fleetURLPrefix = "/fleet/api"

// ResolveFleetURL returns the fleet base URL: the explicit fleetURL when set,
// otherwise serverURL + fleetURLPrefix. Trailing slashes are trimmed.
func ResolveFleetURL(serverURL, fleetURL string) string {
	if fleetURL != "" {
		return strings.TrimRight(fleetURL, "/")
	}
	return strings.TrimRight(serverURL, "/") + fleetURLPrefix
}

// Config is the runtime config for ONE backendRunner (one space/profile).
type Config struct {
	SpaceID    string
	APIKey     string
	ServerURL  string
	FleetURL   string
	MatterURL  string
	DeviceName string
	CLIVersion string

	HeartbeatInterval  time.Duration
	SlowDetectInterval time.Duration
	RegisterTimeout    time.Duration
}

func (c *Config) withDefaults() {
	if c.HeartbeatInterval == 0 {
		// Keep in sync with fleet runSweeper: staleThreshold = 3x this value.
		c.HeartbeatInterval = 5 * time.Second
	}
	if c.SlowDetectInterval == 0 {
		// Decoupled from HeartbeatInterval: slowDetect used to fire every
		// 4 heartbeats (60s at the old 15s heartbeat). When heartbeat
		// shrank to 5s it accidentally accelerated to 20s — own ticker
		// pins it back to the intended 60s regardless of heartbeat tuning.
		// Ops override via OCTO_SLOW_DETECT_SECONDS env var (positive int).
		c.SlowDetectInterval = envSecondsOrDefault("OCTO_SLOW_DETECT_SECONDS", 60*time.Second)
	}
	if c.RegisterTimeout == 0 {
		c.RegisterTimeout = 30 * time.Second
	}
}

// Profile is the persisted form of one backend connection, keyed by space_id.
type Profile struct {
	SpaceID                  string `json:"space_id"`
	ServerURL                string `json:"server_url"`
	FleetURL                 string `json:"fleet_url"`
	MatterURL                string `json:"matter_url,omitempty"`
	APIKey                   string `json:"api_key"`
	DeviceName               string `json:"device_name,omitempty"`
	HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds,omitempty"`
}

// ConfigMeta records who/when last wrote config.json. Refreshed on every
// successful `octo-daemon config` write (the only path that mutates the file).
type ConfigMeta struct {
	LastDaemonCLIVersion string `json:"lastDaemonCliVersion"`
	LastConfigModifyTime string `json:"lastConfigModifyTime"` // RFC3339 with local timezone offset
}

// fileConfig is the on-disk top-level shape: {"meta":{...},"profiles":[...]}.
type fileConfig struct {
	Meta     ConfigMeta `json:"meta"`
	Profiles []Profile  `json:"profiles"`
}

func ConfigFilePath() string {
	return filepath.Join(DataDir(), "config.json")
}

func (p Profile) toConfig() Config {
	c := Config{
		SpaceID:    p.SpaceID,
		APIKey:     p.APIKey,
		ServerURL:  p.ServerURL,
		FleetURL:   p.FleetURL,
		MatterURL:  p.MatterURL,
		DeviceName: p.DeviceName,
	}
	if p.HeartbeatIntervalSeconds > 0 {
		c.HeartbeatInterval = time.Duration(p.HeartbeatIntervalSeconds) * time.Second
	}
	return c
}

func configToProfile(c Config) Profile {
	p := Profile{
		SpaceID:    c.SpaceID,
		ServerURL:  c.ServerURL,
		FleetURL:   c.FleetURL,
		MatterURL:  c.MatterURL,
		APIKey:     c.APIKey,
		DeviceName: c.DeviceName,
	}
	if c.HeartbeatInterval > 0 {
		p.HeartbeatIntervalSeconds = int(c.HeartbeatInterval / time.Second)
	}
	return p
}

// LoadProfiles reads config.json in the {"profiles":[...]} format. A pre-
// multi-profile single-object config has no "profiles" key and therefore loads
// as zero profiles — there is no legacy auto-wrapping; `octo-daemon config`
// replaces such a file outright.
func LoadProfiles(path string) ([]Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var fc fileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfgs := make([]Config, 0, len(fc.Profiles))
	for _, p := range fc.Profiles {
		cfgs = append(cfgs, p.toConfig())
	}
	return cfgs, nil
}

// SaveProfiles writes the profiles to config.json (0600), creating the data
// directory if needed. It stamps the top-level meta with the supplied
// daemon-cli version and the current local time (RFC3339 with tz offset,
// e.g. 2026-06-18T15:17:00+08:00).
func SaveProfiles(path string, cfgs []Config, version string) error {
	fc := fileConfig{
		Meta: ConfigMeta{
			LastDaemonCLIVersion: version,
			LastConfigModifyTime: time.Now().Format(time.RFC3339),
		},
		Profiles: make([]Profile, 0, len(cfgs)),
	}
	for _, c := range cfgs {
		fc.Profiles = append(fc.Profiles, configToProfile(c))
	}
	data, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// BackupLegacyConfig detects a pre-multi-profile single-object config (no
// "profiles" key, but carrying api_key/api_url) and renames it aside to
// <path>.back.<unix-ts>, leaving no config file in place. Returns the backup
// path, or "" when nothing was done. A multi-profile config, a missing file,
// or unparseable JSON is left untouched.
//
// This converts the "legacy config + new binary" case (which would otherwise
// load as zero profiles and look like an empty start) into a clean "no config"
// state, so start surfaces a clear "run config" error instead of a silent exit.
func BackupLegacyConfig(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var probe struct {
		Profiles []json.RawMessage `json:"profiles"`
		APIKey   string            `json:"api_key"`
		APIURL   string            `json:"api_url"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return "", nil // not valid JSON — LoadProfiles will surface a parse error
	}
	if probe.Profiles != nil {
		return "", nil // already multi-profile format
	}
	if probe.APIKey == "" && probe.APIURL == "" {
		return "", nil // not recognizably legacy (e.g. empty {})
	}
	backup := fmt.Sprintf("%s.back.%d", path, time.Now().Unix())
	if err := os.Rename(path, backup); err != nil {
		return "", fmt.Errorf("back up legacy config: %w", err)
	}
	return backup, nil
}

// envSecondsOrDefault reads a positive integer env var as seconds, falling
// back to the supplied default if missing/invalid/non-positive. Used for
// ops-tunable cadence knobs that don't warrant a persisted config field.
func envSecondsOrDefault(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return time.Duration(n) * time.Second
}
