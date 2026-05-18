package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Config struct {
	APIKey     string
	APIURL     string
	DeviceName string
	CLIVersion string

	HeartbeatInterval time.Duration
	RegisterTimeout   time.Duration
}

func (c *Config) withDefaults() {
	if c.HeartbeatInterval == 0 {
		c.HeartbeatInterval = 15 * time.Second
	}
	if c.RegisterTimeout == 0 {
		c.RegisterTimeout = 30 * time.Second
	}
}

type persistedConfig struct {
	APIKey     string `json:"api_key"`
	APIURL     string `json:"api_url"`
	DeviceName string `json:"device_name,omitempty"`
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
	return Config{
		APIKey:     p.APIKey,
		APIURL:     p.APIURL,
		DeviceName: p.DeviceName,
	}, nil
}
