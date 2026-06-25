package internal

import "testing"

func TestShouldSkipDaemonUpgrade(t *testing.T) {
	tests := []struct {
		name      string
		installed string
		target    string
		want      bool
	}{
		{"already at target", "0.0.5", "0.0.5", true},
		{"empty target with version installed", "0.0.5", "", true},
		{"target newer than installed", "0.0.4", "0.0.5", false},
		{"nothing installed, empty target", "", "", false},
		{"nothing installed, target set", "", "0.0.5", false},
		{"v-prefixed target matches installed", "0.0.5", "v0.0.5", true},
		{"installed newer than target (no downgrade)", "0.0.6", "0.0.5", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldSkipDaemonUpgrade(tt.installed, tt.target); got != tt.want {
				t.Errorf("shouldSkipDaemonUpgrade(%q, %q) = %v, want %v", tt.installed, tt.target, got, tt.want)
			}
		})
	}
}
