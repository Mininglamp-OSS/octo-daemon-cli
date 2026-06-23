# cc 一键安装到可聊天 3 缺口修复 — 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让全新用户在 Runtimes 页一键装好 cc 插件后,仅靠 UI 走通「装完→上线→建bot→聊天」,并把网关 URL / 模型做成对不同网关通用的安装输入。

**Architecture:** 三缺口落在 4 仓:cc-channel-octo(gateway 0-bot idle 常驻 + URL 规范化 + `configure --model`)、octo-fleet(model relay + `/v1/models` 代理)、octo-daemon-cli(装后拉起 gateway + 删模型硬写 + 透传 model)、octo-web(URL 默认 + 模型动态下拉)。`model` 字段全链路可选、向后兼容。

**Tech Stack:** TypeScript/vitest(cc, web)、Go/go test(daemon, fleet)、React。

## Global Constraints

- 代码 / commit / PR 无 provenance 痕迹:不带 AI 署名、`Co-Authored-By`、review 工具名、评审轮次 / finding 代号。provider/model 名(`vertexai/claude-opus-4-8`、`claude`)是正当数据。
- 语言:cc + daemon + fleet + web 代码/注释/commit 全英文,Conventional Commits(`feat:`/`fix:`/`test:`)。
- `model` 全链路可选 + 向后兼容:web→fleet→daemon→cc 任一环未提供 → 退回 cc SDK 默认,不硬失败。
- OSS 不烤死部署专属值:开源前端不硬编码 mlamp 网关/模型;默认网关走 `import.meta.env.VITE_OCTO_DEFAULT_GATEWAY_URL`(OSS 默认空)。
- SSRF 分层:install URL 给用户机 daemon 消费,复用 `isAllowedGatewayURL` 快检(有意放行 localhost);**fleet 服务端主动外呼**(models 代理)用更严策略——拨号时校验解析 IP、拒 localhost/私网、禁 redirect(`newSafeProxyClient`),不复用 install 宽松检查。
- dev 阶段:本地跑通即可,逻辑完整,不加生产防御。
- 分支基线:cc/daemon/fleet base `main`;web base `feat/agent-runtime`。各仓独立 PR。
- 跨仓上线顺序:cc → fleet → daemon → web(daemon `configure --model` 依赖 cc 先支持该 flag)。

---

## 文件结构

| 仓库 | 文件 | 责任 |
|---|---|---|
| cc | `src/config.ts` | `resolveBotConfigs` 0-bot 返 `[]` |
| cc | `src/index.ts` | `main()` 0-bot idle 常驻分支 |
| cc | `src/configure.ts` | strip 结尾 `/v1` + 写 `sdk.model`(省略则保留)+ 写顶层 `apiUrl` |
| cc | `src/cli.ts` | parseArgs `--model` / `--api-url` + run 传参 |
| fleet | `modules/runtime/cc_octo_secret.go` | secret 加 `Model` |
| fleet | `modules/runtime/cc_octo_fetch.go` | 响应回 `model` |
| fleet | `modules/runtime/upgrade.go` + `model.go` | install 请求收 + stash `model` |
| fleet | `modules/runtime/llm_models.go`(新) | `/v1/models` 代理端点 + 拨号时 IP 校验严格 SSRF(`isUnsafeIP`/`newSafeProxyClient`) |
| fleet | `modules/runtime/api.go` | 挂代理路由 |
| daemon | `internal/client.go` | `CcOctoConfig` 加 `Model` |
| daemon | `internal/plugin_upgrade.go` | `ccOctoConfigureArgs(url,model,apiURL)` + `ccOctoStartArgs()` + 装后 start |
| daemon | `internal/adapter/claude.go` | 删 per-bot 模型硬写 |
| web | `ccInstallValidate.ts` | `normalizeGatewayUrl` strip `/v1` |
| web | `CcInstallModal.tsx` | URL 默认 + 模型 combobox + 拉取 |
| web | `Runtimes/index.tsx` | secret/onSubmit 串 `model` |
| web | `i18n/locales/{en-US,zh-CN}.json` | 文案 |

---

## cc-channel-octo(base `main`)

### Task 1: #1 — resolveBotConfigs 0-bot 返回 `[]`

**Files:**
- Modify: `src/config.ts`(`resolveBotConfigs`,约 :497-503)
- Test: `src/__tests__/config.test.ts`

**Interfaces:**
- Produces: `resolveBotConfigs(config: Config): Config[]` — 无 `bots[]`、无全局 `botToken`、且 `<baseDir>/default/config.json` 也无 `botToken` 时返回 `[]`;否则仍返回对应 bot(全局 token 或 default per-bot 文件的兼容路径不变)。

- [ ] **Step 1: 写失败测试**(追加到 `config.test.ts`)

```ts
describe('resolveBotConfigs zero-bot idle', () => {
  it('returns [] when no bots[] and no global botToken', () => {
    // loadConfig does NOT require botToken (see existing test at this file);
    // a config with apiUrl but no token represents a freshly-installed gateway.
    const path = writeConfig({ apiUrl: 'https://api.test' });
    const cfg = loadConfig(path);
    expect(resolveBotConfigs(cfg)).toEqual([]);
  });

  it('still returns one default bot when a global botToken is set', () => {
    const path = writeConfig({ apiUrl: 'https://api.test', botToken: 'bf_legacy' });
    const cfg = loadConfig(path);
    const bots = resolveBotConfigs(cfg);
    expect(bots).toHaveLength(1);
    expect(bots[0].botId).toBe('default');
    expect(bots[0].botToken).toBe('bf_legacy');
  });

  it('still starts the default bot when only <baseDir>/default/config.json has a token', () => {
    // No global botToken, no bots[] — but a legacy single bot keeps its token in
    // the default per-bot file. Must NOT idle (return []), must start that bot.
    const path = writeConfig({ apiUrl: 'https://api.test' });
    mkdirSync(join(tmpDir, 'default'), { recursive: true });
    writeFileSync(join(tmpDir, 'default', 'config.json'), JSON.stringify({ botToken: 'bf_perbot' }));
    const cfg = loadConfig(path);
    const bots = resolveBotConfigs(cfg);
    expect(bots).toHaveLength(1);
    expect(bots[0].botToken).toBe('bf_perbot');
  });
});
```

> `mkdirSync`/`writeFileSync`/`join` 若未在 `config.test.ts` 顶部导入则补上;`tmpDir`/`writeConfig` 是本文件既有夹具(见现有 `loadConfig defaults` 测试)。

- [ ] **Step 2: 跑测试确认失败**

Run: `npx vitest run src/__tests__/config.test.ts -t "zero-bot idle"`
Expected: 第一个用例 FAIL —— 现在 0-bot 会 throw `missing botToken`(而非返 `[]`)。

- [ ] **Step 3: 改实现**

`src/config.ts` `resolveBotConfigs` 开头(替换现有 `const entries` 三元):

```ts
export function resolveBotConfigs(config: Config): Config[] {
  const hasInlineBots = !!(config.bots && config.bots.length > 0);
  // Zero-bot idle: a freshly-installed gateway has neither a bots[] list nor a
  // global botToken. BUT a legacy single-bot may carry its token only in
  // <baseDir>/default/config.json (the synthesized "default" entry reads it).
  // Only idle (return []) when there is ALSO no token in that per-bot file —
  // otherwise we'd silently stop a validly-configured single bot.
  if (!hasInlineBots && !config.botToken) {
    const defaultPerBot = readConfigFile(pathJoin(config.baseDir, 'default', 'config.json'));
    if (!defaultPerBot.botToken) {
      return [];
    }
  }
  // Single-bot back-compat: synthesize one "default" entry; its token comes from
  // the global botToken or, failing that, <baseDir>/default/config.json (read in
  // the loop below as perBotFile.botToken).
  const entries: BotOverride[] = hasInlineBots
    ? config.bots!
    : [{ id: 'default', botToken: config.botToken || undefined }];
  // ... rest unchanged (seenIds/seenTokens loop) ...
```

> 注:`readConfigFile` / `pathJoin` 已在本文件内使用(见 `resolveBotConfigs` 现有 per-bot 读取),直接复用,无新依赖。

- [ ] **Step 4: 跑测试确认通过**

Run: `npx vitest run src/__tests__/config.test.ts && npm run type-check`
Expected: PASS。

- [ ] **Step 5: commit**

```bash
git add src/config.ts src/__tests__/config.test.ts
git commit -m "feat(config): resolve zero-bot config to empty list for idle startup"
```

### Task 2: #1 — index.ts main() 0-bot idle 常驻

**Files:**
- Modify: `src/index.ts`(`main()`,`const botConfigs = resolveBotConfigs(config)` 之后,约 :47)
- Test: `src/__tests__/index-idle.test.ts`(新)

**Interfaces:**
- Consumes: `resolveBotConfigs` 返 `[]`(Task 1)。
- Produces: `main()` 在 0-bot 时调用 idle 分支,进程保活;导出纯函数 `shouldRunIdle(botConfigs: { botId?: string }[]): boolean` 供测试。

- [ ] **Step 1: 写失败测试**(`src/__tests__/index-idle.test.ts`)

```ts
import { describe, it, expect } from 'vitest';
import { shouldRunIdle } from '../index.js';

describe('shouldRunIdle', () => {
  it('true for empty bot list', () => {
    expect(shouldRunIdle([])).toBe(true);
  });
  it('false when at least one bot', () => {
    expect(shouldRunIdle([{ botId: 'default' }])).toBe(false);
  });
});
```

- [ ] **Step 2: 跑测试确认失败**

Run: `npx vitest run src/__tests__/index-idle.test.ts`
Expected: FAIL —— `shouldRunIdle` is not exported / not defined。

- [ ] **Step 3: 改实现**

`src/index.ts` 顶部附近导出纯函数:

```ts
/** True when there are no bots to run — the gateway should idle (stay alive,
 *  online, no sockets) until a bot is provisioned. Pure for unit testing. */
export function shouldRunIdle(botConfigs: { botId?: string }[]): boolean {
  return botConfigs.length === 0;
}
```

在 `main()` 里 `const botConfigs = resolveBotConfigs(config);` 之后、`const multi = ...` 之前插入 idle 分支:

```ts
  if (shouldRunIdle(botConfigs)) {
    console.log('[cc-channel-octo] idle (no bots configured) — awaiting provision');
    // A pending Promise alone does NOT keep Node's event loop alive — the
    // process would exit immediately and the supervisor would report "stopped"
    // (claude offline). Hold an explicit ref'd timer as keepalive; clear it on
    // signal so shutdown is clean. The first provision runs
    // `cc-channel-octo restart`, respawning this process with a non-empty bots list.
    await new Promise<void>((resolve) => {
      const keepalive = setInterval(() => {}, 60_000); // ref'd (not unref'd) → keeps loop alive
      const bye = (sig: string) => {
        console.log(`[cc-channel-octo] Received ${sig}, idle shutdown`);
        clearInterval(keepalive);
        resolve();
      };
      process.once('SIGINT', () => bye('SIGINT'));
      process.once('SIGTERM', () => bye('SIGTERM'));
    });
    return;
  }
```

> idle 保活靠**未 unref 的 `setInterval`**(ref'd handle 才能撑住事件循环),不能只靠 pending Promise + 信号 listener。Step 5 手测须确认进程**真的不退出**(`sleep 2; jobs` 仍在)。

- [ ] **Step 4: 跑测试确认通过**

Run: `npx vitest run src/__tests__/index-idle.test.ts && npm run type-check`
Expected: PASS。

- [ ] **Step 5: 手动集成验证**(idle 进程级行为,不入单测)

```bash
# 临时空配置(无 bots/无 token,只 apiUrl)
mkdir -p /tmp/cc-idle && printf '{"apiUrl":"https://api.test"}\n' > /tmp/cc-idle/config.json
npm run build
node dist/index.js &   # 用 baseDir=/tmp/cc-idle 需设 DEFAULT_CONFIG_PATH/或 supervisor;此处验证进程不退出
sleep 2; jobs    # 期望进程仍在(未因 no bots 退出);打印 "idle (no bots configured)"
kill %1
```
Expected: 进程不立即退出,日志含 `idle (no bots configured)`。

- [ ] **Step 6: commit**

```bash
git add src/index.ts src/__tests__/index-idle.test.ts
git commit -m "feat(gateway): idle startup when no bots configured (stay online for provision)"
```

### Task 3: #2 — configure.ts strip 结尾 `/v1`

**Files:**
- Modify: `src/configure.ts`(`configure()`,写 `anthropicBaseUrl` 处)
- Test: `src/__tests__/configure.test.ts`(若无则新建)

**Interfaces:**
- Produces: `configure()` 写入 `sdk.anthropicBaseUrl` 前 strip 结尾 `/v1` 或 `/v1/`;新增导出纯函数 `normalizeGatewayUrl(raw: string): string`。

- [ ] **Step 1: 写失败测试**

```ts
import { describe, it, expect } from 'vitest';
import { normalizeGatewayUrl } from '../configure.js';

describe('normalizeGatewayUrl', () => {
  it('strips a trailing /v1', () => {
    expect(normalizeGatewayUrl('https://gw.test/v1')).toBe('https://gw.test');
  });
  it('strips a trailing /v1/', () => {
    expect(normalizeGatewayUrl('https://gw.test/v1/')).toBe('https://gw.test');
  });
  it('leaves a non-version path intact', () => {
    expect(normalizeGatewayUrl('https://gw.test/api')).toBe('https://gw.test/api');
  });
  it('leaves a bare host intact', () => {
    expect(normalizeGatewayUrl('https://gw.test')).toBe('https://gw.test');
  });
  it('does not strip a mid-path v1', () => {
    expect(normalizeGatewayUrl('https://gw.test/v1/foo')).toBe('https://gw.test/v1/foo');
  });
});
```

- [ ] **Step 2: 跑测试确认失败**

Run: `npx vitest run src/__tests__/configure.test.ts`
Expected: FAIL —— `normalizeGatewayUrl` 未导出。

- [ ] **Step 3: 改实现**

`src/configure.ts` 加导出函数,并在 `configure()` 写入前调用:

```ts
/** The Anthropic SDK appends `/v1/messages` to ANTHROPIC_BASE_URL. A gateway
 *  pasted with a trailing `/v1` would yield `/v1/v1/messages` (404). Strip a
 *  trailing `/v1` (optionally with a slash) so the base is the host root.
 *  Pure for unit testing. */
export function normalizeGatewayUrl(raw: string): string {
  return raw.replace(/\/v1\/?$/, '');
}
```

在 `configure()` 内,`isAllowedApiUrl(gatewayUrl)` 校验**之后**、构造 `merged` 之前:

```ts
  const normalizedUrl = normalizeGatewayUrl(gatewayUrl);
```
并把 `merged.sdk.anthropicBaseUrl` 由 `gatewayUrl` 改为 `normalizedUrl`。

- [ ] **Step 4: 跑测试确认通过**

Run: `npx vitest run src/__tests__/configure.test.ts && npm run type-check`
Expected: PASS。

- [ ] **Step 5: commit**

```bash
git add src/configure.ts src/__tests__/configure.test.ts
git commit -m "fix(configure): strip trailing /v1 from gateway url to avoid /v1/v1 path"
```

### Task 4: #3 + #1 — configure 支持 `--model` 与 `--api-url`

**Files:**
- Modify: `src/cli.ts`(`ParsedArgs`、`parseArgs`、`run` 的 `configure` case、usage)
- Modify: `src/configure.ts`(`configure()` 增 options:写 `sdk.model` / 顶层 `apiUrl`)
- Test: `src/__tests__/configure.test.ts`、`src/__tests__/cli.test.ts`

**为什么加 `--api-url`(关键)**:cc `loadConfig()`(`config.ts:408`)**必填**顶层 `apiUrl`,空则 `throw "Missing required config: apiUrl"`。而 `configure` 原来只写 `sdk.*`,不写 `apiUrl` → 装完(仅跑 configure)后 0-bot idle gateway 一 `loadConfig()` 就崩,到不了 idle 分支。daemon 知道 Octo IM server URL(`d.cfg.ServerURL`),装时经 `--api-url` 一并写入(Task 7 传),idle gateway 才能启动。`apiUrl` 是顶层字段(非 `sdk` 下)。

**Interfaces:**
- Consumes: `normalizeGatewayUrl`(Task 3)。
- Produces:
  - `configure(gatewayUrl: string, apiKey: string, configPath?: string, opts?: { model?: string; apiUrl?: string }): void`
    - `opts.model` 非空 → 写 `sdk.model`;**省略/空 → 保留既有 `sdk.model`**(只在显式给值时改;手工 `configure` 只换 key 不会丢 model。「重置成默认」当 YAGNI 砍掉,要的话以后单加 `--clear-model`)。
    - `opts.apiUrl` 非空 → 写顶层 `config.apiUrl`(并复用 `isAllowedApiUrl` 校验);省略 → 不动既有 `apiUrl`。
  - `parseArgs` 解析 `--model <v>`/`--model=v` → `ParsedArgs.model`;`--api-url <v>`/`--api-url=v` → `ParsedArgs.apiUrl`。

- [ ] **Step 1: 写失败测试**

```ts
// configure.test.ts —— 读回写出的 JSON
import { readFileSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { configure } from '../configure.js';

it('configure writes sdk.model and normalizes url when model provided', () => {
  const dir = mkdtempSync(join(tmpdir(), 'cc-cfg-'));
  const path = join(dir, 'config.json');
  configure('https://gw.test/v1', 'sk-test', path, { model: 'vertexai/claude-opus-4-8' });
  const cfg = JSON.parse(readFileSync(path, 'utf-8'));
  expect(cfg.sdk.anthropicBaseUrl).toBe('https://gw.test');
  expect(cfg.sdk.apiKey).toBe('sk-test');
  expect(cfg.sdk.model).toBe('vertexai/claude-opus-4-8');
});

it('configure PRESERVES a prior sdk.model when model omitted', () => {
  const dir = mkdtempSync(join(tmpdir(), 'cc-cfg-'));
  const path = join(dir, 'config.json');
  writeFileSync(path, JSON.stringify({ sdk: { model: 'old/model' } }));
  configure('https://gw.test', 'sk-test', path); // no model → keep existing
  const cfg = JSON.parse(readFileSync(path, 'utf-8'));
  expect(cfg.sdk.model).toBe('old/model');
});

it('configure writes top-level apiUrl when provided', () => {
  const dir = mkdtempSync(join(tmpdir(), 'cc-cfg-'));
  const path = join(dir, 'config.json');
  configure('https://gw.test', 'sk-test', path, { apiUrl: 'http://127.0.0.1:8090' });
  const cfg = JSON.parse(readFileSync(path, 'utf-8'));
  expect(cfg.apiUrl).toBe('http://127.0.0.1:8090');
});
```

```ts
// cli.test.ts (parseArgs)
import { parseArgs } from '../cli.js';
it('parses --model and --api-url flags', () => {
  expect(parseArgs(['configure', '--model', 'm1']).model).toBe('m1');
  expect(parseArgs(['configure', '--model=m2']).model).toBe('m2');
  expect(parseArgs(['configure', '--api-url', 'http://127.0.0.1:8090']).apiUrl).toBe('http://127.0.0.1:8090');
});
```

- [ ] **Step 2: 跑测试确认失败**

Run: `npx vitest run src/__tests__/configure.test.ts src/__tests__/cli.test.ts`
Expected: FAIL —— `configure` 不接 opts / 无 apiUrl-clear-model 行为 / `parseArgs` 无 `model`/`apiUrl`。

- [ ] **Step 3: 改实现**

`src/configure.ts`(签名换成 options;`isAllowedApiUrl` 已导入):
```ts
export function configure(
  gatewayUrl: string, apiKey: string, configPath?: string,
  opts?: { model?: string; apiUrl?: string },
): void {
  // ... existing gatewayUrl/apiKey checks ...
  if (opts?.apiUrl && !isAllowedApiUrl(opts.apiUrl)) {
    throw new Error(`configure: unsafe --api-url ${opts.apiUrl} (must be https:// or http://localhost)`);
  }
  const normalizedUrl = normalizeGatewayUrl(gatewayUrl);   // Task 3
  // ... read+narrow existing / existingSdk ...
  const sdk: Record<string, unknown> = { ...existingSdk, anthropicBaseUrl: normalizedUrl, apiKey };
  // Write the model only when provided; omitting it PRESERVES any existing
  // sdk.model (the existingSdk spread above), so a manual re-configure that just
  // rotates the key never wipes the model. (Resetting model→default is not a
  // configure feature; add an explicit --clear-model later if ever needed.)
  if (opts?.model) sdk.model = opts.model;
  const merged: Record<string, unknown> = { ...existing, sdk };
  // cc loadConfig requires top-level apiUrl (Octo IM server). Fresh install
  // writes it (daemon passes its server URL) so the zero-bot idle gateway boots.
  if (opts?.apiUrl) merged.apiUrl = opts.apiUrl;
  // ... atomic write of `merged` unchanged ...
}
```

`src/cli.ts`:
- `ParsedArgs` 加 `model?: string; apiUrl?: string;`(注:`apiUrl` = Octo server,区别于 `gatewayUrl` = LLM 网关、`apiKey` = LLM key)。
- `parseArgs` 循环加(仿 `--api-key`),循环前 `let model: string | undefined; let apiUrl: string | undefined;`:
```ts
    } else if (a === '--model') {
      const next = rest[++i];
      if (next === undefined || next.startsWith('--')) throw new Error('configure: --model requires a value');
      model = next;
    } else if (a.startsWith('--model=')) {
      model = a.slice('--model='.length);
    } else if (a === '--api-url') {
      const next = rest[++i];
      if (next === undefined || next.startsWith('--')) throw new Error('configure: --api-url requires a value');
      apiUrl = next;
    } else if (a.startsWith('--api-url=')) {
      apiUrl = a.slice('--api-url='.length);
```
  return 里加 `model, apiUrl`。
- `run` 的 `configure` case:`configure(gatewayUrl ?? '', resolvedApiKey, configPath, { model, apiUrl })`(从 `parseArgs` 解构 `model, apiUrl`)。
- usage 文本:`cc-channel-octo configure --gateway-url <url> [--api-key <key>] [--model <model>] [--api-url <octo-server-url>]`。

- [ ] **Step 4: 跑测试确认通过**

Run: `npx vitest run && npm run type-check && npm run lint`
Expected: PASS(`--max-warnings 0`,无 `any`:`sdk`/`merged` 用 `Record<string, unknown>`)。

- [ ] **Step 5: commit**

```bash
git add src/configure.ts src/cli.ts src/__tests__/configure.test.ts src/__tests__/cli.test.ts
git commit -m "feat(configure): add --model and --api-url; clear sdk.model when omitted"
```

---

## octo-fleet(base `main`)

### Task 5: #3 — install secret 透传 `model`

**Files:**
- Modify: `modules/runtime/cc_octo_secret.go`(`ccOctoSecret` 结构)
- Modify: `modules/runtime/model.go:89-90`(install 请求结构,加 `Model`)
- Modify: `modules/runtime/upgrade.go:358`(stash 时带 `Model`)
- Modify: `modules/runtime/cc_octo_fetch.go:17-18,116`(响应结构 + 回写 `model`)
- Test: `modules/runtime/cc_octo_fetch_test.go`(追加)

**Interfaces:**
- Produces: install 请求 JSON 接受可选 `model`;`GET /v1/upgrades/:task_id/cc-octo-config` 响应 `data.model`(可空,`omitempty`)。

- [ ] **Step 1: 写失败测试**(序列化单测 —— 本模块 `cc_octo_fetch_test.go` 是**静态源码扫描**风格,无可复用的 handler/gin/recorder 夹具,故这里测结构序列化而非起 handler)

```go
func TestCcOctoConfigResponse_SerializesModel(t *testing.T) {
    b, _ := json.Marshal(ccOctoConfigResponse{GatewayURL: "https://gw", APIKey: "sk", Model: "vertexai/claude-opus-4-8"})
    if !strings.Contains(string(b), `"model":"vertexai/claude-opus-4-8"`) {
        t.Fatalf("model not serialized: %s", b)
    }
    // omitempty: 空 model 不应出现在响应里
    b2, _ := json.Marshal(ccOctoConfigResponse{GatewayURL: "https://gw", APIKey: "sk"})
    if strings.Contains(string(b2), `"model"`) {
        t.Fatalf("empty model must be omitted: %s", b2)
    }
}

func TestCcOctoSecret_CarriesModel(t *testing.T) {
    s := ccOctoSecret{GatewayURL: "https://gw", APIKey: "sk", Model: "m1"}
    if s.Model != "m1" { t.Fatalf("got %q", s.Model) }
}
```
(`encoding/json`、`strings` 按需 import。若文件已有同名 import 复用。)

- [ ] **Step 2: 跑测试确认失败**

Run: `cd /Users/caster/octo/octo-fleet && go test ./modules/runtime/ -run "TestCcOctoConfigResponse_SerializesModel|TestCcOctoSecret_CarriesModel"`
Expected: FAIL —— 结构无 `Model` 字段 / 编译错。

- [ ] **Step 3: 改实现**

- `cc_octo_secret.go`:`ccOctoSecret` 加 `Model string`。
- `model.go`(install 请求结构,约 :89-90 紧邻 `GatewayURL`/`APIKey`):加 `Model string \`json:"model,omitempty"\``。
- `upgrade.go:358`:`ccSecret = &ccOctoSecret{GatewayURL: req.GatewayURL, APIKey: req.APIKey, Model: req.Model}`。
- `cc_octo_fetch.go`:`ccOctoConfigResponse` 加 `Model string \`json:"model,omitempty"\``(:17-18 附近);:116 `ResponseData(c, ccOctoConfigResponse{GatewayURL: secret.GatewayURL, APIKey: secret.APIKey, Model: secret.Model})`。
- 注意:`model` 是可选,不要加进 `upgrade.go:314` 的 `GatewayURL==""||APIKey==""` 必填校验。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./modules/runtime/ -run "TestCcOcto" && go build ./...`
Expected: PASS(新序列化测 + 既有静态扫描测 `TestFetchCcOctoConfig_HasOwnershipGate` 均过)。

- [ ] **Step 5: commit**

```bash
git add modules/runtime/cc_octo_secret.go modules/runtime/model.go modules/runtime/upgrade.go modules/runtime/cc_octo_fetch.go modules/runtime/cc_octo_fetch_test.go
git commit -m "feat(runtime): relay optional model through cc-octo install secret"
```

### Task 6: #3 — `/v1/models` 代理端点

**Files:**
- Create: `modules/runtime/llm_models.go`
- Create: `modules/runtime/llm_models_test.go`
- Modify: `modules/runtime/api.go`(web 组加路由)

**为什么不复用 `isAllowedGatewayURL`(关键 SSRF)**:`isAllowedGatewayURL`(`cc_octo_url.go:58`)是给**install 任务**用的 UX 快检——它**放行** `http://localhost`/`127.0.0.1`(本地 dev 网关),且**不做 DNS 解析**。install 场景 URL 是给**用户机器上的 daemon** 消费(localhost=用户机),无害。但 models 代理是 **fleet 服务端主动 GET** —— 复用它 = localhost 指向 fleet 自身 + DNS-rebinding 绕过 = SSRF。故代理用独立、**拨号时**校验解析 IP 的严格策略(见下 `newSafeProxyClient`),而非仅入口字符串/单次解析校验(单次解析后 http.Client 会重解析,留 TOCTOU 窗口)。

**Interfaces:**
- Produces:
  - `POST /v1/runtimes/llm-models`(web scope)body `{gateway_url, api_key}` → `{data:{models:["id1","id2"]}}`。
  - 纯函数 `parseModelIDs(body []byte) ([]string, error)` 解析 `{"data":[{"id":...}]}`。
  - 纯函数 `isUnsafeIP(ip net.IP) bool` —— loopback/private/link-local/unspecified + v4-mapped(`::ffff:`)+ NAT64(`64:ff9b::/96`)内嵌私网,全覆盖,无网络依赖。
  - `validateProxyURL(raw string) error` —— 快失败 UX 检查(https + host≠localhost),**非权威**。
  - `newSafeProxyClient(timeout)` —— 真正的 SSRF 防护:`http.Transport.DialContext` 在**拨号时**解析 host、对每个解析 IP 跑 `isUnsafeIP`、直接拨已校验的 IP(消除"校验后重解析"的 DNS-rebinding TOCTOU);`CheckRedirect` 返错(禁 redirect 跳转到内网)。
  - handler 持可注入字段 `proxyClient *http.Client`(nil → `newSafeProxyClient(10s)`),测试注入指向 httptest 的 client,免真实 DNS。

- [ ] **Step 1: 写失败测试**(全部不依赖真实网络)

```go
func TestParseModelIDs(t *testing.T) {
    body := []byte(`{"data":[{"id":"ali/deepseek-r1","type":"model"},{"id":"vertexai/claude-opus-4-8"}]}`)
    ids, err := parseModelIDs(body)
    if err != nil { t.Fatal(err) }
    if len(ids) != 2 || ids[0] != "ali/deepseek-r1" || ids[1] != "vertexai/claude-opus-4-8" {
        t.Fatalf("got %v", ids)
    }
}

func TestParseModelIDs_Empty(t *testing.T) {
    if ids, err := parseModelIDs([]byte(`{"data":[]}`)); err != nil || len(ids) != 0 {
        t.Fatalf("ids=%v err=%v", ids, err)
    }
}

func TestIsUnsafeIP(t *testing.T) {
    unsafe := []string{
        "127.0.0.1", "::1",                 // loopback
        "10.0.0.5", "192.168.1.1", "172.16.0.1", "fd00::1", // private
        "169.254.1.1", "fe80::1",           // link-local
        "0.0.0.0", "::",                    // unspecified
        "::ffff:10.0.0.1",                  // v4-mapped private
        "64:ff9b::a00:1",                   // NAT64-embedded 10.0.0.1
    }
    for _, s := range unsafe {
        if !isUnsafeIP(net.ParseIP(s)) { t.Errorf("expected unsafe: %s", s) }
    }
    safe := []string{"8.8.8.8", "1.1.1.1", "2606:4700::1111"}
    for _, s := range safe {
        if isUnsafeIP(net.ParseIP(s)) { t.Errorf("expected safe: %s", s) }
    }
    if !isUnsafeIP(nil) { t.Error("nil must be unsafe") }
}

func TestValidateProxyURL(t *testing.T) {
    for _, bad := range []string{"http://gw.test/v1", "https://localhost/v1", "not a url", ""} {
        if err := validateProxyURL(bad); err == nil { t.Errorf("expected reject: %q", bad) }
    }
    if err := validateProxyURL("https://gw.example.com/v1"); err != nil {
        t.Errorf("expected allow: %v", err)
    }
}

func TestFetchLLMModels_ProxiesUpstream(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
        _, _ = w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
    }))
    defer srv.Close()
    // 注入指向 httptest 的 client(httptest 是 127.0.0.1,生产 dial 会拒;测试注入绕过)。
    rt := &Runtime{proxyClient: srv.Client()}
    // 构造 gin 请求 body {gateway_url: srv.URL, api_key:"sk"} 调 rt.fetchLLMModels,
    // 断言响应 data.models == ["m1","m2"]。(validateProxyURL 对 srv.URL 是 http → 需
    //  测试用 srv.URL 时放宽:把 pre-flight 也设成可注入,或测试直接覆盖 base。见 Step 3 注。)
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./modules/runtime/ -run "TestParseModelIDs|TestIsUnsafeIP|TestValidateProxyURL"`
Expected: FAIL —— 函数未定义。

- [ ] **Step 3: 改实现**(`llm_models.go`)

```go
package runtime

func parseModelIDs(body []byte) ([]string, error) {
    var env struct {
        Data []struct{ ID string `json:"id"` } `json:"data"`
    }
    if err := json.Unmarshal(body, &env); err != nil {
        return nil, fmt.Errorf("parse models: %w", err)
    }
    ids := make([]string, 0, len(env.Data))
    for _, m := range env.Data {
        if m.ID != "" { ids = append(ids, m.ID) }
    }
    return ids, nil
}

// isUnsafeIP rejects any address that must never be the target of a server-side
// proxy request (SSRF). Normalizes v4-mapped and unwraps NAT64 before testing.
func isUnsafeIP(ip net.IP) bool {
    if ip == nil { return true }
    if v4 := ip.To4(); v4 != nil { ip = v4 }
    if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
        ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
        return true
    }
    // NAT64 64:ff9b::/96 → unwrap embedded IPv4 and re-check.
    if len(ip) == net.IPv6len && ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b &&
        ip[4] == 0 && ip[5] == 0 && ip[6] == 0 && ip[7] == 0 &&
        ip[8] == 0 && ip[9] == 0 && ip[10] == 0 && ip[11] == 0 {
        return isUnsafeIP(net.IPv4(ip[12], ip[13], ip[14], ip[15]))
    }
    return false
}

// validateProxyURL is a fast-fail UX check; the AUTHORITATIVE SSRF gate is the
// dial-time IP check in newSafeProxyClient (handles DNS rebinding).
func validateProxyURL(raw string) error {
    u, err := url.Parse(strings.TrimSpace(raw))
    if err != nil || u.Host == "" { return fmt.Errorf("invalid url") }
    if u.Scheme != "https" { return fmt.Errorf("gateway must be https") }
    if u.Hostname() == "localhost" { return fmt.Errorf("localhost not allowed") }
    return nil
}

// newSafeProxyClient validates the resolved IP AT DIAL TIME and dials that exact
// IP, so a name that resolves safe-then-private (rebinding) cannot slip through.
func newSafeProxyClient(timeout time.Duration) *http.Client {
    dialer := &net.Dialer{Timeout: timeout}
    return &http.Client{
        Timeout:       timeout,
        CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("redirects not allowed") },
        Transport: &http.Transport{
            DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
                host, port, err := net.SplitHostPort(addr)
                if err != nil { return nil, err }
                ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
                if err != nil || len(ips) == 0 { return nil, fmt.Errorf("resolve %s failed", host) }
                for _, ip := range ips {
                    if isUnsafeIP(ip) { return nil, fmt.Errorf("refusing non-public address %s", ip) }
                }
                return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
            },
        },
    }
}

type llmModelsReq struct {
    GatewayURL string `json:"gateway_url"`
    APIKey     string `json:"api_key"`
}

func (rt *Runtime) fetchLLMModels(c *wkhttp.Context) {
    var req llmModelsReq
    if err := c.BindJSON(&req); err != nil || req.GatewayURL == "" || req.APIKey == "" {
        // respond VALIDATION_ERROR (mirror existing handlers' error helper); return
    }
    if err := validateProxyURL(req.GatewayURL); err != nil {
        // respond VALIDATION_ERROR with err; return
    }
    client := rt.proxyClient
    if client == nil { client = newSafeProxyClient(10 * time.Second) }
    base := strings.TrimSuffix(strings.TrimSuffix(req.GatewayURL, "/"), "/v1")
    // GET {base}/v1/models with x-api-key + anthropic-version via `client`;
    // upstream non-200 → readable error; 200 → parseModelIDs → ResponseData(c, {"models": ids}).
}
```
错误响应 / `wkhttp` 绑定按本模块既有 handler 风格写。imports:`context`、`net`、`net/http`、`net/url`、`strings`、`time`、`encoding/json`、`fmt`。`Runtime` 结构加字段 `proxyClient *http.Client`(测试注入用,生产 nil → 默认 safe client)。测试里 `srv.URL` 是 http://127.0.0.1 会被 `validateProxyURL` 的 https 检查拒——`TestFetchLLMModels_ProxiesUpstream` 用注入 client 时,把 `validateProxyURL` 也设为可注入字段(`proxyURLCheck func(string) error`,nil→默认),或在该测试里直接测 base 拼接 + parse 路径,避免改 pre-flight 语义。

`api.go` web 组加:
```go
		web.POST("/runtimes/llm-models", rt.fetchLLMModels)   // proxy gateway /v1/models for install model picker
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./modules/runtime/ -run "TestParseModelIDs|TestIsUnsafeIP|TestValidateProxyURL|TestFetchLLMModels" && golangci-lint run ./modules/runtime/...`
Expected: PASS。

- [ ] **Step 5: commit**

```bash
git add modules/runtime/llm_models.go modules/runtime/llm_models_test.go modules/runtime/api.go
git commit -m "feat(runtime): proxy gateway /v1/models for install model picker"
```

---

## octo-daemon-cli(base `main`)

### Task 7: #3 — CcOctoConfig.Model + configure 透传 `--model`

**Files:**
- Modify: `internal/client.go:96-99`(`CcOctoConfig` 加 `Model`)
- Modify: `internal/plugin_upgrade.go:155`(`ccOctoConfigureArgs(url, model, apiURL)`)、`:204`(调用传 `cfg.Model` + `d.cfg.ServerURL`)
- Test: `internal/plugin_upgrade_test.go`(扩 `TestPluginUpgradeCommand*` 风格的纯函数测)

**Interfaces:**
- Produces: `ccOctoConfigureArgs(gatewayURL, model, apiURL string) []string` — model 非空追加 `--model <model>`,apiURL 非空追加 `--api-url <apiURL>`;`CcOctoConfig.Model string json:"model"`。
- apiURL 来自 `d.cfg.ServerURL`(Octo IM server,`internal/config.go:63`):cc loadConfig 必填顶层 apiUrl,装时一并写入,idle gateway 才能启动(否则 loadConfig 抛 `Missing required config: apiUrl`)。

- [ ] **Step 1: 写失败测试**(`plugin_upgrade_test.go`)

```go
func TestCcOctoConfigureArgs_Minimal(t *testing.T) {
    got := ccOctoConfigureArgs("https://gw.test", "", "")
    want := []string{"configure", "--gateway-url", "https://gw.test"}
    if !reflect.DeepEqual(got, want) { t.Fatalf("got %v want %v", got, want) }
}

func TestCcOctoConfigureArgs_WithModelAndApiURL(t *testing.T) {
    got := ccOctoConfigureArgs("https://gw.test", "vertexai/claude-opus-4-8", "http://127.0.0.1:8090")
    want := []string{"configure", "--gateway-url", "https://gw.test",
        "--model", "vertexai/claude-opus-4-8", "--api-url", "http://127.0.0.1:8090"}
    if !reflect.DeepEqual(got, want) { t.Fatalf("got %v want %v", got, want) }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd /Users/caster/octo/octo-daemon-cli && go test ./internal/ -run TestCcOctoConfigureArgs`
Expected: FAIL —— `ccOctoConfigureArgs` 只接 1 参,编译错。

- [ ] **Step 3: 改实现**

`internal/client.go`:
```go
type CcOctoConfig struct {
	GatewayURL string `json:"gateway_url"`
	APIKey     string `json:"api_key"`
	Model      string `json:"model"`
}
```
(`FetchCcOctoConfig` 的空校验保持只看 `GatewayURL`/`APIKey`,model 可空。)

`internal/plugin_upgrade.go`:
```go
func ccOctoConfigureArgs(gatewayURL, model, apiURL string) []string {
	args := []string{"configure", "--gateway-url", gatewayURL}
	if model != "" {
		args = append(args, "--model", model)
	}
	if apiURL != "" {
		args = append(args, "--api-url", apiURL)
	}
	return args
}
```
:204 调用改 `ccOctoConfigureArgs(cfg.GatewayURL, cfg.Model, d.cfg.ServerURL)`。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/ -run "TestCcOctoConfigureArgs|TestCcOcto" && go build ./...`
Expected: PASS。

- [ ] **Step 5: commit**

```bash
git add internal/client.go internal/plugin_upgrade.go internal/plugin_upgrade_test.go
git commit -m "feat: pass model and octo server url from install config to cc configure"
```

### Task 8: #1 — 安装后拉起 gateway

**Files:**
- Modify: `internal/plugin_upgrade.go`(`handleCcOctoInstall`,configure 成功后、enrich 前;加 `ccOctoStartArgs()`)
- Test: `internal/plugin_upgrade_test.go`(`ccOctoStartArgs` 纯函数)

**Interfaces:**
- Produces: `ccOctoStartArgs() []string` → `[]string{"start"}`。

- [ ] **Step 1: 写失败测试**

```go
func TestCcOctoStartArgs(t *testing.T) {
    if got := ccOctoStartArgs(); !reflect.DeepEqual(got, []string{"start"}) {
        t.Fatalf("got %v", got)
    }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/ -run TestCcOctoStartArgs`
Expected: FAIL —— 未定义。

- [ ] **Step 3: 改实现**

`plugin_upgrade.go` 加:
```go
// ccOctoStartArgs starts the gateway after a fresh install so the claude
// runtime comes online (idle, zero-bot) without waiting for the first bot.
func ccOctoStartArgs() []string { return []string{"start"} }
```
`handleCcOctoInstall` 在 configure 成功 log 之后、enrich 循环之前插入(start 失败仅 warn,不 fail 整个任务——provision 的 restart 会兜底拉起):
```go
	// Bring the gateway online now (idle, zero bots) so enrich reports claude
	// online immediately after install. A start failure is non-fatal: the
	// first bot.provision runs `cc-channel-octo restart` which will start it.
	if out, serr := exec.CommandContext(installCtx, "cc-channel-octo", ccOctoStartArgs()...).CombinedOutput(); serr != nil {
		log.Printf("[WARN] cc-octo post-install start failed (provision restart will retry): %v\noutput: %s", serr, truncateOutput(string(out), 400))
	}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/ -run TestCcOctoStartArgs && go build ./...`
Expected: PASS。(start-after-configure 接线由本计划末「全链路验证」覆盖。)

- [ ] **Step 5: commit**

```bash
git add internal/plugin_upgrade.go internal/plugin_upgrade_test.go
git commit -m "feat: start cc-channel-octo gateway after install so claude runtime comes online"
```

### Task 9: #3 — 删 Provision 模型硬写

**Files:**
- Modify: `internal/adapter/claude.go`(删 `claudeModel` 常量 :27 + Provision :97-98 的 `sdk.model`)
- Test: `internal/adapter/claude_test.go`(若无则新建,验写出的 per-bot config 不含 model)

**Interfaces:**
- Produces: `Provision` 写的 per-bot `config.json` 不再含 `sdk.model`(model 改由全局 config 决定)。

- [ ] **Step 1: 写失败测试**

```go
func TestProvision_OmitsModel(t *testing.T) {
    home := t.TempDir()
    t.Setenv("HOME", home)
    a := &ClaudeAdapter{ /* mirror existing构造 */ }
    _, err := a.Provision(context.Background(), ProvisionRequest{
        BotUID: "b1_bot", BotToken: "bf_x", APIURL: "http://127.0.0.1:8090",
    })
    // restart 会因无 cc 二进制 warn,但 Provision 不 fail(现行为)。
    if err != nil { t.Fatalf("provision: %v", err) }
    raw, _ := os.ReadFile(filepath.Join(home, ".cc-channel-octo", "b1_bot", "config.json"))
    var cfg map[string]any
    _ = json.Unmarshal(raw, &cfg)
    sdk, _ := cfg["sdk"].(map[string]any)
    if _, has := sdk["model"]; has {
        t.Fatalf("per-bot config must not pin sdk.model, got %v", sdk)
    }
}
```
(`ClaudeAdapter` 构造按现有测试/字段填;若 restart 失败会 fail Provision,则测试改为断言返回 error 同时仍校验已落盘的 config.json。)

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/adapter/ -run TestProvision_OmitsModel`
Expected: FAIL —— 当前写了 `sdk.model = claudeModel`。

- [ ] **Step 3: 改实现**

`internal/adapter/claude.go`:
- 删 `const claudeModel = "vertexai/claude-opus-4-8"`(:26-27)及其注释。
- Provision 的 `cfg`(:96-98)改为:
```go
	cfg := map[string]any{
		"botToken": req.BotToken,
	}
```
  删掉 `"sdk": map[string]any{"model": claudeModel}`。`apiUrl` 注入逻辑(:106)保持。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/adapter/ && go build ./...`
Expected: PASS。

- [ ] **Step 5: commit**

```bash
git add internal/adapter/claude.go internal/adapter/claude_test.go
git commit -m "fix: stop pinning hardcoded model per-bot; model is gateway-level global config"
```

---

## octo-web(base `feat/agent-runtime`)

### Task 10: #2 — URL 规范化 + 默认值 placeholder

**Files:**
- Modify: `packages/dmworkbase/src/Pages/Runtimes/ccInstallValidate.ts`(加 `normalizeGatewayUrl`)
- Modify: `packages/dmworkbase/src/Pages/Runtimes/CcInstallModal.tsx`(初值 + 提交规范化 + 提示)
- Modify: `packages/dmworkbase/src/i18n/locales/{en-US,zh-CN}.json`(新增 hint 文案)
- Test: `packages/dmworkbase/src/Pages/Runtimes/ccInstallValidate.test.ts`(追加)

**Interfaces:**
- Produces: `normalizeGatewayUrl(raw: string): string`(strip 结尾 `/v1`,与 cc 对齐)。

- [ ] **Step 1: 写失败测试**

```ts
import { normalizeGatewayUrl } from "./ccInstallValidate";
describe("normalizeGatewayUrl", () => {
  it("strips trailing /v1", () => {
    expect(normalizeGatewayUrl("https://gw.test/v1")).toBe("https://gw.test");
    expect(normalizeGatewayUrl("https://gw.test/v1/")).toBe("https://gw.test");
  });
  it("leaves other paths intact", () => {
    expect(normalizeGatewayUrl("https://gw.test/api")).toBe("https://gw.test/api");
    expect(normalizeGatewayUrl("https://gw.test")).toBe("https://gw.test");
  });
});
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd /Users/caster/octo/octo-web && pnpm --filter @octo/base test -- ccInstallValidate`
Expected: FAIL —— 未导出 `normalizeGatewayUrl`。(测试命令以仓库实际 vitest 入口为准,见 web CLAUDE.md `cd apps/web && pnpm test`。)

- [ ] **Step 3: 改实现**

`ccInstallValidate.ts` 加:
```ts
/** Mirror cc-channel-octo's normalizeGatewayUrl: strip a trailing /v1(/) so the
 *  SDK's appended /v1/messages doesn't double. */
export function normalizeGatewayUrl(raw: string): string {
  return raw.trim().replace(/\/v1\/?$/, "");
}
```

`CcInstallModal.tsx`:
- 初值用 env 默认:`const [gatewayUrl, setGatewayUrl] = useState(import.meta.env.VITE_OCTO_DEFAULT_GATEWAY_URL ?? "")`。
- 提交时规范化:`props.onSubmit(normalizeGatewayUrl(gatewayUrl), apiKey.trim(), ...)`(model 见 Task 11)。
- URL 输入下加提示:`<div className="wk-cc-install-hint">{t("base.runtimes.ccInstall.gatewayHint")}</div>`。

i18n 两个 locale 加 `base.runtimes.ccInstall.gatewayHint`:
- en-US: `"Enter the gateway base URL without a trailing /v1."`
- zh-CN: `"填写网关基础地址，不要带结尾的 /v1。"`

- [ ] **Step 4: 跑测试确认通过**

Run: `pnpm --filter @octo/base test -- ccInstallValidate && pnpm lint`
Expected: PASS。

- [ ] **Step 5: commit**

```bash
git add packages/dmworkbase/src/Pages/Runtimes/ccInstallValidate.ts packages/dmworkbase/src/Pages/Runtimes/CcInstallModal.tsx packages/dmworkbase/src/Pages/Runtimes/ccInstallValidate.test.ts packages/dmworkbase/src/i18n/locales/en-US.json packages/dmworkbase/src/i18n/locales/zh-CN.json
git commit -m "feat(runtimes): normalize gateway url and prefill default in cc install modal"
```

### Task 11: #3 — 模型 combobox(动态拉取)+ 串到安装请求

**Files:**
- Modify: `packages/dmworkbase/src/Pages/Runtimes/CcInstallModal.tsx`(model 输入 + datalist + 拉取按钮)
- Modify: `packages/dmworkbase/src/Pages/Runtimes/index.tsx`(`secret` 类型 + `handlePluginUpgrade` 透传 `model`)
- Modify: `packages/dmworkbase/src/Pages/Runtimes/botsApi.ts` 或就近 API util(加 `fetchLlmModels`)
- Modify: `i18n/locales/{en-US,zh-CN}.json`(model label/占位/拉取按钮文案)
- Test: 新增/追加 `CcInstallModal` 或 api util 的 vitest

**Interfaces:**
- Consumes: fleet `POST /v1/runtimes/llm-models {gateway_url, api_key}` → `{data:{models:string[]}}`(Task 6)。
- Produces:
  - `CcInstallModal` props `onSubmit: (gatewayUrl: string, apiKey: string, model: string) => void`。
  - `secret: { gatewayUrl: string; apiKey: string; model?: string }`,`/upgrades` 载荷加 `model`。

- [ ] **Step 1: 写失败测试**(api util:拉取并解析 models)

```ts
// 测 fetchLlmModels 解析 {data:{models:[...]}};mock WKApp.apiClient.post 返回该结构,
// 断言返回 string[]。
it("fetchLlmModels returns model ids", async () => {
  // mock apiClient.post -> { data: { models: ["m1", "m2"] } }
  const out = await fetchLlmModels("https://gw.test", "sk");
  expect(out).toEqual(["m1", "m2"]);
});
```

- [ ] **Step 2: 跑测试确认失败**

Run: `pnpm --filter @octo/base test -- ccInstall`
Expected: FAIL —— `fetchLlmModels` 未定义。

- [ ] **Step 3: 改实现**

API util(就近放,如 `ccInstallApi.ts` 新建或并入现有 util):
```ts
export async function fetchLlmModels(gatewayUrl: string, apiKey: string): Promise<string[]> {
  const res = await WKApp.apiClient.post(
    "/runtimes/llm-models",
    { gateway_url: gatewayUrl, api_key: apiKey },
    { baseURL: FLEET_API_BASE },
  );
  return res?.data?.models ?? [];
}
```

`CcInstallModal.tsx`:
- 加 `const [model, setModel] = useState("")`、`const [models, setModels] = useState<string[]>([])`、`const [loadingModels, setLoadingModels] = useState(false)`。
- 加「拉取模型」按钮(填好 url+key 后启用):点了调 `fetchLlmModels(normalizeGatewayUrl(gatewayUrl), apiKey)` → `setModels(...)`;失败 toast/提示但不阻塞(仍可手填)。
- model 输入做 combobox:
```tsx
<input className="wk-cc-install-input" list="cc-model-options"
       placeholder={t("base.runtimes.ccInstall.modelPlaceholder")}
       value={model} onChange={e => setModel(e.target.value)} />
<datalist id="cc-model-options">
  {models.map(m => <option key={m} value={m} />)}
</datalist>
```
- 提交:`props.onSubmit(normalizeGatewayUrl(gatewayUrl), apiKey.trim(), model.trim())`。
- model 可选,不入 `v.ok` 必填校验。

`index.tsx`:
- `secret` 类型扩为 `{ gatewayUrl: string; apiKey: string; model?: string }`(`handlePluginUpgrade` 签名 + 调用处 onSubmit 回调,约 :1075、:1999 的 onSubmit wiring)。
- `/upgrades` 载荷(:1086)改:
```ts
...(secret ? { gateway_url: secret.gatewayUrl, api_key: secret.apiKey, ...(secret.model ? { model: secret.model } : {}) } : {}),
```
- 打开 modal 的 onSubmit 回调把第三参 `model` 装进 `secret`。

i18n 两 locale 加:`modelLabel`、`modelPlaceholder`(如 en `"Model (optional)"` / `"Pick or type a model name"`;zh `"模型(可选)"` / `"选择或输入模型名"`)、`fetchModels`(en `"Fetch models"` / zh `"拉取模型"`)。

- [ ] **Step 4: 跑测试确认通过**

Run: `pnpm --filter @octo/base test -- ccInstall && pnpm lint`
Expected: PASS。

- [ ] **Step 5: commit**

```bash
git add packages/dmworkbase/src/Pages/Runtimes/CcInstallModal.tsx packages/dmworkbase/src/Pages/Runtimes/index.tsx packages/dmworkbase/src/Pages/Runtimes/ccInstallApi.ts packages/dmworkbase/src/i18n/locales/en-US.json packages/dmworkbase/src/i18n/locales/zh-CN.json
git commit -m "feat(runtimes): model picker with dynamic gateway model list in cc install modal"
```

---

## 全链路验证(所有 task 后)

用 `octo-runtime-local-env` skill 起 runtime 四件套 + web,跑一遍:

1. 干净环境卸载 cc,Runtimes 页点安装 → 弹框预填默认网关 → 填 key → 点「拉取模型」见列表 → 选 model → 装。
2. 装完 claude runtime **立即 online**(0-bot idle 生效)。
3. 建第一个 bot(online 门槛自然满足)→ provision restart 接上。
4. 聊天回复正常;换一个 model 重装/重配,验证模型生效(请求打到的模型名是所选)。
5. 验证带 `/v1` 的网关也能聊(strip 生效:`curl base/v1/messages` 200)。

---

## Self-Review(写完对照 spec)

- **#1 死锁**:Task 1(resolveBotConfigs [])+ Task 2(idle 常驻)+ Task 4/Task 7(装时写顶层 `apiUrl`,否则 idle gateway loadConfig 即崩)+ Task 8(装后 start)→ claude 装完 online,web 无需改。✓
- **#2 /v1**:Task 3(cc 权威 strip)+ Task 10(web normalize + 提示 + 默认值)。✓
- **#3 模型**:Task 9(删硬写)+ Task 4(cc `--model`,省略即保留)+ Task 7(daemon 透传 model)+ Task 5(fleet relay)+ Task 6(models 代理 + 严格 SSRF)+ Task 11(web combobox 动态拉取)。✓
- **类型一致**:`normalizeGatewayUrl`(cc/web 同名同义)、`model` 字段(cc `sdk.model` / fleet `model` json / daemon `CcOctoConfig.Model` / web `secret.model`)全链路命名一致、均可选;`apiUrl`(cc 顶层 `config.apiUrl` / daemon `d.cfg.ServerURL` / cc `--api-url` flag)一致。✓
- **占位符扫描**:无 TBD;Step 含真实代码 + 命令 + 期望。fleet Task 5 改为结构序列化单测(本模块 handler 测试是静态源码扫描风格,无 handler 夹具可复用);Task 6 SSRF 校验落在拨号时(`newSafeProxyClient` 的 `DialContext`),`isUnsafeIP`/`validateProxyURL` 为纯函数单测,handler 测试注入 `proxyClient` 指向 httptest 免真实 DNS。
- **跨仓顺序**:cc → fleet → daemon → web,与 Global Constraints 契约一致。✓

### 本轮 plan-review 已纳入的修正
1. **idle 保活**:0-bot 分支用未 unref 的 `setInterval` 撑事件循环(pending Promise 不保活,进程会立即退出),信号处理里 `clearInterval` 清理(Task 2)。
2. **装时写 `apiUrl`**:cc `loadConfig` 必填顶层 `apiUrl`,configure 原只写 `sdk.*` → idle gateway 启动即崩;新增 `configure --api-url`,daemon 传 `d.cfg.ServerURL`(Task 4 / Task 7)。
3. **0-bot 兼容 default per-bot 文件**:早退 `[]` 前还要确认 `<baseDir>/default/config.json` 无 botToken,否则会误停合法单 bot(Task 1)。
4. **models 代理独立严格 SSRF**:不复用放行 localhost / 不解析 DNS 的 install UX 检查;SSRF 校验落在**拨号时**(`newSafeProxyClient` 的 `DialContext` 解析并对每个 IP 跑 `isUnsafeIP`、直拨已校验 IP,消除重解析 TOCTOU),`isUnsafeIP` 覆盖 loopback/私网/link-local/unspecified/v4-mapped/NAT64,且禁 redirect 跟随(Task 6)。
5. **model 省略语义**:configure 省略 model 时**保留**既有 `sdk.model`(只在显式给值时改),手工换 key 不丢 model;全链路纯 omitempty(Task 4)。
6. **fleet 测试夹具属实**:Task 5 改为结构序列化单测,不引用不存在的 handler 夹具。

### 设计决策(models 代理是否支持本地 dev 网关 http://localhost)
不支持。models 代理是 fleet 服务端主动外呼,拨号时 `isUnsafeIP` 生产**默认拒** localhost/私网(防 SSRF);我们的 LLM 网关是公网 https 端点,无需 localhost。本地 dev/单测通过注入 `proxyClient`(指向 httptest)放行,不在生产路径开口。
