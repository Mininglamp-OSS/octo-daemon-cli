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

func TestLoadProfiles_LegacySingleObjectIsEmpty(t *testing.T) {
	// A legacy single-object config.json has no "profiles" key and must load
	// as zero profiles (no auto-wrapping into a "default" profile). `config`
	// then replaces it outright.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	const legacy = `{"api_key":"uk_abc","api_url":"http://localhost:8090","device_name":"laptop"}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	cfgs, err := LoadProfiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfgs) != 0 {
		t.Fatalf("legacy single-object config should yield 0 profiles, got %d: %+v", len(cfgs), cfgs)
	}
}

func TestLoadProfiles_NewFormatRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	want := []Config{
		{SpaceID: "sp_a", APIKey: "uk_a", FleetURL: "http://f/a", ServerURL: "http://s/a", MatterURL: "http://m/a", DeviceName: "dev1", HeartbeatInterval: 3 * time.Second},
		{SpaceID: "sp_b", APIKey: "uk_b", FleetURL: "http://f/b", ServerURL: "http://s/b"},
	}
	if err := SaveProfiles(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadProfiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 profiles, got %d", len(got))
	}
	if got[0].SpaceID != "sp_a" || got[0].APIKey != "uk_a" || got[0].FleetURL != "http://f/a" || got[0].ServerURL != "http://s/a" || got[0].MatterURL != "http://m/a" {
		t.Fatalf("profile 0 round-trip mismatch: %+v", got[0])
	}
	if got[0].HeartbeatInterval != 3*time.Second {
		t.Fatalf("profile 0 heartbeat want 3s, got %v", got[0].HeartbeatInterval)
	}
	if got[1].SpaceID != "sp_b" || got[1].HeartbeatInterval != 0 {
		t.Fatalf("profile 1 round-trip mismatch: %+v", got[1])
	}
}

func TestSaveProfiles_ZeroHeartbeatOmitsField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfgs := []Config{{SpaceID: "sp_q", APIKey: "uk_q", FleetURL: "http://h:1", ServerURL: "http://h:1"}}
	if err := SaveProfiles(path, cfgs); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); strings.Contains(got, "heartbeat_interval_seconds") {
		t.Fatalf("zero HeartbeatInterval should omit field, got: %s", got)
	}
}
