# Octo Agent Runtime — Plan

> 状态：已落地 · 最近更新：2026-06-02
> 配套架构图：[`ARCHITECTURE.html`](./ARCHITECTURE.html)
>
> 本文是**唯一 plan**：所有跨服务契约、状态机、JWT canonical、guardrail 数值以本文为准；与代码冲突时以本文优先，先改 plan 再改代码。
>
> 实施进度：PR-A.1 / A.2 / A.3 / B / C 全部 e2e 通过（2026-06-01 ~ 06-02）。各 repo HEAD 见 §12。

---

## 0. 设计原则 — 为什么 daemon pull · 为什么 0 通信

最初方案曾把 octo-fleet 设计成 push 网关：matter → outbox → fleet → daemon。复杂度过高：

- matter 要写 `runtime_event_outbox` 表 + outbox worker + 重试 / 去重 / DLQ
- fleet 要做 HMAC attestation 回写 matter timeline
- matter 要做本地 HMAC 验签 + idempotency 去重
- 失败爆炸半径大：outbox 卡死 = 全停

定稿方案把 octo-fleet 降级为「daemon 注册中心 + bot 编排元数据表」：

- **3 个后端服务（server / fleet / matter）零业务 HTTP 互调**（仅 fleet/matter 启动时拉 server `/.well-known/jwks.json` 做公钥分发，运行期 0 业务调用）
- daemon 是唯一协调者：**主动 pull** matter 任务 + 直接 POST writeback；同时直接调 server 拿 JWT / bot_token
- 跨服务信任：**server 是唯一 JWT issuer**，fleet/matter 各自拉 server 的 `/.well-known/jwks.json` 缓存公钥，本地验签
- bot_token 永不离开 server DB（不流经浏览器、不流经 fleet）

**一句话链路**：

```text
matter 评论里 @bot
  -> matter 同事务写 matter_bot_task (status=queued)
  -> daemon 心跳后从 fleet 拿 managed_bots 列表
  -> daemon GET matter /api/v1/internal/bot-tasks?bot_uid=X (Bearer daemon JWT)
  -> daemon spawn openclaw agent (本地)
  -> daemon POST matter /api/v1/internal/matters/:id/timeline 写回 bot 回复
  -> daemon POST matter /api/v1/internal/matters/:id/activities 写 agent_task_completed
  -> daemon POST matter /api/v1/internal/bot-tasks/:id/ack (claim_token guard)
```

---

## 1. 服务边界

### octo-server（瘦身后）

- user / IM 账户 / bot 凭据（**bot_token 留 `robot.bot_token` 列，永不外发**）
- space membership / auth / api-key
- **新增 `modules/auth_jwt`**：
  - `POST /v1/auth/token`：用 session（web 登录后）**或** api_key（daemon 启动）换 RS256 JWT；scope 由凭据类型决定（session → `web`，api_key → `daemon`）
  - `GET /.well-known/jwks.json`：暴露公钥（fleet/matter 拉取并缓存）
  - `POST /v1/bot/mint`：web session 调，OBO 创建 IM bot，**返回 `bot_uid`（不返回 bot_token）**
  - `GET /v1/bot/:uid/token`：daemon JWT 调，校验 `robot.creator_uid == JWT.sub` 后返回 `bot_token`
- **删除 `modules/runtime/`**（PR-C 完成）；schema migration 残留行可手动 `DELETE FROM gorp_migrations WHERE id LIKE 'runtime-%'`

### octo-fleet（新仓库）

- 端口 :8092，独立 MySQL schema (`octo_fleet`)
- `agent_runtime` / `bot` / `bot_task`(deprecated) 等表
- daemon endpoints：register / heartbeat / deregister / ping / upgrade / bot-ack
- web endpoints：runtime/bot CRUD + 3-step bot 创建的 fleet 部分（`POST /v1/runtimes/bots`、`POST /v1/runtimes/bots/:id/mint`）
- heartbeat 响应包含 `managed_bots` 列表 + 可选 `pending_command` (bot.provision)
- `internal/auth` 拉 server JWKS 缓存 + 本地验签 middleware

### octo-matter

- 原有：matter / timeline / activity / outputs
- **新增 `matter_bot_task` 表**（PR-B.1）：matter 评论 @bot 时**同事务**入队
- **新增 daemon endpoints**（JWT auth）：`GET /api/v1/internal/bot-tasks` + `POST /api/v1/internal/bot-tasks/:id/ack`
- 现有 `POST /api/v1/internal/matters/:id/timeline` + `/activities` daemon 用于 writeback —— **DualAuth 中间件**接受 daemon JWT 或 X-Internal-Token 任一；daemon 走 JWT，老调用方走 token，互不影响
- `internal/auth/jwt.go` 同 fleet 一样拉 server JWKS

### 谁不做什么

- **server 不知道 fleet 存在**：只暴露 mint / token / token-exchange 端点等被动 endpoint
- **fleet 不调 server / matter**（唯一例外：feed proxy，§7）
- **matter 不调 fleet / server**：只被 daemon pull / push
- **daemon 直接调 3 个后端**：自己持 api_key → 换 JWT → 同一 JWT 调 fleet/matter
- 3 个后端**都不读对方 DB**

### 模块边界纪律

- matter / fleet 任何想加"matter→fleet webhook" / "fleet→matter HTTP" 的需求 → 改成 daemon pull
- fleet 不持有 task / writeback 逻辑；它的世界里 bot 只是个"能 provision 的标识符"
- server 不感知 fleet：只暴露 web/daemon 直接调的 endpoint，不需要 X-Internal-Token

---

## 2. 信任链 / Auth

**核心**：server 是唯一 JWT issuer（RS256），所有其他服务拉 JWKS 本地验签。**业务路径 0 服务间 HTTP 调用**（JWKS 拉取属于公钥分发基础设施，每个 verifier 启动时一次 + unknown kid 触发，不算业务调用）。

### Web 用户流程

```text
浏览器 → server POST /v1/user/login → session token (旧机制，不变)
浏览器 → server POST /v1/auth/token (body={session_token, space_id}) → JWT (web 作用域, 30min)
浏览器 → fleet  : Bearer <JWT>  (fleet 拉 JWKS 本地验签, 缓存 kid)
浏览器 → matter : Bearer <JWT>  (matter 同上)
浏览器 → server : session token (server 自己的内部业务仍用 session)
```

octo-web 的 `APIClient.ts` 加了 async interceptor：URL 匹配 `/runtimes` 或 `/daemon` 时自动注入 `Authorization: Bearer <JWT>`，JWT 在内存中按 60s 余量自动刷新。

### Daemon 流程

```text
daemon 启动 (持 api_key)
daemon → server POST /v1/auth/token (body={api_key, daemon_id}) → JWT (daemon 作用域, 30 天)
daemon → fleet  POST /v1/daemon/register (Bearer JWT)
daemon → fleet  POST /v1/daemon/heartbeat (Bearer JWT, 每 15s)
                 ↳ 响应含 managed_bots + 可选 pending_command(bot.provision)
daemon → matter GET  /api/v1/internal/bot-tasks?bot_uid=X (Bearer JWT)
daemon → matter POST /api/v1/internal/bot-tasks/:id/ack    (Bearer JWT)
daemon → matter POST /api/v1/internal/matters/:id/timeline   (Bearer JWT, DualAuth)
daemon → matter POST /api/v1/internal/matters/:id/activities (Bearer JWT, DualAuth)
daemon → server GET  /v1/bot/:uid/token (Bearer JWT) ← 拉 bot_token 喂给 openclaw
```

### JWT claims

```json
{
  "iss": "octo-server",
  "sub": "<uid>",
  "iat": 1780317312,
  "exp": 1780319112,
  "scope": "web" | "daemon",
  "space_id": "<space uuid>",
  "daemon_id": "<daemon uuid, 仅 daemon scope>"
}
```

### Auth 不变量

- **AU1**：JWT 过期 / kid 未知 / 签名失败 → 401，daemon 收到 401 后自动刷新 JWT 重试
- **AU2**：unknown kid 触发一次 JWKS refresh（10s floor 防 issuer DoS），仍失败 → 401
- **AU3**：fleet/matter 拿到的 JWT.space_id 直接信任（issuer 已校验成员关系），不再查 space_member 表
- **AU4 (修订)**：daemon 必须直连 server（拿 JWT + 拉 bot_token）。fleet/matter 在**业务路径**上仍不调 server（启动期拉 JWKS 是公钥分发，不在此约束内）

### 为什么两种协议并存 (Why two protocols)

> 这一节回答："既然 server 已经有 session token 了，为啥要引入 JWT？"

**session token** 是 lookup-mode auth：token 本身是无意义随机串，含义全在 server 的 Redis 里 (`token:xxx → {uid, name}`)。优势：删 key 即吊销、长期已有大量客户端 (iOS/Android/老服务) hard-code 用法；劣势：**任何想验证它的服务必须能读 server 的 Redis 或网络可达 server**。

**JWT** 是 self-contained auth：身份信息直接打包进 token，server 用私钥签名，任何拿到 JWKS 公钥的服务都能本地验签。优势：跨服务零 HTTP 互调；劣势：发出去的 JWT 在 TTL 内**无法立即吊销**（要做立即吊销需要 JTI blacklist / version claim 这些复杂机制）。

**为啥 fleet/matter 必须用 JWT 而非 session**：

- 每个请求都 fleet → server "这 token 是谁" 是额外 HTTP 跳，破 0 通信纪律
- server 一挂，fleet/matter 全员 auth 中断，从 3 个独立服务退化成"server + 2 个小弟"
- session token 没有 scope 概念，daemon 拿到等于全权，没法限"只能 pull bot_task / 写 timeline"

**为啥 server 业务 API 不切 JWT**：

- iOS / Android / 老 octo-xx 服务 hard-code 了 session 用法，全量切换 = 多端版本协同灾难
- session 一删 key 即吊销，比 JWT 立即吊销实现简单很多
- server 现有中间件链全按 session 设计，重写没收益（这条路本来就 work）

**结论**：session 是 octo-server 业务主协议，**JWT 加在新链路**（web↔fleet/matter、daemon↔ 三后端、服务间信任传递）。具体落地见 §7 DualAuth — matter writeback 双协议并存，新调用方走 JWT，老调用方走 X-Internal-Token，**0 cutover 风险**。

### JWT TTL 和撤销窗口

| token 类型 | TTL | 撤销路径 |
|---|---|---|
| 浏览器 web JWT | 30 分钟 | 用户登出 → session 失效 → 下次换 JWT 失败；存量 JWT 最长 30 分钟内自然过期 |
| daemon JWT | 30 天 | 管理员禁用 `user_api_key` → daemon 下次换 JWT 失败；**存量 JWT 最长 30 天内自然过期** |

⚠️ **撤销窗口风险**：daemon api_key 一旦泄露，攻击者已经换出去的 JWT **最长还能用 30 天**（即使你已经撤了 api_key）。

权衡 (待生产部署前决策)：

- **30 天**（PoC 当前）: daemon 每月才换一次，server `/v1/auth/token` 压力小；坏处是上面那个窗口
- **24 小时**: 把窗口砍到 1 天，代价是 daemon 每天多 1 次 `/v1/auth/token` 调用，量级可忽略 — **生产推荐**
- **JTI blacklist**: 即时吊销，需在 server / matter / fleet 共享一个 revocation list (Redis)，实现复杂度上去 — 仅在确实需要"立即吊销"语义时启用

---

## 3. Bot 创建：3-step web 编排（无服务间调用）

mint OBO 流程**不走 fleet → server**，而是浏览器编排 3 步，bot_token 全程不进浏览器。

```text
1. 浏览器 → fleet  POST /v1/runtimes/bots {runtime_id, name, runtime_kind}
                    ↳ fleet 落 bot 行 (status='draft', bot_uid 暂空)
                    ↳ 返回 {id: <fleet bot.id>}

2. 浏览器 → server POST /v1/bot/mint {display_name, space_id}
                    ↳ server 调 botfather.MintBotOBO 内部函数
                    ↳ 创建 IM user + 写 robot.bot_token + space_member + 互相 friend
                    ↳ 返回 {bot_uid}  ← bot_token 留 server DB

3. 浏览器 → fleet  POST /v1/runtimes/bots/:id/mint {bot_uid}
                    ↳ fleet UPDATE bot SET bot_uid=?, status='bot_minted'
                    ↳ 触发 bot.provision pending command (留待下次 heartbeat 派发)
                    ↳ 返回更新后的 bot 元数据

4. daemon 心跳收到 pending_command
                    ↳ 看到 bot_token 为空 (fleet 不存 token)
                    ↳ daemon → server GET /v1/bot/:bot_uid/token (Bearer daemon JWT)
                                       ↳ server 校验 robot.creator_uid == JWT.sub → 返回 bot_token
                    ↳ daemon openclaw agents add + config patch + agents bind
                    ↳ daemon → fleet POST /v1/daemon/bots/:id/ack {claim_token, status='active'}
                                       ↳ fleet UPDATE bot SET status='active' (触发 §6 状态机转 active)
```

### Failure modes

| 阶段失败 | 状态 | 影响 |
|---|---|---|
| Step 1 fleet 落表失败 | fleet 无行 | 用户重试，幂等（name 不唯一也能再来） |
| Step 2 server mint 失败 | fleet 有 draft 行，server 无 IM 账户 | 用户 retry 或 sweeper 清 draft |
| Step 3 fleet patch 失败 | server 有 IM 账户，fleet 行未升级 | 用户 retry（PATCH 幂等）|
| Step 4 daemon 拉 bot_token 失败 | fleet 已派 pending，daemon 重试 | 自动恢复 |
| Step 4 daemon openclaw 失败 | fleet 行 status='failed' | UI 显示，用户归档重建 |

---

## 4. matter 端 `matter_bot_task` 表

> **同名歧义**：本节 `claim_token` 是 **matter** `matter_bot_task` 维度（daemon 拉 task 时颁发），跟 §6 里 fleet `bot.claim_token`（bot.provision 派发时颁发，语义上更像 `provision_token`）**不是同一个**。两个 token 互不通用、寿命也不同。后续 cleanup 计划把 fleet 那个改名 `provision_token`（在「后续讨论项」里）。

### Schema (`octo-matter/migrations/008_bot_task.sql`)

```sql
CREATE TABLE matter_bot_task (
  id                CHAR(36)     NOT NULL,
  matter_id         CHAR(36)     NOT NULL,
  space_id          VARCHAR(64)  NOT NULL,
  bot_uid           VARCHAR(64)  NOT NULL,
  trigger_kind      VARCHAR(32)  NOT NULL DEFAULT 'mention',   -- 'mention' | 'assignee_added'
  trigger_entry_id  CHAR(36)     NULL,                          -- timeline entry id
  prompt            MEDIUMTEXT   NOT NULL,
  matter_title      VARCHAR(255) NOT NULL DEFAULT '',
  status            VARCHAR(16)  NOT NULL DEFAULT 'queued',     -- queued/dispatched/succeeded/failed
  claim_token       VARCHAR(64)  NULL,
  claimed_by        VARCHAR(64)  NULL,
  claimed_at        DATETIME(3)  NULL,
  lease_until       DATETIME(3)  NULL,
  attempt           INT          NOT NULL DEFAULT 0,
  max_attempts      INT          NOT NULL DEFAULT 3,
  error_msg         TEXT         NULL,
  result_summary    TEXT         NULL,
  elapsed_ms        BIGINT       NULL,
  created_at        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_bot_status (bot_uid, status, created_at),
  KEY idx_matter (matter_id),
  KEY idx_claim_lease (status, lease_until),
  -- 同一 (matter, bot, trigger) 只入队一次；NULL trigger_entry_id 允许多次（assignee 反复添加）
  UNIQUE KEY uk_trigger (matter_id, bot_uid, trigger_entry_id)
);
```

### 写入路径（matter timeline_handler + matter_handler）

**@mention 路径**（`timeline_handler.dispatchMentionedAgents`）：

```text
POST /api/v1/matters/:id/timeline
  ↓ TimelineService.Create (Tx)
    ├─ INSERT matter_timelines
    ├─ INSERT matter_activities (kind='comment')
    COMMIT
  ↓ 异步 worker
    └─ FOR EACH [@bot](mention://agent/<bot_uid>) in content:
         build continuation prompt
         INSERT matter_bot_task (status='queued')   ← uk_trigger 触发幂等
```

**assignee 路径**（`matter_handler.dispatchOneBotAssignee`，PR-B.4.5 修复）：

```text
POST /api/v1/matters  (或 POST .../assignees)
  ↓ 异步 worker
    └─ FOR EACH assignee with `_bot` suffix:
         build minimal "you've been assigned" prompt
         INSERT matter_bot_task (status='queued', trigger_kind='assignee_added')
```

> **核心不变量**：评论/事项的持久化和 bot_task 入队 **要么走同事务，要么走同一 worker 不会丢的异步**。messaging 损失靠 sweeper（§8）兜底。

### Daemon pull 协议

```text
GET /api/v1/internal/bot-tasks?bot_uid=<X>&limit=10
Header: Authorization: Bearer <daemon JWT>

Resp (200): {
  tasks: [
    {
      id, matter_id, space_id, bot_uid,
      prompt, matter_title,
      claim_token,           # daemon 必须在 writeback 时回传
      lease_until,           # daemon 必须在此时间前完成 (默认 10min)
    },
    ...
  ]
}
```

#### Atomic claim（matter 端 SQL）

```sql
-- 一条 SQL 原子 claim
UPDATE matter_bot_task
   SET status='dispatched', claim_token=?, claimed_by=?, claimed_at=NOW(3), lease_until=?
 WHERE bot_uid=? AND status='queued'
 ORDER BY created_at ASC
 LIMIT ?;
-- 然后 SELECT status='dispatched' AND claim_token=<刚生成的批次 token>
```

### Writeback + Ack 协议

```text
# 成功
POST /api/v1/internal/matters/:id/timeline   (Bearer daemon JWT, DualAuth)
  Body: { actor_uid: <bot_uid>, space_id, content: <agent reply> }
POST /api/v1/internal/matters/:id/activities (Bearer daemon JWT, DualAuth)
  Body: { actor_uid: <bot_uid>, action: 'agent_task_completed',
          detail: { bot_uid, task_id, elapsed_ms, bytes } }
POST /api/v1/internal/bot-tasks/:id/ack       (Bearer daemon JWT)
  Body: { claim_token, status: 'succeeded', result_summary, elapsed_ms }

# 失败
POST /api/v1/internal/matters/:id/activities (Bearer daemon JWT, DualAuth)
  Body: { actor_uid: <bot_uid>, action: 'agent_task_failed', detail: {...} }
POST /api/v1/internal/bot-tasks/:id/ack       (Bearer daemon JWT)
  Body: { claim_token, status: 'failed', error_msg, elapsed_ms }
```

> writeback 端点采用 **DualAuth 中间件**：先尝试 `Authorization: Bearer <JWT>`，无 Bearer header 时 fallback `X-Internal-Token`。daemon 一律走 JWT（用户机器上 0 shared secret）；fleet bot-feed proxy / 老服务保持 X-Internal-Token 不动。两条协议同 URL 共存、无 cutover 风险。

#### Task 不变量

- **T1**：ack 时 `claim_token` 不匹配 → 409，daemon 必须丢弃结果不重试（lease 过期被其他 daemon 拿走的情形）
- **T2**：`lease_until` 过期由 matter sweeper（§8）reclaim 回 `queued`，`attempt++`，超 `max_attempts` 标 failed
- **T3**：同 (matter_id, bot_uid, trigger_entry_id) 在 UNIQUE 上 dedup；`trigger_entry_id IS NULL` 允许多次（assignee 多次添加）
- **T4**：timeline / activity / ack 三个写入不要求原子；daemon 失败重试，幂等性靠 claim_token 一次性

---

## 5. Daemon claim 循环

```text
loop {
  # 每 N 秒
  for runtime in registered_runtimes:
    resp = fleet.POST /v1/daemon/heartbeat {runtime_id} (Bearer JWT)
    if resp.PendingCommand?.action == "bot.provision":
        go handleBotProvision(resp.PendingCommand)
    if len(resp.ManagedBots) > 0:
        go pollMatterTasksForManagedBots(resp.ManagedBots)
}

func pollMatterTasksForManagedBots(bots):
  for bot in bots:
    tasks = matter.GET /api/v1/internal/bot-tasks?bot_uid=bot.bot_uid (Bearer JWT)
    for task in tasks:
       reply = runOpenclawAgent(bot.workspace_id, task.prompt)
       matter.POST /api/v1/internal/matters/:matter_id/timeline   (Bearer JWT, DualAuth)
       matter.POST /api/v1/internal/matters/:matter_id/activities (Bearer JWT, DualAuth)
       matter.POST /api/v1/internal/bot-tasks/:task_id/ack         (Bearer JWT)
```

### Daemon 怎么知道自己管理哪些 bot

`POST /v1/daemon/heartbeat` 响应里 fleet 注入 `managed_bots`：

```json
{
  "status": "ok",
  "managed_bots": [
    {"bot_uid": "27xxx_bot", "workspace_id": "demo-f1c7"},
    ...
  ],
  "pending_command": { ... }  // 可选 bot.provision
}
```

fleet 用 `SELECT bot_uid, workspace_id FROM bot WHERE daemon_id=? AND status='active'` 生成。

### Heartbeat 节奏

- 默认 15s 一次（`OCTO_HEARTBEAT_INTERVAL` 可调）
- 每次心跳后逐 bot pull matter（n+1 HTTP，可接受；每 bot 5 task limit）
- 长任务（openclaw agent 跑 10min）期间 daemon 继续 heartbeat；matter sweeper 见 §8

---

## 6. Bot 生命周期

### 状态机（简化后实际实现）

```text
[draft] → [bot_minted] → [dispatched] → [active] ←→ [archived]
              │              │              │
              └──→ [failed] ←┴──────────────┘
```

| 状态 | 含义 | 触发 |
|---|---|---|
| `draft` | 浏览器 step 1 完成，fleet 落了行没 bot_uid | `POST /v1/runtimes/bots` |
| `bot_minted` | 浏览器 step 3 patch 上 bot_uid，等 daemon 心跳取 | `POST /v1/runtimes/bots/:id/mint` |
| `dispatched` | daemon heartbeat 拿到 pending_command 拉走 | fleet `bot.claim_token` 颁发（语义上是 provision_token，§4 注） |
| `active` | daemon openclaw bind 成功 ack 回 fleet | daemon ack 200 |
| `failed` | mint 失败 / provision 失败 | server 4xx / daemon ack 'failed' |
| `archived` | 浏览器删除 | `DELETE /v1/runtimes/bots/:id` |

### `bot` 表 schema（fleet 端）

```sql
CREATE TABLE bot (
  id              BIGINT AUTO_INCREMENT PRIMARY KEY,
  space_id        VARCHAR(64)  NOT NULL,
  owner_uid       VARCHAR(64)  NOT NULL,
  runtime_id      BIGINT       NOT NULL,
  runtime_kind    VARCHAR(32)  NOT NULL,        -- 'openclaw' | 'claude' | 'codex' | 'hermes'
  daemon_id       VARCHAR(64)  NOT NULL,
  name            VARCHAR(120) NOT NULL,
  bot_uid         VARCHAR(64)  NOT NULL DEFAULT '',   -- step 3 填
  bot_token       VARCHAR(120) NOT NULL DEFAULT '',   -- 始终为空，PR-B 后死字段
  workspace_id    VARCHAR(64)  NOT NULL DEFAULT '',   -- openclaw workspace
  status          VARCHAR(32)  NOT NULL,
  claim_token     VARCHAR(64)  NOT NULL DEFAULT '',  -- 语义上是 provision_token (§4 注)，未来 rename
  error_msg       TEXT,
  created_by      VARCHAR(32)  NOT NULL DEFAULT 'web',
  created_at      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  KEY idx_runtime (runtime_id, status),
  KEY idx_bot_uid (bot_uid)
);
```

> `bot.bot_token` 列保留是 PoC4 残留，PR-B 后无人写入。可在 PR-D cleanup 删掉（一行 migration）。

### `bot.provision` heartbeat command

```json
{
  "id": <fleet bot.id>,
  "action": "bot.provision",
  "workspace_id": "<derived from name, e.g. demo-f1c7>",
  "display_name": "<bot name>",
  "bot_uid": "<server minted>",
  "bot_token": "",                  // 始终空，daemon 主动拉
  "api_url": "<server external base>",
  "claim_token": "<uuid>"
}
```

daemon 收到后：
- 若 `bot_token == ""`：先 `GET /v1/bot/:bot_uid/token` 拉
- `api_url == ""` fallback `OCTO_SERVER_URL` env
- 对 openclaw：`openclaw agents add <workspace>` + patch `channels.octo.accounts.<bot_uid>` + `openclaw agents bind --agent <workspace> --bind octo:<bot_uid>`
- 对 claude/codex/hermes：当前 PoC 不支持，UI 在 CreateBotModal 显示「暂不支持」

#### Bot 不变量

- **B1**：`bot.bot_uid` 全局唯一（server 端 robot 表 UNIQUE）
- **B2**：`bot.runtime_id` 必须 = `agent_runtime` 表中同 owner 的行 id
- **B3**：`bot.status='archived'` 时 fleet 不再把它放进 `managed_bots`，daemon 自然停止 pull

---

## 7. Matter 写回：DualAuth（JWT 优先，X-Internal-Token fallback）

writeback 端点 `POST /api/v1/internal/matters/:id/timeline` + `/activities` 装上**DualAuth 中间件**：

- 请求带 `Authorization: Bearer <jwt>` → 走 daemon JWT 验签路径
- 否则 → fallback 原有 `X-Internal-Token` 校验

daemon 一律走 JWT — 用户机器上**零 shared secret**（BotFather 安装命令不再要 `NOTIFY_INTERNAL_TOKEN` env）。fleet 的 bot-feed proxy、其他还在用 X-Internal-Token 的服务保持不动，**无 cutover 风险**。

新增文件：`octo-matter/internal/auth/dual_auth.go`（~15 行）。daemon 端 `matterInternalPost` 改用 `EnsureJWT` 拼 Bearer header。

### Writeback context binding（防 actor_uid 伪造）

DualAuth 把 timeline / activity 端点向 daemon JWT 开放后，"任何持有合法 JWT 的 daemon 都能在任意 bot 名下写"的风险曾出现 — 因为老 X-Internal-Token 路径默认 caller 是 trusted server，body 里的 `actor_uid` 当真。修正:

- 请求体新增 `task_id` + `claim_token` 字段
- DaemonJWTMiddleware 把 `daemon_id` claim 注入 `gin.Context`
- handler 检测到 `daemon_id` 时（即走 JWT 分支），调用 `BotTaskRepo.LoadDispatchedForWriteback(task_id, claim_token)` 拿到关联的 `matter_bot_task` 行
- 强制 4 项 invariants — 任意一项不通过返 **403 WRITEBACK_FORBIDDEN**:

| invariant | 防的攻击 |
|---|---|
| `row.bot_uid == body.actor_uid` | daemon 在别人 bot 名下伪造写入 |
| `row.space_id == body.space_id` (若 body 带) | 跨 space 写入 |
| `row.claimed_by == JWT.daemon_id` | daemon B 抢 daemon A 的 task 写入 |
| `claim_token == row.claim_token AND status='dispatched'` | task 已被 sweeper 回收 / 已 ack 后仍想写 |

X-Internal-Token 路径**不受影响** — 没 `daemon_id` claim，校验函数直接 return nil。

> **已知行为 (非 bug)**: `LoadDispatchedForWriteback` 返回 task 行之后到 `CreateInternalEntry` 之间存在小 race window — sweeper 此刻 reclaim 这条 task，timeline insert 仍会成功（insert 不绑 task status）。后果: lease 过期 + sibling daemon 已重跑 → 两条回复都 append 到 timeline，actor 仍合法 bot。可接受 — writeback 不保证幂等是设计约束，由 daemon 端 lease 续约 / sweeper 阈值控制概率。

### 升级顺序约束（重要 — DualAuth 唯一 cutover 风险）

DualAuth 对**老调用方 (fleet bot-feed proxy / 任何还用 X-Internal-Token 的服务) 0 cutover**，但**新链路** (daemon JWT writeback) 有强升级顺序:

1. **server** 升级 (拿 v2 session fix + JWKS 已就绪) — 否则 web 用户拿不到 JWT，进不去 Runtimes 页
2. **matter** 升级 + 配 `OCTO_SERVER_JWKS_URL` env (DualAuth 中间件就绪)
3. **daemon** 升级 (切 JWT writeback)

反过来不行：daemon 先升级 → matter 还没 DualAuth → daemon Bearer JWT 请求**全 401** → bot 回复永远不到 matter，timeline / activity 全丢。

部署 runbook 必须按上面顺序；rollback 反方向 (daemon → matter → server)。

> 唯一例外：fleet 的 `GET /v1/runtimes/bots/:id/feed` 是 proxy 到 matter `GET /api/v1/internal/bots/:bot_uid/feed`（X-Internal-Token），matter URL 从 `OCTO_MATTER_URL` env 兜底。这条违反 0 通信，未来改成浏览器直接调 matter。

---

## 8. Sweeper（matter 5min 周期）

`octo-matter` 跑一个 5 分钟 tick 的 sweeper（`BotTaskRepo.ReclaimExpired`）：

```sql
-- 先 dead-letter (避免下一步又 reclaim 已到 cap 的行)
UPDATE matter_bot_task
   SET status='failed', error_msg='exceeded max_attempts'
 WHERE status='dispatched' AND lease_until < NOW(3) AND attempt+1 > max_attempts;

-- 再 reclaim
UPDATE matter_bot_task
   SET status='queued', attempt=attempt+1, claim_token=NULL, claimed_by=NULL, claimed_at=NULL, lease_until=NULL
 WHERE status='dispatched' AND lease_until < NOW(3);
```

无需 audit worker — `matter_bot_task` 是 ground truth，web UI bot 详情页直接 SELECT 显示。

---

## 9. Daemon 执行侧安全约束

- agent run 用 ACP fresh session（每个 task 独立 session_key）
- `runOpenclawAgent` 子进程 10min hard timeout（`exec.CommandContext` + `context.WithTimeout`）
- stdout 截 `maxResultSummaryBytes = 64KB`（`matter activity.detail.bytes` 字段记录原始长度）
- agent 端工具调用（grep / bash / etc）由 openclaw 自己 approval 系统约束，daemon 不再加层
- `claim_token` / JWT 永不打到日志

---

## 10. 配置项汇总

> **端口免责**：以下端口为本文 PoC / 本地 dev 默认（server `:8090`、matter `:8080`、fleet `:8092`）。生产部署由 deployment 仓库的 docker-compose / k8s manifest 决定，可能不同；env 名保持稳定。

### octo-server

```env
JWT_PRIVATE_KEY_PATH=~/.octo-server/jwt-priv.pem  # 默认；首次启动自动生成 RSA-2048
# (其他 server 既有配置不变)
```

### octo-fleet

```env
OCTO_MATTER_URL=http://127.0.0.1:8080             # bot feed proxy 兜底用
NOTIFY_INTERNAL_TOKEN=<shared-secret>             # bot feed proxy 调 matter 时用
# configs/fleet.yaml 里还有：
#   addr: ":8092"
#   db.mysqlAddr: "root:demo@tcp(127.0.0.1:3306)/octo_fleet?..."
#   db.redisAddr: "127.0.0.1:6379"
#   auth.serverJwksURL: "http://localhost:8090/.well-known/jwks.json"
```

### octo-matter

```env
OCTO_SERVER_JWKS_URL=http://localhost:8090/.well-known/jwks.json  # daemon JWT 验签用
NOTIFY_INTERNAL_TOKEN=<shared-secret>                              # X-Internal-Token fallback（fleet bot-feed proxy 仍用）
# (其他 matter 既有配置不变)
```

### octo-daemon-cli

```env
OCTO_FLEET_URL=http://127.0.0.1:8092       # 心跳 / register / bot ack
OCTO_SERVER_URL=http://127.0.0.1:8090      # 拿 JWT / 拉 bot_token
OCTO_MATTER_URL=http://127.0.0.1:8080      # 拉 bot_task / writeback
# api_key 在 ~/.octo-daemon/config.json，启动时换 JWT
# 不再需要 NOTIFY_INTERNAL_TOKEN — DualAuth 让 writeback 也走 daemon JWT
```

### octo-web

```env
# 不需要 ENV，所有改动在 vite.config.ts 的 dev proxy + src/Service/APIClient.ts 的 interceptor
# 生产部署时 nginx 把 /api/v1/runtimes* / /api/v1/daemon* 路由到 fleet :8092
```

---

## 11. 替代设计对比

下表展示 push/outbox 替代设计的复杂度差异——草稿期曾考虑，因 fleet 复杂度过高 + matter↔fleet 强耦合放弃。

| 维度 | 替代方案：push / outbox | 本 plan：daemon pull · 0 通信 |
|---|---|---|
| 触发模式 | matter outbox → fleet push | daemon pull from matter / fleet |
| matter → fleet | webhook | 无 |
| fleet → matter | HMAC writeback | 无（仅 bot feed proxy 一处，将来移除） |
| fleet → server | mint OBO + verify api-key | **无** |
| daemon → server | 无 | 有：`POST /v1/auth/token` + `GET /v1/bot/:uid/token`（必要） |
| matter 复杂度 | +outbox 表 +outbox worker +HMAC 验签 | +matter_bot_task 表 +sweeper +daemon endpoints |
| fleet 复杂度 | 高（outbox / HMAC / worker） | 中（runtime 管理 + bot 编排元数据） |
| daemon 复杂度 | 中（被推） | 中高（主动 pull + 协调）|
| 失败爆炸半径 | matter outbox 卡死 = 全停 | daemon 挂 = 单机停 |
| 时延 | push 即时 | pull 间隔 15s |
| HMAC canonical 规范 | 200+ 行 | 不需要 |

---

## 12. 实施实况（已落地）

PR 拆分按服务边界做：

| PR | 范围 | 涉及 repo | 状态 |
|---|---|---|---|
| **PR-A.1** | JWT 信任链（server issuer + fleet verifier + daemon JWT exchange） | octo-server, octo-fleet, octo-daemon-cli | ✅ |
| **PR-A.2** | runtime 业务搬 fleet + 浏览器接 JWT + daemon 切 fleet URL + server runtime deprecate | 全部 4 repo + 新增 octo-fleet | ✅ |
| **PR-A.3** | bot 创建 3-step 编排（fleet draft → server mint → fleet patch + daemon 拉 bot_token） | server, fleet, daemon, web | ✅ |
| **PR-B.1** | matter_bot_task 表 + daemon JWT endpoints + @mention 写本地 | matter | ✅ |
| **PR-B.2** | daemon 改 pull from matter（fleet 心跳吐 managed_bots） | daemon, fleet | ✅ |
| **PR-B.3** | fleet 卸载 bot_task（endpoint 410，table 留 rollback） | fleet | ✅ |
| **PR-B.4** | e2e + space_id fixup | matter, daemon | ✅ |
| **PR-B.4.5** | assignee 路径补走本地 DB（之前漏改）| matter | ✅ |
| **PR-C** | server 删 modules/runtime（27 文件 / 3819 行） | server | ✅ |

各 repo 分支：

| Repo | Branch | HEAD commit |
|---|---|---|
| octo-server | `feat/agent-runtime` | https://github.com/Mininglamp-OSS/octo-server/tree/feat/agent-runtime |
| octo-fleet (新) | `main` | https://github.com/Mininglamp-OSS/octo-fleet |
| octo-matter | `feat/agent-runtime` | https://github.com/Mininglamp-OSS/octo-matter/tree/feat/agent-runtime |
| octo-daemon-cli | `feat/agent-runtime` | https://github.com/Mininglamp-OSS/octo-daemon-cli/tree/feat/agent-runtime |
| octo-web | `feat/agent-runtime` | https://github.com/Mininglamp-OSS/octo-web/tree/feat/agent-runtime |

---

## 13. 决策记录

- [x] **服务名**：`octo-fleet`（不是 octo-runtime），端口 :8092，独立 GitHub repo (private)
- [x] **0 通信架构**：server / fleet / matter 三者之间 0 HTTP 互调；唯一例外 fleet→matter 的 bot feed proxy 是 polish 项
- [x] **server 作为 JWT issuer**：直接走最终态 RS256 + JWKS，跳过 PoC 期"调 server /v1/auth/verify"中间方案
- [x] **bot_token 留 server**：daemon 用自己的 daemon-scope JWT 拉，never 经过浏览器/fleet
- [x] **bot_task 归 matter**：写 timeline 同事务入队，daemon pull from matter，fleet 不再持有
- [x] **lease 续约**：PoC 不做（10min lease 覆盖大部分 agent run）；GA 加 `POST /api/v1/internal/bot-tasks/:id/heartbeat` refresh lease
- [x] **多 daemon 并发**：matter `ClaimNextForBot` 用 `UPDATE...WHERE status='queued' ORDER BY ... LIMIT N` 原子 claim；contract test 列入 GA blocker
- [x] **server runtime 删 vs 留**：PR-C 真删，rollback 路径靠 `LEGACY_RUNTIME_ROUTES=true` env（实际上代码已删，env 不再生效；要 rollback 需 git revert）
- [x] **writeback 端点 DualAuth**：timeline / activities 接受 daemon JWT 或 X-Internal-Token 任一；daemon 一律走 JWT，**用户机器上 0 shared secret**（NOTIFY_INTERNAL_TOKEN 从 BotFather /daemon 安装命令移除）

### 后续讨论项（暂不阻塞 GA）

- agent timeout（10min）期间 daemon crash → 失去"已部分完成"进度。GA 加 `POST /api/v1/internal/bot-tasks/:id/checkpoint` 写流式进度
- 多个 bot 在同一 daemon 上并行 task 的资源限额（openclaw gateway scope upgrade 撞车 PoC 实测）
- JWT key rotation 协调机制（PoC 无所谓）
- bot feed proxy 改成浏览器直接调 matter（彻底 0 fleet→matter，顺带把 fleet 的 X-Internal-Token 也撤掉）
- bot table 字段瘦身（bot_token 死字段删除；fleet `bot.claim_token` rename `provision_token` 消歧 §4 注）
- 单元测试补齐（auth_jwt / JWTVerifier / BotTaskRepo / DualAuth — 当前覆盖 0%）
