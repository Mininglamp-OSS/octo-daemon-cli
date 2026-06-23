package internal

import "testing"

func TestParseDeviceComponents(t *testing.T) {
	byKey := func(cs []DeviceComponent, key string) DeviceComponent {
		for _, c := range cs {
			if c.ComponentKey == key {
				return c
			}
		}
		t.Fatalf("component %q not found in output", key)
		return DeviceComponent{}
	}

	tests := []struct {
		name string
		in   string
		want map[string]string // component_key → version
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
			name: "missing package reported empty",
			in:   `{"dependencies":{"@mininglamp-oss/octo-cli":{"version":"1.0.10"}}}`,
			want: map[string]string{
				"@mininglamp-oss/octo-daemon":     "",
				"@mininglamp-oss/octo-cli":        "1.0.10",
				"@mininglamp-oss/cc-channel-octo": "",
				"@anthropic-ai/claude-agent-sdk":  "",
			},
		},
		{
			name: "unscoped local link ignored",
			in:   `{"dependencies":{"cc-channel-octo":{"version":"9.9.9"}}}`,
			want: map[string]string{
				"@mininglamp-oss/cc-channel-octo": "",
			},
		},
		{
			name: "invalid json yields empty versions",
			in:   `npm error stuff not json`,
			want: map[string]string{
				"@mininglamp-oss/octo-daemon": "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseDeviceComponents([]byte(tt.in))
			for key, wantVer := range tt.want {
				c := byKey(got, key)
				if c.Version != wantVer {
					t.Errorf("%s version = %q, want %q", key, c.Version, wantVer)
				}
				if c.Type != "nodejs" {
					t.Errorf("%s type = %q, want nodejs", key, c.Type)
				}
			}
		})
	}
}
