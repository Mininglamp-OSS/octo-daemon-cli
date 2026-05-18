package internal

import (
	"testing"
)

const pluginsListFixture = `{
  "registry": {"source": "persisted", "diagnostics": []},
  "plugins": [
    {
      "id": "active-memory",
      "name": "Active Memory",
      "source": "/opt/homebrew/lib/node_modules/openclaw/dist/extensions/active-memory/index.js",
      "origin": "bundled",
      "enabled": false,
      "status": "disabled"
    },
    {
      "id": "openclaw-channel-dmwork",
      "name": "openclaw-channel-dmwork",
      "version": "0.6.3-dev.dc640a2e",
      "source": "/Users/caster/.openclaw/npm/node_modules/openclaw-channel-dmwork/dist/index.js",
      "origin": "global",
      "enabled": true,
      "status": "loaded"
    },
    {
      "id": "@larksuite/openclaw-lark",
      "name": "Lark",
      "version": "2026.4.8",
      "source": "global:openclaw-lark/index.js",
      "origin": "global",
      "enabled": true,
      "status": "loaded"
    },
    {
      "id": "stub-no-version",
      "enabled": true,
      "status": "loaded"
    }
  ]
}`

func TestParseOpenclawPluginsJSON_FiltersDisabledAndMissingVersion(t *testing.T) {
	got, err := parseOpenclawPluginsJSON([]byte(pluginsListFixture))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 plugins, got %d: %+v", len(got), got)
	}

	byName := make(map[string]string)
	for _, p := range got {
		byName[p.Name] = p.Version
	}
	if byName["openclaw-channel-dmwork"] != "0.6.3-dev.dc640a2e" {
		t.Errorf("octo plugin wire name/version wrong: %+v", byName)
	}
	if byName["@larksuite/openclaw-lark"] != "2026.4.8" {
		t.Errorf("lark plugin wire name/version wrong: %+v", byName)
	}
	if _, ok := byName["active-memory"]; ok {
		t.Errorf("disabled plugin should be filtered out")
	}
	if _, ok := byName["stub-no-version"]; ok {
		t.Errorf("plugin without version should be filtered out")
	}
}

func TestParseOpenclawPluginsJSON_UsesIDAsName(t *testing.T) {
	// Regression guard for the server/frontend matching invariant:
	// PluginInfo.Name must be the openclaw plugin id (= npm package name),
	// NOT the human-readable display name. Otherwise close-out by name breaks.
	input := `{"plugins":[{"id":"openclaw-channel-dmwork","name":"Display Name","version":"1.0.0","enabled":true}]}`
	got, err := parseOpenclawPluginsJSON([]byte(input))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1, got %d", len(got))
	}
	if got[0].Name != "openclaw-channel-dmwork" {
		t.Errorf("PluginInfo.Name should carry id, got %q", got[0].Name)
	}
}

func TestParseOpenclawPluginsJSON_EmptyPluginsArray(t *testing.T) {
	// "plugins: []" is a legitimate state (nothing enabled on this host).
	// Must not error, must return empty slice.
	got, err := parseOpenclawPluginsJSON([]byte(`{"plugins":[]}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %+v", got)
	}
}

func TestParseOpenclawPluginsJSON_NoPluginsField_Errors(t *testing.T) {
	// Object without plugins key → schema-invalid. Must error so caller
	// falls back to the directory scan (Fix 1: schema drift safety).
	if _, err := parseOpenclawPluginsJSON([]byte(`{"registry":{}}`)); err == nil {
		t.Error("expected error when plugins field missing")
	}
	if _, err := parseOpenclawPluginsJSON([]byte(`{"error":"bad"}`)); err == nil {
		t.Error("expected error on error-shaped object without plugins field")
	}
}

func TestParseOpenclawPluginsJSON_PrefixNoise(t *testing.T) {
	// Future-proof: if stdout ever prefixes log lines before the JSON object.
	input := `[openclaw] booting extensions...
warning: something
{"plugins":[{"id":"openclaw-channel-dmwork","version":"0.6.0","enabled":true}]}`
	got, err := parseOpenclawPluginsJSON([]byte(input))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || got[0].Name != "openclaw-channel-dmwork" {
		t.Errorf("expected octo plugin, got %+v", got)
	}
}

func TestParseOpenclawPluginsJSON_NoisyJSONBeforeReal(t *testing.T) {
	// Regression guard for Fix 2: if a prefix log line contains a self-
	// contained JSON object (e.g. structured log), the scanner must skip
	// it and find the real plugins object that follows.
	input := `{"level":"info","msg":"starting"}
{"trace":"abc","ignored":true}
{"plugins":[{"id":"openclaw-channel-dmwork","version":"0.6.0","enabled":true}]}`
	got, err := parseOpenclawPluginsJSON([]byte(input))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || got[0].Name != "openclaw-channel-dmwork" {
		t.Errorf("expected octo plugin after log noise, got %+v (err=%v)", got, err)
	}
}

func TestParseOpenclawPluginsJSON_TrailingJunkAfterObject(t *testing.T) {
	// Noise after the plugins object (e.g. a trailing log line) should not
	// cause extraction to fail. First candidate wins.
	input := `{"plugins":[{"id":"openclaw-channel-dmwork","version":"0.6.0","enabled":true}]}
bye`
	got, err := parseOpenclawPluginsJSON([]byte(input))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected 1 plugin, got %+v", got)
	}
}

func TestParseOpenclawPluginsJSON_Malformed(t *testing.T) {
	_, err := parseOpenclawPluginsJSON([]byte(`not json at all`))
	if err == nil {
		t.Error("expected error on malformed input")
	}
}
