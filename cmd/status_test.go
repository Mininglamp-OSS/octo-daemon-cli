package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Mininglamp-OSS/octo-daemon-cli/internal"
)

func TestStatusJSONReportsStoppedWithStalePID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := filepath.Join(home, ".octo-daemon")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "daemon.pid"), []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := writeStatus(&buf, true); err != nil {
		t.Fatal(err)
	}

	var report statusReport
	if err := json.Unmarshal(buf.Bytes(), &report); err != nil {
		t.Fatalf("decode status json: %v", err)
	}
	if report.Status != "stopped" || report.Locked {
		t.Fatalf("status = %#v, want stopped/unlocked", report)
	}
	if report.PID != 999999 || !report.PIDFileStale {
		t.Fatalf("stale pid fields = %#v, want pid 999999 and stale=true", report)
	}
}

func TestStatusJSONReportsHeldLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	lockFile, err := internal.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = lockFile.Close()
		internal.RemovePID()
	}()

	var buf bytes.Buffer
	if err := writeStatus(&buf, true); err != nil {
		t.Fatal(err)
	}

	var report statusReport
	if err := json.Unmarshal(buf.Bytes(), &report); err != nil {
		t.Fatalf("decode status json: %v", err)
	}
	if report.Status != "running" || !report.Locked {
		t.Fatalf("status = %#v, want running/locked", report)
	}
	if report.PID != os.Getpid() || report.PIDFileStale {
		t.Fatalf("lock pid fields = %#v, want pid %d and stale=false", report, os.Getpid())
	}
}
