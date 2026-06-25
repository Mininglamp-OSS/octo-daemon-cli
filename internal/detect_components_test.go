package internal

import "testing"

func TestParseDeviceComponents(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    map[string]string // component_key → version, for components expected present
		wantErr bool
	}{
		{
			name: "all present",
			in: `{"dependencies":{
				"@mininglamp-oss/octo-daemon":{"version":"1.0.5"},
				"@mininglamp-oss/octo-cli":{"version":"1.0.10"},
				"@mininglamp-oss/cc-channel-octo":{"version":"1.0.1"},
				"@anthropic-ai/claude-agent-sdk":{"version":"0.3.170"}
			}}`,
			want: map[string]string{
				"@mininglamp-oss/octo-daemon":     "1.0.5",
				"@mininglamp-oss/octo-cli":        "1.0.10",
				"@mininglamp-oss/cc-channel-octo": "1.0.1",
				"@anthropic-ai/claude-agent-sdk":  "0.3.170",
			},
		},
		{
			name: "missing packages omitted, not reported empty",
			in:   `{"dependencies":{"@mininglamp-oss/octo-cli":{"version":"1.0.10"}}}`,
			want: map[string]string{
				"@mininglamp-oss/octo-cli": "1.0.10",
			},
		},
		{
			name: "unscoped local link ignored",
			in:   `{"dependencies":{"cc-channel-octo":{"version":"9.9.9"}}}`,
			want: map[string]string{},
		},
		{
			name: "valid json, nothing installed → empty inventory, no error",
			in:   `{"dependencies":{}}`,
			want: map[string]string{},
		},
		{
			name:    "invalid json → error, not an authoritative empty inventory",
			in:      `npm error stuff not json`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDeviceComponents([]byte(tt.in))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for malformed input, got nil (and %v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			gotMap := make(map[string]string, len(got))
			for _, c := range got {
				gotMap[c.ComponentKey] = c.Version
				if c.Type != "nodejs" {
					t.Errorf("%s type = %q, want nodejs", c.ComponentKey, c.Type)
				}
				if c.Version == "" {
					t.Errorf("%s reported with empty version; not-installed components must be omitted", c.ComponentKey)
				}
			}
			if len(gotMap) != len(tt.want) {
				t.Errorf("got %d components %v, want %d %v", len(gotMap), gotMap, len(tt.want), tt.want)
			}
			for key, wantVer := range tt.want {
				if gotMap[key] != wantVer {
					t.Errorf("%s version = %q, want %q", key, gotMap[key], wantVer)
				}
			}
		})
	}
}
