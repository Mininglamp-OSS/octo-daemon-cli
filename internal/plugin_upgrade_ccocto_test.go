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

// TestRedactSecretBeforeTruncation verifies that when an API key sits across
// the truncation boundary, the composition truncateOutput(redactSecret(...))
// correctly removes ALL fragments of the key. This is a regression test for
// the blocking finding where the old order (truncate then redact) could leak
// partial keys.
func TestRedactSecretBeforeTruncation(t *testing.T) {
	key := "sk-test1234567890abcdef" // 23 chars
	// Build a string where the key sits right at the 800-char boundary
	prefix := strings.Repeat("a", 790) // 790 chars
	middle := key                      // 23 chars, spans 790-813
	suffix := strings.Repeat("b", 100) // padding after
	output := prefix + middle + suffix

	// Correct order: redact first, then truncate
	result := truncateOutput(redactSecret(output, key), 800)

	// The result must NOT contain ANY substring of the key
	if strings.Contains(result, key) {
		t.Fatalf("full key leaked in truncated output: %q", result[len(result)-100:])
	}

	// Check that even fragments >=8 chars of the key don't appear
	for i := 0; i <= len(key)-8; i++ {
		fragment := key[i : i+8]
		if strings.Contains(result, fragment) {
			t.Fatalf("key fragment %q (pos %d) found in truncated output", fragment, i)
		}
	}

	// Verify the output was actually truncated
	if !strings.HasSuffix(result, "...(truncated)") {
		t.Errorf("expected truncation marker, got: %s", result[len(result)-50:])
	}

	// Length should be exactly 800 + len("...(truncated)") = 814
	expectedLen := 800 + len("...(truncated)")
	if len(result) != expectedLen {
		t.Errorf("expected length %d, got %d", expectedLen, len(result))
	}
}

// TestRedactSecretWithEmptyKey verifies redactSecret handles empty secret gracefully.
func TestRedactSecretWithEmptyKey(t *testing.T) {
	input := "some log message without secrets"
	out := redactSecret(input, "")
	if out != input {
		t.Errorf("empty secret should return input unchanged: got %q", out)
	}
}
