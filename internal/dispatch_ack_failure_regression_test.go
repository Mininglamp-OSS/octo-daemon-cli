package internal

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Jerry-Xin Critical fix: handler 把 terminal ack/report 失败往上抛 →
// dispatcher 不 markDone, 不 advance cursor → 下次 replay/heartbeat 重试.
//
// 这组 regression test 用 source-grep lock 死 fix 的不变性 (跟 fleet
// PR 那边 F-1/F-2/F-3/F-4 同 pattern, 因为 Daemon.client 是 concrete
// struct 没 interface 化, 无法 mock 真 client 在 unit test 里模拟
// AckBot/ReportUpgrade 失败. source-grep 防 future refactor 静默拆掉
// fix — 如有人把 `handlerErr != nil` 分支去掉, 或者把 handler signature
// 改回 void, test 会 fail loudly.

// readSource 读 internal/<file>.go 全文 (跟 caller 同 dir).
func readSource(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	return string(b)
}

// TestReportUpgradeReturnsError — reportUpgrade signature 必须返 error,
// 否则 handler 拿不到 ReportUpgrade 失败信号.
func TestReportUpgradeReturnsError(t *testing.T) {
	src := readSource(t, "upgrade.go")
	re := regexp.MustCompile(`func \(d \*Daemon\) reportUpgrade\(ctx context\.Context, taskID, status, errMsg string\) error \{`)
	if !re.MatchString(src) {
		t.Fatalf("reportUpgrade signature must be `func (d *Daemon) reportUpgrade(...) error` — Jerry-Xin Critical fix")
	}
	// body 末尾必须 `return err` (失败 propagate) + `return nil` (成功)
	if !strings.Contains(src, "return err") || !strings.Contains(src, "return nil") {
		t.Fatalf("reportUpgrade body must propagate err on failure + return nil on success")
	}
}

// TestAckBotProvisionReturnsError — 同 reportUpgrade.
func TestAckBotProvisionReturnsError(t *testing.T) {
	src := readSource(t, "exec_openclaw.go")
	re := regexp.MustCompile(`func \(d \*Daemon\) ackBotProvision\(ctx context\.Context, cmd \*PendingAgentCommand, status, errMsg string\) error \{`)
	if !re.MatchString(src) {
		t.Fatalf("ackBotProvision signature must be `func (d *Daemon) ackBotProvision(...) error` — Jerry-Xin Critical fix")
	}
}

// TestHandleUpgradeReturnsError — handleUpgrade 必须返 error 让 adapter 透传.
func TestHandleUpgradeReturnsError(t *testing.T) {
	src := readSource(t, "upgrade.go")
	re := regexp.MustCompile(`func \(d \*Daemon\) handleUpgrade\(ctx context\.Context, up \*PendingUpgrade\) error \{`)
	if !re.MatchString(src) {
		t.Fatalf("handleUpgrade signature must return error — Jerry-Xin Critical fix (adapter HandleUpgrade 透传)")
	}
	// handleDaemonUpgrade 同
	re2 := regexp.MustCompile(`func \(d \*Daemon\) handleDaemonUpgrade\(ctx context\.Context, up \*PendingUpgrade\) error \{`)
	if !re2.MatchString(src) {
		t.Fatalf("handleDaemonUpgrade signature must return error")
	}
}

// TestHandleBotProvisionReturnsError — 同.
func TestHandleBotProvisionReturnsError(t *testing.T) {
	src := readSource(t, "exec_openclaw.go")
	re := regexp.MustCompile(`func \(d \*Daemon\) handleBotProvision\(ctx context\.Context, cmd \*PendingAgentCommand\) error \{`)
	if !re.MatchString(src) {
		t.Fatalf("handleBotProvision signature must return error — Jerry-Xin Critical fix")
	}
}

// TestHandleComponentUpgradeReturnsError + TestHandlePluginUpgradeReturnsError.
func TestSubHandlersReturnError(t *testing.T) {
	cases := []struct {
		file, fn string
	}{
		{"component_upgrade.go", `func \(d \*Daemon\) handleComponentUpgrade\(ctx context\.Context, up \*PendingUpgrade\) error \{`},
		{"plugin_upgrade.go", `func \(d \*Daemon\) handlePluginUpgrade\(ctx context\.Context, up \*PendingUpgrade\) error \{`},
	}
	for _, c := range cases {
		src := readSource(t, c.file)
		if !regexp.MustCompile(c.fn).MatchString(src) {
			t.Fatalf("%s: signature must return error — Jerry-Xin Critical fix", c.file)
		}
	}
}

// TestAdaptersPropagateHandlerErr — HandleUpgrade/HandleBotProvision adapter
// 必须 `return d.handleUpgrade(...)` / `return d.handleBotProvision(...)`,
// 不能 `d.handleUpgrade(...); return nil` (老 pattern 会 swallow ack failure).
func TestAdaptersPropagateHandlerErr(t *testing.T) {
	src := readSource(t, "daemon.go")
	if !strings.Contains(src, "return d.handleUpgrade(ctx, up)") {
		t.Fatalf("HandleUpgrade adapter must `return d.handleUpgrade(ctx, up)` to propagate err — Jerry-Xin Critical fix")
	}
	if !strings.Contains(src, "return d.handleBotProvision(ctx, cmd)") {
		t.Fatalf("HandleBotProvision adapter must `return d.handleBotProvision(ctx, cmd)` to propagate err — Jerry-Xin Critical fix")
	}
	// 反向 grep: 不能再有老 pattern (handler 后跟 `return nil`).
	// 老 pattern: `d.handleUpgrade(ctx, up)\n\treturn nil` 或类似.
	// 用 multiline regex 防匹配到合理的 `return nil` (e.g. recover block).
	oldHU := regexp.MustCompile(`(?m)^\s*d\.handleUpgrade\(ctx, up\)\s*\n\s*return nil\s*$`)
	if oldHU.MatchString(src) {
		t.Fatalf("HandleUpgrade adapter still has old pattern `d.handleUpgrade(...) + return nil` — must use `return d.handleUpgrade(...)` to propagate ack failure")
	}
	oldHBP := regexp.MustCompile(`(?m)^\s*d\.handleBotProvision\(ctx, cmd\)\s*\n\s*return nil\s*$`)
	if oldHBP.MatchString(src) {
		t.Fatalf("HandleBotProvision adapter still has old pattern — must `return d.handleBotProvision(...)` to propagate")
	}
}

// TestHeartbeatUpgradeHandlerErrTriggersUnclaim — runHeartbeatUpgrade 必须
// 把 `d.handleUpgrade` 返的 err 存到 named var (`handlerErr`), defer 里
// 必须根据 `handlerErr != nil` 走 unclaim, 否则 ack 失败时仍会 markDone
// (= 老 silent-drop bug).
func TestHeartbeatUpgradeHandlerErrTriggersUnclaim(t *testing.T) {
	src := readSource(t, "daemon.go")

	// 提取 runHeartbeatUpgrade 函数体 (从 `func ... runHeartbeatUpgrade` 到下一个 `^func `).
	body := extractFuncBody(t, src, "runHeartbeatUpgrade")

	// 1) 必须把 handler 返的 err 存到 var (named `handlerErr` 或类似)
	if !regexp.MustCompile(`handlerErr\s*=\s*d\.handleUpgrade\(`).MatchString(body) {
		t.Fatalf("runHeartbeatUpgrade must capture handler return: `handlerErr = d.handleUpgrade(ctx, up)` — Jerry-Xin Critical fix")
	}

	// 2) defer 里必须根据 handlerErr != nil 走 unclaim (而不是 markDone)
	if !regexp.MustCompile(`if handlerErr != nil`).MatchString(body) {
		t.Fatalf("runHeartbeatUpgrade defer must check `if handlerErr != nil` — Jerry-Xin Critical fix")
	}
	if !regexp.MustCompile(`unclaim\(sseEventUpgrade`).MatchString(body) {
		t.Fatalf("runHeartbeatUpgrade defer must unclaim on err path — Jerry-Xin Critical fix")
	}
}

// TestHeartbeatBotProvisionHandlerErrTriggersUnclaim — 同 upgrade.
func TestHeartbeatBotProvisionHandlerErrTriggersUnclaim(t *testing.T) {
	src := readSource(t, "daemon.go")
	body := extractFuncBody(t, src, "runHeartbeatBotProvision")

	if !regexp.MustCompile(`handlerErr\s*=\s*d\.handleBotProvision\(`).MatchString(body) {
		t.Fatalf("runHeartbeatBotProvision must capture handler return: `handlerErr = d.handleBotProvision(...)` — Jerry-Xin Critical fix")
	}
	if !regexp.MustCompile(`if handlerErr != nil`).MatchString(body) {
		t.Fatalf("runHeartbeatBotProvision defer must check `if handlerErr != nil` — Jerry-Xin Critical fix")
	}
	if !regexp.MustCompile(`unclaim\(sseEventBotProvision`).MatchString(body) {
		t.Fatalf("runHeartbeatBotProvision defer must unclaim on err path — Jerry-Xin Critical fix")
	}
}

// TestSSEClientHasReadDeadline — yujiawei P2-1: SSE http.Client 必须有
// 非零 Timeout, 防 silent TCP drop 时 readLoop 卡死直到 ctx cancel.
func TestSSEClientHasReadDeadline(t *testing.T) {
	src := readSource(t, "sse.go")

	// 匹配 `client := &http.Client{Timeout: <value>}`
	// 不能匹配 `client := &http.Client{}` (老 pattern, P2-1 前)
	re := regexp.MustCompile(`client\s*:?=\s*&http\.Client\{\s*Timeout:\s*\d+`)
	if !re.MatchString(src) {
		t.Fatalf("SSE http.Client must set non-zero Timeout — yujiawei P2-1 fix (silent TCP drop self-heal)")
	}

	// 反向: 不能再有 `&http.Client{}` 字面量 (no timeout)
	if regexp.MustCompile(`&http\.Client\{\s*\}`).MatchString(src) {
		t.Fatalf("SSE http.Client must not be `&http.Client{}` (no timeout) — yujiawei P2-1 fix")
	}
}

// extractFuncBody 从 source 里提取指定函数的 body (从 `func ... <name>(` 到
// 下一个 `^func ` 之前). 用于 source-grep test 范围限定到单函数防误抓.
func extractFuncBody(t *testing.T, src, name string) string {
	t.Helper()
	startRe := regexp.MustCompile(`(?m)^func [^\n]*\b` + regexp.QuoteMeta(name) + `\(`)
	loc := startRe.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("func %s not found in source", name)
	}
	rest := src[loc[0]:]
	// 找下一个 `^func ` (新函数开始), 截到它之前.
	nextRe := regexp.MustCompile(`(?m)^func `)
	matches := nextRe.FindAllStringIndex(rest, 2)
	if len(matches) < 2 {
		// 是最后一个函数 — 截到文件末尾
		return rest
	}
	return rest[:matches[1][0]]
}

// assertTerminalCallsReturn 锁: 函数体内每个 `d.<helper>(..., "<terminalStatus>", ...)`
// 调用都必须 `return d.<helper>(...)` 前缀, 不能裸调用 / `_ =` swallow / 后跟无值
// return. N2 强化: 老 swallow pattern `d.reportUpgrade(..., "failed", ...); return`
// 之类的回归会被这个 test loudly fail.
//
// 排除以 // 开头的注释行 (含 `"failed"` 字面量的文档不算).
func assertTerminalCallsReturn(t *testing.T, fnName, helperName, terminalStatus, body string) {
	t.Helper()
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if !strings.Contains(line, `"`+terminalStatus+`"`) {
			continue
		}
		if !strings.Contains(line, "d."+helperName+"(") {
			continue
		}
		if !strings.Contains(line, "return d."+helperName+"(") {
			t.Fatalf("%s line %d: terminal `d.%s(..., %q, ...)` must be `return d.%s(...)` to propagate ack err — Jerry-Xin Critical fix (N2 lock). Line: %q",
				fnName, i+1, helperName, terminalStatus, helperName, line)
		}
	}
}

// TestHandleDaemonUpgrade_TerminalReportPropagates — N2: handleDaemonUpgrade
// 内所有 `d.reportUpgrade(..., "failed", ...)` 必须 `return ...` 前缀.
func TestHandleDaemonUpgrade_TerminalReportPropagates(t *testing.T) {
	src := readSource(t, "upgrade.go")
	body := extractFuncBody(t, src, "handleDaemonUpgrade")
	assertTerminalCallsReturn(t, "handleDaemonUpgrade", "reportUpgrade", "failed", body)
}

// TestHandleComponentUpgrade_TerminalReportPropagates — 同.
func TestHandleComponentUpgrade_TerminalReportPropagates(t *testing.T) {
	src := readSource(t, "component_upgrade.go")
	body := extractFuncBody(t, src, "handleComponentUpgrade")
	assertTerminalCallsReturn(t, "handleComponentUpgrade", "reportUpgrade", "failed", body)
}

// TestHandlePluginUpgrade_TerminalReportPropagates — 同.
func TestHandlePluginUpgrade_TerminalReportPropagates(t *testing.T) {
	src := readSource(t, "plugin_upgrade.go")
	body := extractFuncBody(t, src, "handlePluginUpgrade")
	assertTerminalCallsReturn(t, "handlePluginUpgrade", "reportUpgrade", "failed", body)
}

// TestHandleBotProvision_TerminalAckPropagates — 同, 但 helper 是 ackBotProvision,
// 终态有 "failed" 和 "active" 两种, 都必须 return.
func TestHandleBotProvision_TerminalAckPropagates(t *testing.T) {
	src := readSource(t, "exec_openclaw.go")
	body := extractFuncBody(t, src, "handleBotProvision")
	assertTerminalCallsReturn(t, "handleBotProvision", "ackBotProvision", "failed", body)
	assertTerminalCallsReturn(t, "handleBotProvision", "ackBotProvision", "active", body)
}
