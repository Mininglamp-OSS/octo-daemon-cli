package internal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestExtractJSONArray_PrettyPrinted(t *testing.T) {
	input := `[dmwork] registering before_prompt_build hook
[
  {
    "id": "main",
    "bindings": 10,
    "isDefault": true
  }
]
[dmwork] registering before_prompt_build hook`

	result := extractJSONArray([]byte(input))
	if result == nil {
		t.Fatal("expected JSON array, got nil")
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(result, &arr); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(arr) != 1 {
		t.Fatalf("expected 1 element, got %d", len(arr))
	}
}

func TestExtractJSONArray_SingleLine(t *testing.T) {
	input := `[plugins] loading...
[{"id":"main","bindings":2,"isDefault":true}]
done`

	result := extractJSONArray([]byte(input))
	if result == nil {
		t.Fatal("expected JSON array, got nil")
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(result, &arr); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(arr) != 1 {
		t.Fatalf("expected 1 element, got %d", len(arr))
	}
}

func TestExtractJSONArray_CleanJSON(t *testing.T) {
	input := `[{"id":"main"},{"id":"test"}]`

	result := extractJSONArray([]byte(input))
	if result == nil {
		t.Fatal("expected JSON array, got nil")
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(result, &arr); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 elements, got %d", len(arr))
	}
}

func TestExtractJSONArray_Empty(t *testing.T) {
	result := extractJSONArray([]byte("no json here"))
	if result != nil {
		t.Fatalf("expected nil, got %s", string(result))
	}
}

func TestExtractJSONArray_PrefixWithBracket(t *testing.T) {
	input := `[plugins] octo loaded
[dmwork] hook registered
[
  {"id": "main", "bindings": 5, "isDefault": true}
]`

	result := extractJSONArray([]byte(input))
	if result == nil {
		t.Fatal("expected JSON array, got nil")
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(result, &arr); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
}

// writeFakeBin 写一个 shell 脚本到 dir 模拟一个名叫 binName 的 binary,
// 拿到任何 args 都打印 stdoutLine 然后退出 0. 用来给 isCcChannelOctoRunning
// 单测在 PATH 里塞控制好的输出.
func writeFakeBin(t *testing.T, dir, binName, stdoutLine string) {
	t.Helper()
	path := filepath.Join(dir, binName)
	script := "#!/bin/sh\nprintf '%s\\n' " + shellQuote(stdoutLine) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bin: %v", err)
	}
}

func shellQuote(s string) string {
	// 单引号包裹后, 把内部 ' 替换成 '"'"' (闭引号 + 双引引内单引 + 重开).
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func TestIsCcChannelOctoRunning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-shadow trick relies on POSIX shell")
	}
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{"running with pid", "cc-channel-octo: running (pid 1306), logs at /tmp/g.log", true},
		{"stopped", "cc-channel-octo: stopped", false},
		{"not running variant", "cc-channel-octo: not running", false},
		{"empty output", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFakeBin(t, dir, "cc-channel-octo", tc.out)
			origPath := os.Getenv("PATH")
			t.Setenv("PATH", dir+string(os.PathListSeparator)+origPath)

			got := isCcChannelOctoRunning()
			if got != tc.want {
				t.Fatalf("isCcChannelOctoRunning() got=%v want=%v (output=%q)", got, tc.want, tc.out)
			}
		})
	}
}

func TestIsCcChannelOctoRunning_NotInPath(t *testing.T) {
	// 空 PATH → LookPath 失败 → 返 false (= 没装 cc-channel-octo = offline).
	t.Setenv("PATH", "")
	if got := isCcChannelOctoRunning(); got {
		t.Fatalf("expected false when binary not in PATH, got true")
	}
}

// findCcOctoPlugin returns the cc-octo plugin entry from a runtime's Plugins, or nil.
func findCcOctoPlugin(r RuntimeInfo) *PluginInfo {
	for i := range r.Plugins {
		if r.Plugins[i].Name == "cc-octo" {
			return &r.Plugins[i]
		}
	}
	return nil
}

func TestEnrichClaudeRuntime_AddsCcOctoPlugin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-shadow trick relies on POSIX shell")
	}
	dir := t.TempDir()
	writeFakeBin(t, dir, "cc-channel-octo", "1.2.3")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Offline on purpose: cc-octo version must be reported regardless of gateway
	// liveness, so the upgrade entry survives an offline gateway.
	in := []RuntimeInfo{{Provider: "claude", Status: "offline"}}
	out := EnrichClaudeRuntime(in)
	p := findCcOctoPlugin(out[0])
	if p == nil {
		t.Fatalf("expected cc-octo plugin, got Plugins=%v", out[0].Plugins)
	}
	if p.Version != "1.2.3" {
		t.Fatalf("cc-octo version got=%q want=1.2.3", p.Version)
	}
}

func TestEnrichClaudeRuntime_StripsVPrefixAndText(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-shadow trick relies on POSIX shell")
	}
	for _, out := range []string{"v1.2.3", "cc-channel-octo 1.2.3"} {
		dir := t.TempDir()
		writeFakeBin(t, dir, "cc-channel-octo", out)
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		got := EnrichClaudeRuntime([]RuntimeInfo{{Provider: "claude", Status: "online"}})
		p := findCcOctoPlugin(got[0])
		if p == nil || p.Version != "1.2.3" {
			t.Fatalf("output %q → got %+v, want version 1.2.3", out, p)
		}
	}
}

func TestEnrichClaudeRuntime_SkipsNonClaude(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-shadow trick relies on POSIX shell")
	}
	dir := t.TempDir()
	writeFakeBin(t, dir, "cc-channel-octo", "1.2.3")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out := EnrichClaudeRuntime([]RuntimeInfo{{Provider: "openclaw", Status: "online"}})
	if p := findCcOctoPlugin(out[0]); p != nil {
		t.Fatalf("openclaw runtime must not get a cc-octo plugin, got %+v", p)
	}
}

func TestEnrichClaudeRuntime_MissingBinary_NoPlugin(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no cc-channel-octo on PATH
	out := EnrichClaudeRuntime([]RuntimeInfo{{Provider: "claude", Status: "online"}})
	if p := findCcOctoPlugin(out[0]); p != nil {
		t.Fatalf("missing cc-channel-octo must yield no cc-octo plugin, got %+v", p)
	}
}
