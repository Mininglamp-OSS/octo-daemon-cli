package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Config struct {
	APIKey     string
	APIURL     string
	DeviceName string
	CLIVersion string

	HeartbeatInterval  time.Duration
	SlowDetectInterval time.Duration
	MatterPullInterval time.Duration
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
	if c.MatterPullInterval == 0 {
		// Decoupled from HeartbeatInterval. Used to ride the heartbeat
		// response (one matter pull per heartbeat tick), which coupled
		// matter pull frequency to whatever heartbeat got tuned to.
		// Own ticker pins matter pull to its own cadence.
		// Ops override via OCTO_MATTER_PULL_SECONDS env var (positive int).
		c.MatterPullInterval = envSecondsOrDefault("OCTO_MATTER_PULL_SECONDS", 3*time.Second)
	}
	if c.RegisterTimeout == 0 {
		c.RegisterTimeout = 30 * time.Second
	}
}

type persistedConfig struct {
	APIKey                   string `json:"api_key"`
	APIURL                   string `json:"api_url"`
	DeviceName               string `json:"device_name,omitempty"`
	HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds,omitempty"`
}

func ConfigFilePath() string {
	return filepath.Join(DataDir(), "config.json")
}

func SaveConfig(cfg Config) error {
	p := persistedConfig{
		APIKey:     cfg.APIKey,
		APIURL:     cfg.APIURL,
		DeviceName: cfg.DeviceName,
	}
	if cfg.HeartbeatInterval > 0 {
		p.HeartbeatIntervalSeconds = int(cfg.HeartbeatInterval / time.Second)
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	path := ConfigFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var p persistedConfig
	if err := json.Unmarshal(data, &p); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	cfg := Config{
		APIKey:     p.APIKey,
		APIURL:     p.APIURL,
		DeviceName: p.DeviceName,
	}
	if p.HeartbeatIntervalSeconds > 0 {
		cfg.HeartbeatInterval = time.Duration(p.HeartbeatIntervalSeconds) * time.Second
	}
	return cfg, nil
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
