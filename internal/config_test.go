package internal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWithDefaults_AppliesHeartbeatDefault(t *testing.T) {
	c := Config{}
	c.withDefaults()
	if c.HeartbeatInterval != 5*time.Second {
		t.Fatalf("default HeartbeatInterval want 5s, got %v", c.HeartbeatInterval)
	}
	if c.RegisterTimeout != 30*time.Second {
		t.Fatalf("default RegisterTimeout want 30s, got %v", c.RegisterTimeout)
	}
}

func TestWithDefaults_PreservesNonZero(t *testing.T) {
	c := Config{HeartbeatInterval: 2 * time.Second, RegisterTimeout: 7 * time.Second}
	c.withDefaults()
	if c.HeartbeatInterval != 2*time.Second {
		t.Fatalf("non-zero HeartbeatInterval should be preserved, got %v", c.HeartbeatInterval)
	}
	if c.RegisterTimeout != 7*time.Second {
		t.Fatalf("non-zero RegisterTimeout should be preserved, got %v", c.RegisterTimeout)
	}
}

func TestSaveLoad_BackwardsCompat_NoHeartbeatField(t *testing.T) {
	// Legacy config.json without heartbeat_interval_seconds should load
	// with HeartbeatInterval=0, then withDefaults applies the 5s default.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	const legacy = `{"api_key":"uk_abc","api_url":"http://localhost:8090","device_name":"laptop"}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != "uk_abc" || cfg.APIURL != "http://localhost:8090" || cfg.DeviceName != "laptop" {
		t.Fatalf("unexpected legacy load: %+v", cfg)
	}
	if cfg.HeartbeatInterval != 0 {
		t.Fatalf("legacy file should leave HeartbeatInterval zero (defaults apply later), got %v", cfg.HeartbeatInterval)
	}
	cfg.withDefaults()
	if cfg.HeartbeatInterval != 5*time.Second {
		t.Fatalf("post-defaults HeartbeatInterval want 5s, got %v", cfg.HeartbeatInterval)
	}
}

func TestSaveLoad_RoundTripHeartbeat(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir) // ConfigFilePath uses DataDir() under $HOME/.octo-daemon
	want := Config{
		APIKey:            "uk_xyz",
		APIURL:            "http://host:8090",
		DeviceName:        "dev1",
		HeartbeatInterval: 3 * time.Second,
	}
	if err := SaveConfig(want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(ConfigFilePath())
	if err != nil {
		t.Fatal(err)
	}
	if got.HeartbeatInterval != 3*time.Second {
		t.Fatalf("round-trip HeartbeatInterval want 3s, got %v", got.HeartbeatInterval)
	}
	if got.APIKey != want.APIKey || got.APIURL != want.APIURL || got.DeviceName != want.DeviceName {
		t.Fatalf("round-trip identity mismatch: %+v", got)
	}
}

func TestSaveConfig_ZeroHeartbeatOmitsField(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	cfg := Config{APIKey: "uk_q", APIURL: "http://h:1", HeartbeatInterval: 0}
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(ConfigFilePath())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); strings.Contains(got, "heartbeat_interval_seconds") {
		t.Fatalf("zero HeartbeatInterval should omit field, got: %s", got)
	}
}
