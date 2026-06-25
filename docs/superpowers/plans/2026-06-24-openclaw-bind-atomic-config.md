# openclaw provision 原子写 config 替换全量重启 — 实现 Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 daemon openclaw provision 的「config patch + agents bind + gateway restart」三步，改成「一次原子写 openclaw.json（accounts+bindings）」，消除每建一个 bot 就全量重启 gateway 的副作用。

**Architecture:** 保留 `openclaw agents add`（建 workspace 目录 + 写 agents.*）；新增 `internal/adapter/openclaw_config.go` 实现「定位 config 路径（走 CLIRunner 跑 `openclaw config file`）→ 读 openclaw.json → 内存 upsert account/binding → tmp+rename 原子写」；`Provision` 用它替换 patch+bind，并删除 restartGateway。生效依赖 openclaw 的 reload 机制：一次写盘让 changedPaths 同含 `channels.octo.*` 与 `bindings`，触发非 noop snapshot swap。

**Tech Stack:** Go 1.26，标准库 `encoding/json` / `os` / `strings`；既有 `CLIRunner` 抽象用于单测注入。

## Global Constraints

- 模块路径：`github.com/Mininglamp-OSS/octo-daemon-cli`，包 `package adapter`。
- account 字段集必须与原 `patchOctoAccount` 完全一致：`botToken`、`apiUrl`、`name`(=WorkspaceID)、`requireMention`(=true)。
- binding 结构：`{agentId: <WorkspaceID>, match: {channel: "octo", accountId: <BotUID>}}`；upsert 键 = `match.channel=="octo" && match.accountId==BotUID`；命中只改 agentId，保留其余字段。
- **不写 `session.dmScope`**：与现有 `patchOctoAccount` 行为一致（它不写）；该字段归 create-openclaw-octo / 用户管理，不在本修复范围。已存在的 dmScope 必须原样保留。
- 读后合并，**绝不整体覆盖** openclaw.json；其他 bot / 顶层字段必须原样保留。
- 原子写：在 config 同目录用 `os.CreateTemp` 建唯一临时文件（chmod 0600）写入后 `os.Rename`；不要用固定的 `<path>.tmp`（并发会互踩）。
- 读取用 `json.Decoder` + `UseNumber()`，避免大整数被 coerce 成 float64 改写用户其它配置。
- 读—合并—写全程持进程级 `sync.Mutex`（`openclawConfigMu`），防并发 provision 互相丢配置。
- 不对 account key 做大小写归一。不引入运行态自检。
- OSS 规约：提交 / 注释 / PR 不含 AI 署名、`Co-Authored-By`、review 工具名、流程痕迹。注释只写长期技术理由。
- 提交前自查：`grep -nEi 'codex|Octo-Q|code-review|round [0-9]|Co-Authored' <改动文件>`。

---

## File Structure

- **Create** `internal/adapter/openclaw_config.go` — config 路径解析 + 合并纯函数 + 原子写 + 对外 `writeOctoConfig`。
- **Create** `internal/adapter/openclaw_config_test.go` — 上述各单元的文件级 / 纯函数单测。
- **Modify** `internal/adapter/openclaw.go` — `Provision` 调 `writeOctoConfig` 替换 patch+bind；删 `patchOctoAccount`/`bindBot`/`restartGateway` 及其专用常量。
- **Modify** `internal/adapter/openclaw_test.go` — 更新 `TestOpenclawProvisionRunsAllStepsInOrder`、`TestOpenclawProvisionToleratesAlreadyExistsOnReplay` 以反映新流程（无 patch/bind/restart 三条 CLI 调用）。
- **Modify** `internal/exec_openclaw.go` — 删除已被 `adapter.OpenclawAdapter.Provision` 取代、标注 `Deprecated` 的死代码 `addOpenclawWorkspace`/`patchOctoAccount`/`bindBotToWorkspace`（无调用方）。

---

## Task 1: config 路径解析（解析 `openclaw config file` 输出）

**Files:**
- Create: `internal/adapter/openclaw_config.go`
- Test: `internal/adapter/openclaw_config_test.go`

**Interfaces:**
- Produces: `func parseConfigFilePath(out string) (string, error)` — 从 `openclaw config file` 的多行输出（可能混入 banner / 插件日志）中提取以 `openclaw.json` 结尾的路径行；找不到返回 error。

- [ ] **Step 1: Write the failing test**

```go
package adapter

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseConfigFilePath(t *testing.T) {
	home, _ := os.UserHomeDir()
	cases := []struct {
		name string
		out  string
		want string
	}{
		{"clean", "/Users/x/.openclaw/openclaw.json\n", "/Users/x/.openclaw/openclaw.json"},
		{"with banner", "🦞 OpenClaw 2026.6.6\n[plugins] octo loaded\n/home/u/.openclaw/openclaw.json\n", "/home/u/.openclaw/openclaw.json"},
		{"trailing spaces", "  /tmp/cfg/openclaw.json  \n", "/tmp/cfg/openclaw.json"},
		{"tilde expands to home", "~/.openclaw/openclaw.json\n", filepath.Join(home, ".openclaw", "openclaw.json")},
		{"relative resolves against home", "openclaw.json\n", filepath.Join(home, "openclaw.json")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseConfigFilePath(c.out)
			if err != nil {
				t.Fatalf("parseConfigFilePath: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	if _, err := parseConfigFilePath("no path here\n[plugins] noise\n"); err == nil {
		t.Error("expected error when no openclaw.json line present")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/adapter/ -run TestParseConfigFilePath -v`
Expected: FAIL（`undefined: parseConfigFilePath`，编译错误）

- [ ] **Step 3: Write minimal implementation**

在 `openclaw_config.go`：

```go
package adapter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// parseConfigFilePath extracts the openclaw.json path from `openclaw config
// file` output. openclaw may prepend a banner or plugin log lines to stdout,
// so scan for the line ending in openclaw.json rather than trusting line count.
// The returned path is normalized (~ expanded; a non-absolute path resolved
// against home) so os.ReadFile / os.Rename act on the same file the gateway
// watches.
func parseConfigFilePath(out string) (string, error) {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasSuffix(l, "openclaw.json") {
			return normalizeConfigPath(l), nil
		}
	}
	return "", fmt.Errorf("openclaw config file: no openclaw.json path in output: %q", truncate(out, 200))
}

// normalizeConfigPath expands a leading ~ to the user's home dir, and resolves a
// non-absolute path against home (openclaw runs unix-side here; Windows ~\ is
// out of scope, tracked separately under daemon Windows support). A path that
// can't be resolved (no home) is returned unchanged.
func normalizeConfigPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
		return p
	}
	if !filepath.IsAbs(p) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p)
		}
	}
	return p
}
```

注：`truncate` 现有签名是 `truncate(s string, max int)`（`internal/adapter/common.go:137`），故此处直接传 `out`（string），不要 `[]byte(out)`。

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/adapter/ -run TestParseConfigFilePath -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/openclaw_config.go internal/adapter/openclaw_config_test.go
git commit -m "feat(adapter): parse and normalize openclaw config file path"
```

---

## Task 2: 合并纯函数（account + binding upsert）

**Files:**
- Modify: `internal/adapter/openclaw_config.go`
- Test: `internal/adapter/openclaw_config_test.go`

**Interfaces:**
- Consumes: 无（操作传入的 `map[string]any`）。
- Produces: `func mergeOctoBot(cfg map[string]any, workspaceID, botUID, botToken, apiURL string) map[string]any` — 在 cfg 上 upsert `channels.octo.accounts.<botUID>` 与 `bindings[]`，返回同一 map（就地改 + 返回，便于链式与测试）。nil cfg 视为空 map 新建。**不触碰 `session.dmScope`**。

- [ ] **Step 1: Write the failing test**

```go
func TestMergeOctoBotFresh(t *testing.T) {
	cfg := mergeOctoBot(nil, "ws-1", "bot-abc", "bf_tok", "https://api.x")

	ch := cfg["channels"].(map[string]any)["octo"].(map[string]any)
	acc := ch["accounts"].(map[string]any)["bot-abc"].(map[string]any)
	if acc["botToken"] != "bf_tok" || acc["apiUrl"] != "https://api.x" ||
		acc["name"] != "ws-1" || acc["requireMention"] != true {
		t.Errorf("account fields wrong: %#v", acc)
	}

	binds := cfg["bindings"].([]any)
	if len(binds) != 1 {
		t.Fatalf("bindings len = %d, want 1", len(binds))
	}
	b := binds[0].(map[string]any)
	m := b["match"].(map[string]any)
	if b["agentId"] != "ws-1" || m["channel"] != "octo" || m["accountId"] != "bot-abc" {
		t.Errorf("binding wrong: %#v", b)
	}

	// daemon must NOT introduce session.dmScope (it's a global config owned by
	// create-openclaw-octo / the user, unrelated to #27 routing).
	if _, ok := cfg["session"]; ok {
		t.Errorf("mergeOctoBot must not create session/dmScope, got %#v", cfg["session"])
	}
}

func TestMergeOctoBotPreservesOthersAndUpserts(t *testing.T) {
	cfg := map[string]any{
		"channels": map[string]any{"octo": map[string]any{"accounts": map[string]any{
			"other-bot": map[string]any{"botToken": "x", "name": "other"},
		}}},
		"bindings": []any{
			map[string]any{"agentId": "other", "type": "route",
				"match": map[string]any{"channel": "octo", "accountId": "other-bot"}},
			map[string]any{"agentId": "stale", "type": "route",
				"match": map[string]any{"channel": "octo", "accountId": "bot-abc"}},
		},
		"session":  map[string]any{"dmScope": "some-preexisting-value"},
		"topLevel": "keep-me",
	}

	cfg = mergeOctoBot(cfg, "ws-1", "bot-abc", "bf_tok", "https://api.x")

	// other bot untouched
	accs := cfg["channels"].(map[string]any)["octo"].(map[string]any)["accounts"].(map[string]any)
	if _, ok := accs["other-bot"]; !ok {
		t.Error("other-bot account dropped")
	}
	// top-level preserved
	if cfg["topLevel"] != "keep-me" {
		t.Error("topLevel field dropped")
	}
	// any pre-existing dmScope is left exactly as-is — daemon neither reads,
	// rewrites, nor recommends a value; it only preserves what's there.
	if cfg["session"].(map[string]any)["dmScope"] != "some-preexisting-value" {
		t.Error("existing dmScope altered")
	}
	// binding upserted in place (no duplicate), agentId updated, type preserved
	binds := cfg["bindings"].([]any)
	count, updated := 0, false
	for _, raw := range binds {
		b := raw.(map[string]any)
		m := b["match"].(map[string]any)
		if m["channel"] == "octo" && m["accountId"] == "bot-abc" {
			count++
			if b["agentId"] == "ws-1" && b["type"] == "route" {
				updated = true
			}
		}
	}
	if count != 1 || !updated {
		t.Errorf("upsert wrong: count=%d updated=%v binds=%#v", count, updated, binds)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/adapter/ -run TestMergeOctoBot -v`
Expected: FAIL（`undefined: mergeOctoBot`）

- [ ] **Step 3: Write minimal implementation**

在 `openclaw_config.go` 追加：

```go
// mergeOctoBot upserts one bot's octo account and routing binding into cfg,
// leaving every other key untouched. It returns cfg (mutated in place; a nil
// cfg is treated as a fresh map). account + binding land in the SAME write so
// openclaw's reload plan sees both channels.octo.* and bindings change at once
// and swaps the routing snapshot — a binding written alone is a noop reload and
// never takes effect without a full gateway restart. It deliberately does NOT
// write session.dmScope: that is a global setting owned by create-openclaw-octo
// / the user and unrelated to this routing fix.
func mergeOctoBot(cfg map[string]any, workspaceID, botUID, botToken, apiURL string) map[string]any {
	if cfg == nil {
		cfg = map[string]any{}
	}

	channels := childMap(cfg, "channels")
	octo := childMap(channels, "octo")
	accounts := childMap(octo, "accounts")
	accounts[botUID] = map[string]any{
		"botToken": botToken,
		"apiUrl":   apiURL,
		// name is octo's routing key and must equal the agent name created by
		// `agents add` (= workspaceID), not the user-facing display name, or
		// inbound IM can't route to this agent and falls back to main.
		"name":           workspaceID,
		"requireMention": true,
	}

	cfg["bindings"] = upsertBinding(cfg["bindings"], workspaceID, botUID)

	return cfg
}

// childMap returns parent[key] as a map[string]any, creating it if absent or if
// the existing value is not an object (defensive against hand-edited config).
func childMap(parent map[string]any, key string) map[string]any {
	if m, ok := parent[key].(map[string]any); ok {
		return m
	}
	m := map[string]any{}
	parent[key] = m
	return m
}

// upsertBinding finds the octo binding for botUID and updates its agentId, or
// appends a new one. Existing fields on a matched binding (e.g. "type") are
// preserved. raw is the current cfg["bindings"] value (may be nil or non-slice).
func upsertBinding(raw any, workspaceID, botUID string) []any {
	binds, _ := raw.([]any)
	for _, item := range binds {
		b, ok := item.(map[string]any)
		if !ok {
			continue
		}
		m, ok := b["match"].(map[string]any)
		if ok && m["channel"] == "octo" && m["accountId"] == botUID {
			b["agentId"] = workspaceID
			return binds
		}
	}
	return append(binds, map[string]any{
		"agentId": workspaceID,
		"match":   map[string]any{"channel": "octo", "accountId": botUID},
	})
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/adapter/ -run TestMergeOctoBot -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/openclaw_config.go internal/adapter/openclaw_config_test.go
git commit -m "feat(adapter): merge octo account/binding in one config map"
```

---

## Task 3: 读—改—原子写文件层

**Files:**
- Modify: `internal/adapter/openclaw_config.go`
- Test: `internal/adapter/openclaw_config_test.go`

**Interfaces:**
- Consumes: `mergeOctoBot`（Task 2）。
- Produces: `func mergeAndWriteOctoConfig(path, workspaceID, botUID, botToken, apiURL string) error` — 读 path（不存在按空处理）→ `mergeOctoBot` → 唯一 temp + rename 原子写，全程持 `openclawConfigMu`。供 Task 4 的 `writeOctoConfig` 调用。

- [ ] **Step 1: Write the failing test**

```go
import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

func TestMergeAndWriteOctoConfigAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openclaw.json")

	// pre-existing config with another bot
	seed := map[string]any{
		"channels": map[string]any{"octo": map[string]any{"accounts": map[string]any{
			"old-bot": map[string]any{"botToken": "x", "name": "old"},
		}}},
		"topLevel": "keep",
	}
	raw, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := mergeAndWriteOctoConfig(path, "ws-1", "bot-abc", "bf_tok", "https://api.x"); err != nil {
		t.Fatalf("mergeAndWriteOctoConfig: %v", err)
	}

	// result re-parses and contains both bots + preserved top-level
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result not valid json: %v", err)
	}
	accs := got["channels"].(map[string]any)["octo"].(map[string]any)["accounts"].(map[string]any)
	if _, ok := accs["old-bot"]; !ok {
		t.Error("old-bot dropped")
	}
	if _, ok := accs["bot-abc"]; !ok {
		t.Error("bot-abc not written")
	}
	if got["topLevel"] != "keep" {
		t.Error("topLevel dropped")
	}
	// no temp residue (CreateTemp uses random names; assert none survive)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestMergeAndWriteOctoConfigCreatesWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openclaw.json")
	if err := mergeAndWriteOctoConfig(path, "ws-1", "bot-abc", "bf_tok", "https://api.x"); err != nil {
		t.Fatalf("mergeAndWriteOctoConfig: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config not created: %v", err)
	}
}

// Large integers in unrelated top-level config must survive the round-trip
// verbatim (UseNumber guard) — a plain Unmarshal would rewrite them as floats.
func TestMergeAndWriteOctoConfigPreservesLargeNumbers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openclaw.json")
	if err := os.WriteFile(path, []byte(`{"bigCounter": 1234567890123456789}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mergeAndWriteOctoConfig(path, "ws-1", "bot-abc", "bf_tok", "https://api.x"); err != nil {
		t.Fatalf("mergeAndWriteOctoConfig: %v", err)
	}
	out, _ := os.ReadFile(path)
	if !strings.Contains(string(out), "1234567890123456789") {
		t.Errorf("large integer not preserved verbatim; got: %s", out)
	}
}

// Concurrent provisions of different bots must all survive (mutex guards the
// read-modify-write; without it the slower writer drops the other's account).
func TestMergeAndWriteOctoConfigConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openclaw.json")

	const n = 5
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("bot-%d", i)
			if err := mergeAndWriteOctoConfig(path, "ws-"+id, id, "bf_"+id, "https://api.x"); err != nil {
				t.Errorf("merge %s: %v", id, err)
			}
		}(i)
	}
	wg.Wait()

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result not valid json: %v", err)
	}
	accs := got["channels"].(map[string]any)["octo"].(map[string]any)["accounts"].(map[string]any)
	for i := 0; i < n; i++ {
		if _, ok := accs[fmt.Sprintf("bot-%d", i)]; !ok {
			t.Errorf("bot-%d lost under concurrency", i)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/adapter/ -run TestMergeAndWriteOctoConfig -v`
Expected: FAIL（`undefined: mergeAndWriteOctoConfig`）

- [ ] **Step 3: Write minimal implementation**

在 `openclaw_config.go` 追加（补 import `bytes`、`encoding/json`、`os`、`sync`）：

```go
// openclawConfigMu serializes the read-merge-write cycle against openclaw.json.
// The daemon provisions up to openclawMaxConcurrency (5) bots in parallel; two
// concurrent read-modify-write cycles on the same file would otherwise drop
// whichever account/binding the slower writer never saw. One process-wide mutex
// is correct because a single daemon process owns its openclaw.json.
var openclawConfigMu sync.Mutex

// mergeAndWriteOctoConfig reads openclaw.json at path (absent → empty config),
// upserts the bot via mergeOctoBot, and writes the result back atomically
// (unique temp file + rename) so the gateway's file watcher never observes a
// half-written file. A missing file is created. The whole cycle holds
// openclawConfigMu so concurrent provisions don't clobber each other.
//
// Known boundary (openclaw noop-reload): this writes account + binding together,
// so it never produces a "account present, binding missing" half-state itself.
// If such a half-state exists from outside (legacy daemon / hand-edited config)
// and the account bytes happen to be unchanged, the resulting write touches only
// bindings — which is a noop reload in openclaw and won't take routing effect
// without a manual `openclaw gateway restart`. We do not paper over this with a
// forced dummy change; see design doc §4.
func mergeAndWriteOctoConfig(path, workspaceID, botUID, botToken, apiURL string) error {
	openclawConfigMu.Lock()
	defer openclawConfigMu.Unlock()

	cfg := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read openclaw config %s: %w", path, err)
	}
	if err == nil && len(data) > 0 {
		// UseNumber keeps large integers / exact numeric forms in the user's
		// other config intact through the round-trip (plain Unmarshal would
		// coerce every number to float64 and rewrite e.g. big ints as 1e+18).
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&cfg); err != nil {
			return fmt.Errorf("parse openclaw config %s: %w", path, err)
		}
	}

	cfg = mergeOctoBot(cfg, workspaceID, botUID, botToken, apiURL)

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal openclaw config: %w", err)
	}
	// Unique temp file in the same dir so parallel writers never share a tmp
	// path; rename within the dir is atomic.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".openclaw-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp openclaw config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp openclaw config: %w", err)
	}
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp openclaw config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp openclaw config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename openclaw config: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/adapter/ -run TestMergeAndWriteOctoConfig -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/openclaw_config.go internal/adapter/openclaw_config_test.go
git commit -m "feat(adapter): atomically merge bot into openclaw.json on disk"
```

---

## Task 4: 对外入口 `writeOctoConfig`（串路径解析 + 文件层，走 CLIRunner）

**Files:**
- Modify: `internal/adapter/openclaw_config.go`
- Test: `internal/adapter/openclaw_config_test.go`

**Interfaces:**
- Consumes: `parseConfigFilePath`（Task 1）、`mergeAndWriteOctoConfig`（Task 3）、`CLIRunner`（`runner.go`）。
- Produces: `func writeOctoConfig(ctx context.Context, runner CLIRunner, req ProvisionRequest) error` — 跑 `openclaw config file` 定位路径，再合并落盘。供 Task 5 的 `Provision` 调用。

- [ ] **Step 1: Write the failing test**

复用现有 `recordingRunner`（claude_test.go 已定义于同包）需要能返回自定义 stdout——若 `recordingRunner` 不支持返回值，本测试改用就近内联的 fake。先写期望行为的测试：

```go
// configFileRunner returns a fixed path for `config file` and records calls.
type configFileRunner struct {
	calls    [][]string
	pathLine string
}

func (r *configFileRunner) Run(_ context.Context, name string, args []string, _ []byte) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if len(args) >= 2 && args[0] == "config" && args[1] == "file" {
		return []byte(r.pathLine + "\n"), nil
	}
	return nil, nil
}

func TestWriteOctoConfigResolvesPathAndWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openclaw.json")
	runner := &configFileRunner{pathLine: path}

	err := writeOctoConfig(context.Background(), runner, ProvisionRequest{
		WorkspaceID: "ws-1", BotUID: "bot-abc", BotToken: "bf_tok", APIURL: "https://api.x",
	})
	if err != nil {
		t.Fatalf("writeOctoConfig: %v", err)
	}
	// invoked `openclaw config file`
	if len(runner.calls) == 0 || runner.calls[0][1] != "config" || runner.calls[0][2] != "file" {
		t.Errorf("expected `config file` call, got %v", runner.calls)
	}
	// file written with the bot
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	accs := got["channels"].(map[string]any)["octo"].(map[string]any)["accounts"].(map[string]any)
	if _, ok := accs["bot-abc"]; !ok {
		t.Error("bot not written")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/adapter/ -run TestWriteOctoConfig -v`
Expected: FAIL（`undefined: writeOctoConfig`）

- [ ] **Step 3: Write minimal implementation**

在 `openclaw_config.go` 追加（补 import `context`）：

```go
// writeOctoConfig locates openclaw.json via `openclaw config file`, then merges
// the bot's account + binding into it atomically. It replaces the old
// `config patch` + `agents bind` CLI steps: writing account and binding in one
// file mutation lets openclaw hot-reload pick up the new routing without a full
// gateway restart.
func writeOctoConfig(ctx context.Context, runner CLIRunner, req ProvisionRequest) error {
	cctx, cancel := context.WithTimeout(ctx, openclawConfigTimeout)
	defer cancel()
	out, err := runner.Run(cctx, openclawBin, []string{"config", "file"}, nil)
	if err != nil {
		return fmt.Errorf("openclaw config file: %w (output: %s)", err, truncate(string(out), 800))
	}
	path, err := parseConfigFilePath(string(out))
	if err != nil {
		return err
	}
	if err := mergeAndWriteOctoConfig(path, req.WorkspaceID, req.BotUID, req.BotToken, req.APIURL); err != nil {
		return err
	}
	log.Printf("[DEBUG] [openclaw] wrote octo account+binding for bot_uid=%s into %s", req.BotUID, path)
	return nil
}
```

补 import `log`；在 `openclaw.go` 的常量区或本文件加超时常量：

```go
// openclawConfigTimeout bounds the `openclaw config file` lookup + local
// file rewrite.
const openclawConfigTimeout = 30 * time.Second
```

（放本文件需 import `time`。）

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/adapter/ -run TestWriteOctoConfig -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/openclaw_config.go internal/adapter/openclaw_config_test.go
git commit -m "feat(adapter): add writeOctoConfig entrypoint resolving path via CLI"
```

---

## Task 5: Provision 切换 + 删除旧步骤 + 更新既有测试

**Files:**
- Modify: `internal/adapter/openclaw.go`
- Modify: `internal/adapter/openclaw_test.go`

**Interfaces:**
- Consumes: `writeOctoConfig`（Task 4）。
- Produces: 新 `Provision` 行为（CLI 只剩 `agents add`；config 落盘走 writeOctoConfig；无 gateway restart）。

- [ ] **Step 1: 先改既有测试为新期望（失败的红测）**

把 `openclaw_test.go` 的 `TestOpenclawProvisionRunsAllStepsInOrder` 改为：Provision 只产生 `agents add` 一条 CLI 调用 + 一条 `config file` 调用（用上面 `configFileRunner` 的能力：让 runner 对 `config file` 返回一个临时路径，并断言不再出现 `config patch`/`agents bind`/`gateway restart`）。

```go
func TestOpenclawProvisionRunsAllStepsInOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgPath := filepath.Join(home, "openclaw.json")

	runner := &configFileRunner{pathLine: cfgPath}
	a := NewOpenclawAdapter(runner)
	res, err := a.Provision(context.Background(), ProvisionRequest{
		WorkspaceID: "ws-1", BotUID: "bot-123", BotToken: "bf_secret", APIURL: "https://api.example",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.WorkspaceID != "ws-1" {
		t.Errorf("WorkspaceID = %q, want ws-1", res.WorkspaceID)
	}

	workspace := filepath.Join(home, ".openclaw", "workspaces", "ws-1")
	// only `agents add` (CLI side-effect) + `config file` (path lookup) remain.
	for _, c := range runner.calls {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "config patch") ||
			strings.Contains(joined, "agents bind") ||
			strings.Contains(joined, "gateway restart") {
			t.Errorf("unexpected legacy CLI step: %v", c)
		}
	}
	// exact CLI sequence: agents add (build workspace) THEN config file (locate
	// openclaw.json for the atomic write). No other openclaw subprocesses.
	wantAdd := []string{openclawBin, "agents", "add", "ws-1", "--non-interactive", "--workspace", workspace}
	wantCalls := [][]string{
		wantAdd,
		{openclawBin, "config", "file"},
	}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Errorf("calls =\n%v\nwant\n%v", runner.calls, wantCalls)
	}
	// config landed on disk
	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf("openclaw.json not written: %v", err)
	}
}
```

`TestOpenclawProvisionToleratesAlreadyExistsOnReplay`：去掉对 `agents bind` 的 already-exists 模拟（bind 已不再是 CLI 步骤），保留对 `agents add` 的 already-exists 容忍；让其 runner 也响应 `config file`（返回临时路径）使写盘成功。给 `alreadyExistsRunner` 加 `pathLine` 字段并在 `config file` 时返回。

```go
type alreadyExistsRunner struct {
	calls    [][]string
	armed    bool
	pathLine string
}

func (r *alreadyExistsRunner) Run(_ context.Context, name string, args []string, _ []byte) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if len(args) >= 2 && args[0] == "config" && args[1] == "file" {
		return []byte(r.pathLine + "\n"), nil
	}
	if r.armed && len(args) >= 2 && args[0] == "agents" && args[1] == "add" {
		return []byte("Error: agent already exists"), errors.New("exit status 1")
	}
	return nil, nil
}
```

并在该测试体里给 runner 设置 `pathLine: filepath.Join(home, "openclaw.json")`。确保导入含 `strings`、`os`（若测试文件未导入则补）。

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/adapter/ -run TestOpenclawProvision -v`
Expected: FAIL（旧 `Provision` 仍调 patch/bind/restart，与新断言冲突；或编译错误，取决于改动顺序）

- [ ] **Step 3: 改 Provision 与删除旧方法**

`openclaw.go`：

```go
func (a *OpenclawAdapter) Provision(ctx context.Context, req ProvisionRequest) (ProvisionResult, error) {
	log.Printf("[DEBUG] [openclaw] provision request: %s", debugDumpProvision(req))
	if req.WorkspaceID == "" || req.BotUID == "" {
		return ProvisionResult{}, fmt.Errorf("%w: missing workspace_id/bot_uid", ErrInvalidConfig)
	}
	if err := a.addWorkspace(ctx, req); err != nil {
		return ProvisionResult{}, err
	}
	// Write account + routing binding in a single atomic config mutation so the
	// gateway hot-reloads the new route. Writing the binding separately is a
	// noop reload in openclaw and never takes effect without a full restart;
	// folding both into one write is what lets us avoid restarting the gateway
	// (and dropping every other bot's session) on each new bot.
	if err := writeOctoConfig(ctx, a.runner, req); err != nil {
		return ProvisionResult{}, err
	}
	return ProvisionResult{WorkspaceID: req.WorkspaceID}, nil
}
```

删除方法：`patchOctoAccount`、`bindBot`、`restartGateway`。
删除常量：`openclawPatchTimeout`、`openclawBindTimeout`、`openclawRestartTimeout`（确认无其它引用：`grep -n 'openclawPatchTimeout\|openclawBindTimeout\|openclawRestartTimeout' internal/adapter/*.go`）。
更新 `Provision` 上方文档注释，去掉「3 步 + restart」描述，改为「agents add + 原子写 config」。删除后 `encoding/json` 若仅 `patchOctoAccount` 用过需确认仍被 `debugDumpProvision` 使用（仍在用，保留）。

- [ ] **Step 4: 清理 exec_openclaw.go 的 Deprecated 死代码**

`internal/exec_openclaw.go` 里 `addOpenclawWorkspace`、`patchOctoAccount`、`bindBotToWorkspace` 三个函数均标注 `Deprecated: superseded by adapter.OpenclawAdapter.Provision`，真入口 `handleBotProvision` 走 `runtimeAdapter(...).Provision`，已无调用方。删除这三个函数。

先确认无调用方再删：

```bash
rg -n 'addOpenclawWorkspace|bindBotToWorkspace|\bpatchOctoAccount\b' internal
```

Expected: 仅三个函数各自的定义行（`internal/exec_openclaw.go`）出现，无其它调用点（`internal/adapter/` 里的同名是另一套，本步不碰）。

删除这三个函数后，以下 import 仅被它们使用、会变成 unused，需一并删除：**`os`**（`os.UserHomeDir`）、**`os/exec`**（`exec.CommandContext` ×3）、**`path/filepath`**（`filepath.Join`）。保留 `context`/`encoding/json`/`fmt`/`log`/`strings`/`time` 与 `adapter` import（仍被 `handleBotProvision`/`ackBotProvision`/`debugDumpCommand` 使用）。

同时更新 `handleBotProvision` 顶部注释（若仍描述旧的 add→patch→bind 三步），改为「经 runtimeAdapter 分发到对应 adapter.Provision」。

`go build ./...` 会对残留 unused import 报错，作为删干净的校验。

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/... -v`
Expected: PASS（adapter 包 + internal 包均通过）
Run: `go build ./... && go vet ./...`
Expected: 无错误（含 exec_openclaw.go 删函数后无 unused import / dead ref）

- [ ] **Step 6: Commit**

```bash
git add internal/adapter/openclaw.go internal/adapter/openclaw_test.go internal/exec_openclaw.go
git commit -m "feat(adapter): provision openclaw bots via atomic config write, drop gateway restart"
```

---

## Task 6: 全量校验 + provenance 自查

**Files:** 无新增改动，仅校验。

- [ ] **Step 1: 跑全仓测试 + 构建 + vet**

Run:
```bash
go build ./...
go test ./...
go vet ./...
```
Expected: 全 PASS / 无输出错误。

- [ ] **Step 2: provenance 痕迹自查（OSS 硬规则）**

Run:
```bash
git diff main...HEAD --name-only | xargs grep -nEi 'codex|Octo-Q|code-review|round [0-9]|cc\+|Co-Authored|Generated with|Claude' 2>/dev/null
```
Expected: 无输出（命中则清理对应注释 / commit message）。

- [ ] **Step 3: 确认改动范围**

Run: `git diff main...HEAD --stat`
Expected: 仅 `internal/adapter/openclaw.go`、`internal/adapter/openclaw_config.go`、`internal/adapter/openclaw_config_test.go`、`internal/adapter/openclaw_test.go`、`internal/exec_openclaw.go` 五个文件。

---

## 后续（不在本 plan 的编码范围，进 Qflow ④/⑤）

- 本地真环境验证（design doc §6 标准）：web 建 bot → 日志 `config change detected (channels.octo.accounts.X, bindings)` 同批 + 无 `shutdown started: gateway restarting` + 私聊不掉 main。
- 还原 stash 的 `CC_OCTO_LOCAL_PATH` 测试 hook（`git stash pop`），勿带入本分支提交。
- finishing-a-development-branch：PR 合回（目标分支按 daemon 基线 = main）。

---

## Self-Review

**1. Spec coverage**（对照 design doc）：
- §2 新单元 path 解析 → Task 1 ✓；合并 upsert → Task 2 ✓；原子写 → Task 3 ✓；config file 入口 → Task 4 ✓。
- §2 Provision 流程变更 + 删 patch/bind/restart → Task 5 ✓。
- §4 幂等 / 并发 / 数值保真：replay（Task 5 `...ToleratesAlreadyExists`）✓；并发不丢配置（Task 3 `...Concurrent` + mutex）✓；大整数保真（Task 3 `...PreservesLargeNumbers` + UseNumber）✓；外部遗留半成品边界 → design §4 文档化、不在编码范围 ✓。
- §5 测试清单：空 config 新建（Task 3 CreatesWhenMissing）✓、多 bot 合并不丢（Task 2/3）✓、binding upsert 幂等（Task 2 Upserts）✓、不触碰 dmScope（Task 2 Fresh 断言无 session + Upserts 断言已有值不变）✓、原子写无 .tmp 残留（Task 3 Atomic）✓、路径解析过滤噪音 + ~ 归一（Task 1）✓、account 字段一致（Task 2 Fresh）✓、并发安全（Task 3 Concurrent）✓、数值保真（Task 3 PreservesLargeNumbers）✓。
- §6 验证标准 → 列入「后续」（属 ④ 真环境验证）✓。
- Global Constraints provenance 自查 → Task 6 ✓。

**2. Placeholder scan:** 各 code step 均为完整可粘贴代码，无 TBD/TODO/"similar to"。✓

**3. Type consistency:** `parseConfigFilePath(string)→(string,error)`、`normalizeConfigPath(string)→string`、`mergeOctoBot(map,4×string)→map`、`childMap`/`upsertBinding` 辅助、`mergeAndWriteOctoConfig(5×string)→error`（持 `openclawConfigMu sync.Mutex`）、`writeOctoConfig(ctx,CLIRunner,ProvisionRequest)→error` 在 Task 1→4 定义、Task 4/5 引用一致。`configFileRunner` 在 Task 4 定义、Task 5 复用。`truncate(s string, max int)` 沿用 `internal/adapter/common.go:137` 既有签名——所有调用点均传 string（`truncate(out, 200)` 处 out 已是 string；`truncate(string(out), 800)` 处显式转换）。✓
