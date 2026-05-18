package internal

import "testing"

func TestIsVersionOlder(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
		desc            string
	}{
		{"0.12.0", "0.13.0", true, "patch older"},
		{"0.13.0", "0.13.0", false, "equal"},
		{"0.13.1", "0.13.0", false, "newer"},
		{"v0.12.0", "0.13.0", true, "v prefix on current"},
		{"0.12.0", "v0.13.0", true, "v prefix on latest"},
		{"v0.13.0", "0.13.0", false, "equal with v prefix"},
		{"0.6.3-dev.dc640a2e", "0.6.3", false, "dev suffix stripped equals release"},
		{"0.6.3-dev.dc640a2e", "0.6.4", true, "dev suffix stripped is older"},
		{"2026.5.7", "2026.6.0", true, "date-based"},
		{"dev", "0.1.0", true, "dev always older"},
		{"unknown", "0.1.0", true, "unknown always older"},
		{"", "0.1.0", true, "empty always older"},
		{"0.1.0", "dev", false, "real version not older than dev"},
		{"0.13", "0.13.0", true, "shorter shape treated as older"},
		{"abc", "1.0.0", false, "non-numeric returns false"},
		{"1.0.0", "abc", false, "non-numeric returns false reverse"},
	}
	for _, c := range cases {
		if got := isVersionOlder(c.current, c.latest); got != c.want {
			t.Errorf("%s: isVersionOlder(%q, %q) = %v, want %v", c.desc, c.current, c.latest, got, c.want)
		}
	}
}
