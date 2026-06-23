# cc 一键安装到可聊天的 3 个缺口修复 — 设计文档

**来源**:octo-daemon-cli issue #65。1b cc 插件一键安装本地端到端实测挖出的 3 个缺口,单独都能手工绕过,叠加后一个全新用户无法仅靠 UI 走通「装完 → 上线 → 建 bot → 聊天」。

**目标**:让全新用户在 Runtimes 页一键装好 cc 插件后,仅靠 UI 即可走通到聊天;并顺手把网关 URL / 模型选择做成对不同网关通用、对用户友好的安装输入。

**架构思路**:三个缺口分别落在 cc-channel-octo(gateway 行为)、octo-daemon-cli(安装编排 + provision)、octo-fleet(安装后端)、octo-web(安装 UI)。各仓改动通过明确的可选契约字段解耦,任一环缺失都向后兼容地退化。

## 分支基线(各仓 1b 均已合并)

| 仓库 | base 分支 | 本轮覆盖 |
|---|---|---|
| cc-channel-octo | `main` | #1 idle 常驻 + #2 URL 规范化 + #3 `configure --model` |
| octo-daemon-cli | `main` | #1 装后拉起 gateway + #3 删模型硬写 / 透传 model |
| octo-fleet | `main` | #3 relay model + models 代理端点 |
| octo-web | `feat/agent-runtime` | #2 URL 提示/默认 + #3 模型 combobox(动态拉取) |

## 全局约束(每个 task 隐含遵守)

- **代码 / commit / PR 无 provenance 痕迹**:不带 AI 署名、`Co-Authored-By`、review 工具名、评审轮次 / finding 代号。provider/model 名(如 `vertexai/claude-opus-4-8`、`claude`)是正当数据,不在禁列。
- **各仓语言规范**:cc-channel-octo 代码/注释/commit 全英文(Conventional Commits);octo-server/fleet/daemon Go 代码英文 Conventional Commits;octo-web 英文 Conventional Commits。
- **model 全链路可选 + 向后兼容**:web → fleet → daemon → cc 任一环未提供 model,最终退回 cc SDK 默认模型,不得硬失败。
- **OSS 不烤死部署专属值**:开源前端不硬编码 mlamp 网关 URL / 模型清单;部署专属默认值走 env 注入(OSS 默认空)。
- **SSRF 复用**:任何「服务端用用户提供的 URL 发起请求」的新路径,必须复用现有私网 / loopback 拦截策略,不得新开探测内网的口子。
- **dev 阶段**:本地跑通即可,不加生产防御措施,但逻辑要完整。

---

## 缺口 #1 — 首个 cc bot 鸡生蛋死锁

### 现象
cc 插件装好后 claude runtime 离线;Web 建 bot 的运行时选择器选不了离线 claude,导致第一个 cc bot 建不出来。

### 根因(三处叠加)
- cc `src/config.ts:497-502` `resolveBotConfigs`:`bots=[]` 时合成 `{id:'default', botToken: config.botToken||undefined}`,随后 `:535` 因无 botToken `throw`。
- cc `src/index.ts:47` 在 resilient `Promise.allSettled` **之前**同步调用 `resolveBotConfigs`,0-bot 时直接 throw → 进程退出。
- daemon `internal/detect.go:82` claude 在线判定 = `isCcChannelOctoRunning()`,而该函数(`detect.go:354`)跑 `cc-channel-octo status` 看输出含 `": running"`;cc supervisor 的 running 判定(`src/cli.ts` `cmdStatus`/`cmdStart`)**纯看进程是否 `isAlive`**,不等 bot/socket 就绪。
- daemon `handleCcOctoInstall`(`internal/plugin_upgrade.go:188`)只 configure + enrich,不启动 gateway。
- web `CreateBotModal.tsx:127-130` 提交要求 runtime `supported && status==='online'`。

### 方案:cc gateway 支持 0-bot idle 常驻 + daemon 装后拉起
- **cc `src/config.ts resolveBotConfigs`**:无 `bots[]` 且无全局 `botToken` 时返回 `[]`,不再合成无 token default。保留兼容:有全局 `botToken` → 仍合成单 `default`。
- **cc `src/index.ts main()`**:`botConfigs.length===0` 时进 idle 模式——安装 SIGINT/SIGTERM 处理、log「idle (no bots configured)」、保持进程存活(不 throw `no bots started`)。进程存活 → supervisor `status: running` → daemon 判 claude 在线。
- **daemon `handleCcOctoInstall`**:configure 成功后调 `cc-channel-octo start` 拉起 gateway(idle),再 enrich;装完即 online。start 失败不应让整个 install 任务失败(已 configure + 后续可由 provision 的 restart 兜底拉起),记 warn。
- **provision 链路不变**(`internal/adapter/claude.go Provision`):写 per-bot config + 加全局 `bots[]` + `cc-channel-octo restart`(stop+start),新进程重读到 1-bot config 接上,socket 连上,完全在线。
- **web 无需改**:0-bot idle 后装完 claude 即 online,`CreateBotModal` 的 online 门槛自然满足。

### 不变量 / 边界
- idle 进程不持有 per-bot 资源(无 gateway.lock、无 socket),restart 时 stop 干净杀掉 idle、start 读新配置。
- idle→1bot 始终经 provision 的 restart,无「不 restart 直接加 bot」路径。
- 单例由 supervisor pidfile(`readRunningPid`)保证,与 idle 无关。

---

## 缺口 #2 — 网关 URL 带 `/v1` 被二次拼接

### 现象
用户填带版本前缀的网关 `https://<host>/v1` 后聊天报「模型不存在」,实为 URL 问题:Anthropic SDK 在 `ANTHROPIC_BASE_URL` 后自拼 `/v1/messages`,base 已含 `/v1` → `…/v1/v1/messages` → 404,被 Claude Code 误报成模型错。(`curl /v1/messages` 200、`/v1/v1/messages` 404 可证。)

### 根因
- cc `src/configure.ts:50` 原样写 `anthropicBaseUrl: gatewayUrl`,无规范化。
- cc `src/agent-bridge.ts:43` 把它喂给 SDK 的 `ANTHROPIC_BASE_URL`。
- web `ccInstallValidate.ts` trim 但不 strip `/v1`。

### 方案:写入时权威规范化 + 输入提示 + 可选默认
- **cc `src/configure.ts`**(权威点,覆盖 daemon 一键装 + 手工 `cc-channel-octo configure`):写 `anthropicBaseUrl` 前 strip 结尾的 `/v1` 或 `/v1/`(仅精确去结尾版本段,不动其它路径前缀);其它合法 base 原样保留。
- **web `ccInstallValidate.ts` / CcInstallModal**:提交前软规范化(同样 strip 结尾 `/v1`),让用户看到规范后的值;输入旁加提示「填不含 `/v1` 的网关地址」(i18n)。
- **web 网关 URL 默认值**:加前端构建期 env(如 `VITE_OCTO_DEFAULT_GATEWAY_URL`)。设了 → 输入框**预填**该值(可编辑);没设(OSS 默认)→ 空 + i18n 通用 placeholder。mlamp 部署 build 设成我们的网关,用户开箱见建议值、改即覆盖。开源仓不硬编码网关。

---

## 缺口 #3 — daemon 给 bot 硬写模型名

### 现象
`internal/adapter/claude.go:27` `const claudeModel = "vertexai/claude-opus-4-8"`;`Provision`(`:97-98`)给每个 bot 的 per-bot config 写该值并覆盖全局。不同用户网关模型名不同,写死单值对其它网关不可用。

### 根因
- model 本是「网关 + 账号」级属性(同 runtime 所有 bot 一致),却被 daemon 逐 bot 硬写并覆盖全局。
- cc `config.ts:158` 已支持 `sdk.model` 省略(SDK 默认),也支持显式设置——daemon 不该越俎。

### 方案:全局可选 model + 动态下拉安装输入
- **daemon `internal/adapter/claude.go Provision`**:删掉 per-bot `sdk.model` 写入 + `claudeModel` 常量;model 只活在全局 config。
- **cc `src/configure.ts` + `src/cli.ts`**:加可选 `--model <model>`(或 env,与 `--api-key` 同模式),写全局 `sdk.model`;省略则不写(SDK 默认)。
- **daemon `CcOctoConfig` + `handleCcOctoInstall`**:`CcOctoConfig` 加 `Model` 字段;非空时 `cc-channel-octo configure --model <model>` 透传。
- **fleet**:install 配置载荷(secret relay)加可选 `model` 字段,relay 给 daemon。
- **fleet 新增 models 代理端点**:`POST /runtimes/llm-models`(路径名待定)接收 `{gatewayUrl, apiKey}` → SSRF / 私网拦截校验 → 服务端 `GET {gateway}/v1/models`(实测我们网关返 OpenAI 风格 `{"data":[{"id":...}]}`,HTTP 200)→ 解析 `data[].id` 返列表;网关错误(401/超时等)透传成可读提示。
- **web CcInstallModal**:模型输入做成**可编辑 combobox**——用户填完 url+key 后(失焦或点「拉取模型」)调 fleet 代理拉真实模型列表作建议项,可选可手填;提交时 model 随 url+key 走既定链路(web→fleet→daemon→cc)。
- **向后兼容**:model 全链路可选,任一环缺失 → cc SDK 默认。

---

## 跨仓库契约与上线顺序(#3)

契约字段 `model` 全可选、向后兼容,部分上线只退默认不硬失败。建议顺序:

1. **cc**:`configure --model`(additive flag)。
2. **fleet**:install 载荷接受 + relay `model`;新增 models 代理端点。
3. **daemon**:从 fetch 的配置取 `model` 透传 `configure --model`;删 per-bot 硬写。
4. **web**:combobox 采集 model + 调 fleet 代理拉列表。

daemon `configure --model` 依赖 cc 已支持该 flag(仅当 model 非空时才发);cc 先行最稳。

## PR 结构(4 仓各自 base,各自 PR)

- cc-channel-octo(base main):#1 idle(config.ts + index.ts)+ #2 strip(configure.ts)+ #3 `--model`(configure.ts + cli.ts)。
- octo-daemon-cli(base main):#1 装后 start gateway(plugin_upgrade.go)+ #3 删硬写(claude.go)+ `CcOctoConfig.Model` 透传(plugin_upgrade.go + 配置结构)。
- octo-fleet(base main):#3 relay model + models 代理端点(runtime module)。
- octo-web(base feat/agent-runtime):#2 提示 + URL env 默认(CcInstallModal + ccInstallValidate)+ #3 模型 combobox(CcInstallModal + 调代理)。

## 测试

- **cc**(vitest):`resolveBotConfigs` 0-bot 返 `[]`、有全局 token 仍单 bot;`configure` strip `/v1`、`/v1/`、不动非版本路径;`configure --model` 写 `sdk.model`、省略不写。idle 启动:集成或手测(进程存活 + `status: running`)。
- **daemon**(go test):`CcOctoConfig` 反序列化 `Model`;`handleCcOctoInstall` configure 后调 start(注入命令 mock 断言);`Provision` 不再写 `sdk.model`。
- **fleet**(go test):install 载荷 `model` relay;models 代理——SSRF 拒私网/loopback、解析 `data[].id`、网关错误透传。
- **web**(vitest):`validateCcInstall` strip `/v1`;env 默认预填 / 缺省 placeholder;combobox 拉取 + 手填 + model 透传 onSubmit。
- **全链路**:本地 env 跑 install → claude idle online → 拉模型列表 → 建 bot → 选不同 model 聊天验证生效。

## 风险 / 待定

- models 代理路径名 / 归属(fleet `/runtimes/llm-models` vs 其它)在 plan 阶段定稿。
- cc idle 保活实现细节(never-resolving promise vs 空 interval)在 plan 阶段定。
- `VITE_*` env 变量名最终命名在 plan 阶段定。
- 网关 `/v1/models` 返回为 OpenAI 风格(`data[].id`);若未来支持的网关返回 Anthropic 风格,代理解析需兼容(本轮先支持已验证的 OpenAI 风格,Anthropic 风格留待出现时扩展,并 log 未识别结构)。
