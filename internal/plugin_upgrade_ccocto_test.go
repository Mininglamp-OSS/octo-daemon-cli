package internal

import (
	"strings"
	"testing"
)

func TestCcOctoConfigureArgs(t *testing.T) {
	args := ccOctoConfigureArgs("https://gw")
	want := []string{"configure", "--gateway-url", "https://gw"}
	if len(args) != len(want) {
		t.Fatalf("got %v want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("arg[%d]=%q want %q", i, args[i], want[i])
		}
	}
}

// runtime_id 必须能从 upgrade metadata 解析出来（fleet DispatchUpgrade 透传）。
func TestParseUpgradeRuntimeID(t *testing.T) {
	if got := parseUpgradeRuntimeID(`{"runtime_id":42}`); got != 42 {
		t.Fatalf("got %d want 42", got)
	}
	if got := parseUpgradeRuntimeID(""); got != 0 {
		t.Fatalf("empty metadata should give 0, got %d", got)
	}
	if got := parseUpgradeRuntimeID("not json"); got != 0 {
		t.Fatalf("bad metadata should give 0, got %d", got)
	}
}

// redactSecret 必须脱敏 key（不出现在返回串里）。
func TestRedactSecret(t *testing.T) {
	out := redactSecret("npm err sk-supersecret leaked", "sk-supersecret")
	if strings.Contains(out, "sk-supersecret") {
		t.Fatalf("key not redacted: %q", out)
	}
}
