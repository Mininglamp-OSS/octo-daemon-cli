package internal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateSpaceID(t *testing.T) {
	ok := []string{"default", "sp_abc", "minglue_default", "A-1.b"}
	for _, s := range ok {
		if err := ValidateSpaceID(s); err != nil {
			t.Errorf("ValidateSpaceID(%q) unexpected error: %v", s, err)
		}
	}
	bad := []string{"", "..", ".", "a/b", `a\b`, "../etc", "has space", "x*y"}
	for _, s := range bad {
		if err := ValidateSpaceID(s); err == nil {
			t.Errorf("ValidateSpaceID(%q) expected error, got nil", s)
		}
	}
}

func TestEnsureDaemonID_PerSpaceAndStable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	a1, err := EnsureDaemonID("sp_a")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := EnsureDaemonID("sp_a")
	if err != nil {
		t.Fatal(err)
	}
	if a1 != a2 {
		t.Fatalf("daemon id not stable: %q vs %q", a1, a2)
	}
	b, err := EnsureDaemonID("sp_b")
	if err != nil {
		t.Fatal(err)
	}
	if a1 == b {
		t.Fatalf("different spaces must get different ids, both %q", a1)
	}
	if _, err := os.Stat(filepath.Join(dir, ".octo-daemon", "sp_a", "daemon.id")); err != nil {
		t.Fatalf("per-space daemon.id not created: %v", err)
	}
}
