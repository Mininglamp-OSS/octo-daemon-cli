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
