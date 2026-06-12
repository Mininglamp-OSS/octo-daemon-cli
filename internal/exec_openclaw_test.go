package internal

import (
	"strings"
	"testing"
)

func TestValidateProvisionID(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"plain dash", "bot-123", false},
		{"underscore", "ws_abc_1", false},
		{"mixed case alnum", "Bot123", false},
		{"max length", strings.Repeat("a", 128), false},
		{"empty", "", true},
		{"dotdot traversal", "../../etc", true},
		{"slash", "a/b", true},
		{"dot segment", "a.b", true},
		{"leading dash flag", "-rf", true},
		{"space", "a b", true},
		{"too long", strings.Repeat("a", 129), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateProvisionID("id", tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateProvisionID(%q) err=%v, wantErr=%v", tt.value, err, tt.wantErr)
			}
		})
	}
}

// TestHandleBotProvisionValidatesIDs locks the A1/A2 ingress guard: both
// server-supplied ids must be validated before any filesystem/CLI/URL use, or
// path traversal and argument injection regress. Source-grep, like the sibling
// regression tests, because *Client is not mockable for a behavioral test.
func TestHandleBotProvisionValidatesIDs(t *testing.T) {
	body := extractFuncBody(t, readSource(t, "exec_openclaw.go"), "handleBotProvision")
	if !strings.Contains(body, `validateProvisionID("workspace_id", cmd.WorkspaceID)`) {
		t.Fatal("handleBotProvision must validate workspace_id at ingress (A1/A2 path traversal + arg injection)")
	}
	if !strings.Contains(body, `validateProvisionID("bot_uid", cmd.BotUID)`) {
		t.Fatal("handleBotProvision must validate bot_uid at ingress (A1/A2 path traversal + arg injection)")
	}
}

// TestHandleBotProvisionRedactsToken locks the C guard: bot_token must be
// scrubbed from the provision-failure error before it reaches the fleet ack
// error_msg (which is persisted and surfaced in the Web UI).
func TestHandleBotProvisionRedactsToken(t *testing.T) {
	body := extractFuncBody(t, readSource(t, "exec_openclaw.go"), "handleBotProvision")
	if !strings.Contains(body, "strings.ReplaceAll(msg, cmd.BotToken,") {
		t.Fatal("handleBotProvision must redact cmd.BotToken from the failure error_msg before ack (C)")
	}
}
