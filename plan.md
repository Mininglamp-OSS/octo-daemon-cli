# Octo Agent Runtime — Plan

> 状态：定稿 · 作者：Q-daemon + Q-daemon-R · 日期：2026-06-01
> 配套架构图：[`ARCHITECTURE.html`](./ARCHITECTURE.html)
>
> 本文是**唯一 plan**：所有跨服务契约、状态机、JWT canonical、guardrail 数值以本文为准；与代码冲突时以本文优先，先改 plan 再改代码。

---

## 0. 设计原则 — 为什么 daemon pull 而不是 service push

草稿阶段曾把 octo-fleet 设计成 push 网关：matter → outbox → fleet → daemon。复杂度过高：

- matter 要写 `runtime_event_outbox` 表 + outbox worker + 重试 / 去重 / DLQ
- fleet 要做 HMAC attestation 回写 matter timeline
- matter 要做本地 HMAC 验签 + idempotency 去重
- 失败爆炸半径大：outbox 卡死 = 全停

定稿方案把 octo-fleet 降级为「daemon 注册中心 + bot↔agent 映射表」：

- matter ↔ fleet **互不知道对方存在**（无 outbox · 无 webhook · 无 HMAC）
- daemon 是唯一协调者：**主动 pull** matter 任务 + 直接 POST writeback
- fleet 唯一保留的服务间调用是 `fleet → server` mint bot OBO（绕不开）
- 跨服务信任：fleet 颁发 JWT · matter 共享公钥本地验签

**一句话链路**：

```text
matter 评论里 @bot
  -> matter 同事务写 bot_task 表 (status=queued)
  -> daemon 心跳间隙 pull GET /matters/internal/tasks?bot_uid=X
  -> daemon spawn openclaw agent (本地)
  -> daemon POST /matters/:id/timeline 写回 bot 回复
  -> daemon POST /matters/:id/activities 写 agent_task_completed
```

PoC 期（当前代码）走的是「server 内嵌路径」（bot_task 表在 server 里），cutover 时按本 plan 拆出 fleet + 把 bot_task 表搬到 matter。

---

## 1. 服务边界

### octo-matter 负责

- matter / timeline / activity / outputs 的存储 + API
- **`bot_task` 表**：新增。matter 评论里 @bot 时**同事务**入队
- `GET /matters/internal/tasks?bot_uid=X` 给 daemon pull
- `POST /matters/:id/timeline` / `POST /matters/:id/activities` 给 daemon writeback
- mention 解析 / continuation prompt 构造

### octo-fleet 负责

- daemon register / heartbeat / runtime token 颁发
- `bot` 表：bot_uid ↔ openclaw agent_id 映射
- Web 端「智能体」tab 的 CRUD（建 bot / 改名 / 归档）
- daemon 在 heartbeat 拉 `pending_command`（bot.provision 之类的物理操作）
- 调 server mint bot OBO（唯一服务间调用）

### octo-server 负责（保持不变）

- user / bot / IM 账户
- space membership / auth / api-key
- `/v1/internal/bot/mint-obo` 给 runtime 调
- `/v1/internal/auth/api-key/verify` 给 runtime 调

### 谁不做什么

- **matter 不知道 runtime 存在** — 不调 runtime，不存 daemon 信息
- **runtime 不知道 matter 存在** — 不调 matter，不存 task 队列
- **daemon 不直接调 server** — 鉴权链走 runtime token，避免 daemon 依赖 server
- 三个服务**都不直接读其他服务的 DB**

### 模块边界纪律

- matter / runtime 之间只通过 daemon 间接耦合；任何想"加 matter → runtime webhook"的需求 → 改成 daemon pull
- runtime 不持有 task / writeback 逻辑；它的世界里 bot 只是个"能 provision 的标识符"
- server 不知道 runtime 存在；只暴露 OBO / verify internal API，调用方用 `X-Internal-Token` 验证

---

## 2. 信任链 / Auth

### Web 用户 session（不变）

```text
浏览器 → server: POST /v1/user/login → session token
浏览器 → server / matter / runtime: 都用 session token
```

server 是 session 的 issuer。matter / runtime 验证 session 的两种方式：

| 方案 | 性能 | 实现 |
|---|---|---|
| **A · 共享 JWK 本地验签** | 快（无网络） | server 签 session 时用 JWT + 公钥；matter / runtime 持有同一公钥 |
| **B · 调 server verify** | 每次 +1 跳 | matter / runtime → `POST /v1/auth/verify` |

**PoC 期选 B**（实现简单，可用 server 已有的 `/v1/auth/verify`）；**GA 切 A**（少一跳，对 matter/fleet 解耦）。

### Daemon 鉴权

```text
daemon 启动 → 持 api-key
daemon → runtime: POST /v1/daemon/register {api-key}
runtime → server: POST /v1/internal/auth/api-key/verify (X-Internal-Token)
              ↳ {uid, space_id, active}
runtime 颁发 runtime_token (JWT, 30 天)
  scope: daemon_id + space_id + runtime_id
  signed by 共享私钥 (runtime issuer)

daemon → matter: Bearer runtime_token
              ↳ matter 用同一共享公钥本地验签
              ↳ 检查 token.scope.space_id == matter 查到的 matter.space_id
daemon → runtime: Bearer runtime_token
              ↳ runtime 本地验签
```

#### Auth 不变量（必须有自动 test 覆盖）

- **AU1**：runtime_token 在 revoke / rotate / 超期 / scope mismatch 四种 case 下必须被 matter / runtime 中间件拒绝
- **AU2**：revoke 后该 token 的现有连接在下一次请求被拒绝；daemon 收到 401/403 三次后 exit 78（沿用现有行为）
- **AU3**：matter 用共享公钥验签失败时 → 401，不 fallback 调 runtime（避免 N+1）
- **AU4**：daemon 永远不直接调 server（除了 binary upgrade 这类后续可独立的场景）

---

## 3. Bot mint OBO 契约（唯一保留的服务间调用）

`fleet → server` 是唯一保留的服务间调用，因为 IM bot 只能由 server 创建（涉及 IM channel / space_member / friend 关系，fleet 没法绕）。

### Server 暴露的 internal API

```text
POST /v1/internal/bot/mint-obo
Header: X-Internal-Token: <NOTIFY_INTERNAL_TOKEN>
Body: {
  owner_uid:    string,   # 用户 uid (调用方代理的"on behalf of"主体)
  space_id:     string,
  display_name: string,
  bot_token:    string,   # caller-supplied bf_xxx token (复用 IM /newbot 体系)
}
Resp: {
  bot_uid:     string,
  bot_token:   string,    # echoed back; future-proof if server overrides
  created_at:  string,
}
```

调用方：当前 PoC 阶段是 octo-server 自己内部调用（复用 `botfather.MintBotOBO`）；cutover 后由 octo-fleet 调。

### Server schema 策略

- bot 创建时打 `origin='fleet'` 标记
- bot 不能被 web BotFather UI 编辑（避免漂移）
- bot 仍是普通 IM user（`robot=1`），不需要新表

---

## 4. matter 端 `bot_task` 表（取代 outbox）

### Schema

```sql
CREATE TABLE matter_bot_task (
  id              char(36)    NOT NULL,           -- uuid
  matter_id       char(36)    NOT NULL,
  space_id        varchar(64) NOT NULL,
  bot_uid         varchar(64) NOT NULL,           -- 目标 bot
  trigger_kind    varchar(32) NOT NULL,           -- 'mention' | 'assignee_added' | 'created'
  trigger_entry_id char(36)   NULL,               -- timeline entry / activity 触发源
  prompt          text        NOT NULL,           -- continuation prompt (已包含 history)
  status          varchar(16) NOT NULL,           -- 'queued' | 'dispatched' | 'succeeded' | 'failed' | 'cancelled'
  claim_token     varchar(64) NULL,               -- daemon 取走时 issue 的幂等 token
  claimed_by      varchar(64) NULL,               -- daemon_id
  claimed_at      datetime(3) NULL,
  lease_until     datetime(3) NULL,               -- 取走后租约到期时间
  attempt         int         NOT NULL DEFAULT 0,
  max_attempts    int         NOT NULL DEFAULT 3,
  error_msg       text        NULL,
  result_summary  text        NULL,               -- 完成后的简短 summary（详情在 timeline）
  elapsed_ms      bigint      NULL,
  created_at      datetime(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at      datetime(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_bot_status (bot_uid, status, created_at),
  KEY idx_matter (matter_id),
  KEY idx_claim (status, lease_until)
);
```

### 写入路径（matter 评论 handler）

```text
POST /api/v1/matters/:id/timeline
  ↓
service.TimelineService.Create (Tx)
  ├─ INSERT matter_timelines
  ├─ INSERT matter_activities (kind='comment')
  └─ FOR EACH @bot mention in content:
       INSERT matter_bot_task (status='queued', prompt=BuildContinuationPrompt())
  COMMIT  ← 单事务，要么全成要么全不
```

**核心不变量**：评论持久化和 bot_task 入队**同事务**，杜绝消息丢失的可能。这是 push/outbox 设计要解决的核心问题，本 plan 用 matter 自己事务直接解决。

### Daemon pull 协议

```text
GET /matters/internal/tasks?bot_uid=<X>&limit=10
Header: Authorization: Bearer <runtime_token>
       (matter 用 JWK 本地验签)

Resp (200): {
  tasks: [
    {
      id, matter_id, prompt,
      claim_token,            # daemon 必须在 writeback 时回传
      lease_until,            # daemon 必须在此时间前完成
    },
    ...
  ]
}
```

#### Atomic claim（matter 端 SQL）

```sql
-- 一条 SQL 原子 claim 一批
UPDATE matter_bot_task
SET status      = 'dispatched',
    claim_token = UUID(),
    claimed_by  = ?,
    claimed_at  = NOW(3),
    lease_until = DATE_ADD(NOW(3), INTERVAL 10 MINUTE)
WHERE bot_uid = ?
  AND status  = 'queued'
ORDER BY created_at ASC
LIMIT 10;
-- 然后 SELECT 出 status='dispatched' AND claim_token=<刚生成的批次 token>
```

### Writeback 协议

```text
# 成功
POST /matters/:id/timeline
  Body: {
    actor_uid:  <bot_uid>,
    content:    <agent reply>,
  }
POST /matters/:id/activities
  Body: {
    actor_uid:  <bot_uid>,
    kind:       'agent_task_completed',
    detail:     {bot_uid, task_id, agent_id, elapsed_ms, bytes},
  }
POST /matters/internal/tasks/<task_id>/ack
  Body: {
    claim_token: <匹配 task 的 claim_token>,
    status:      'succeeded',
    result_summary: <≤500 chars>,
    elapsed_ms:  <int>,
  }

# 失败
POST /matters/internal/tasks/<task_id>/ack
  Body: { claim_token, status:'failed', error_msg, elapsed_ms }
POST /matters/:id/activities
  Body: { actor_uid:bot_uid, kind:'agent_task_failed', detail:{error,...} }
```

#### Task 不变量

- **T1**：ack 时 `claim_token` 不匹配 → 409，daemon 必须丢弃结果不重试
- **T2**：`lease_until` 过期的 task 由 matter sweeper 重置回 `queued`，attempt++（最多 max_attempts）
- **T3**：同一 (matter_id, bot_uid, trigger_entry_id) 只入队一次（dedup by trigger_entry_id）
- **T4**：写 timeline + 写 activity + ack 顺序敏感但**不必原子**（三个独立 HTTP 请求；失败时 daemon 重试，幂等性靠 trigger_entry_id 解决）

---

## 5. Daemon claim 循环

```text
loop {
  # 每 N 秒
  for runtime in registered_runtimes:
    heartbeat(runtime_id)
       ↳ runtime 返回 pending_command (provision bot)
       ↳ 如果有 → 处理 (§6)

  for bot in my_managed_bots:    # runtime 注册时告诉 daemon 自己负责哪些 bot
    tasks = matter.GET tasks?bot_uid=bot.uid
    for task in tasks:
      result = spawn_openclaw_agent(bot.agent_id, task.prompt)
      matter.POST timeline(task.matter_id, result.reply)
      matter.POST activity(task.matter_id, kind='completed', ...)
      matter.POST tasks/<task.id>/ack(claim_token, 'succeeded', ...)
}
```

### Daemon 怎么知道自己管理哪些 bot

`POST /v1/daemon/register` response 增加 `managed_bots` 列表：

```json
{
  "runtimes": [...],
  "managed_bots": [
    {"bot_uid": "27xxx_bot", "agent_id": "openclaw-workspace-xxx"}
  ]
}
```

daemon 用这个列表去 matter pull tasks。注册/heartbeat 时 runtime 更新这个列表（用户在 web 端建/删 bot 时变化）。

### Heartbeat 节奏

- 默认 5s 一次
- pull task 节奏 = 心跳节奏（在 heartbeat 之间穿插，避免抢资源）
- 长任务（agent 跑 10min）期间继续 heartbeat，task 自己 lease 续约（POST tasks/<id>/heartbeat refresh lease_until）

---

## 6. Bot 生命周期（runtime 端）

### 状态机

```text
[draft] → [provisioning] → [bot_minted] → [provisioned] → [active] ←→ [archived]
                ↓                              ↓
            [mint_failed]               [provision_failed]
```

| 状态 | 含义 | 触发 |
|---|---|---|
| `draft` | 用户提交「创建 bot」表单，runtime 落了行还没动 | web POST /runtimes/bots |
| `provisioning` | runtime 调 server mint OBO | 紧接 draft |
| `bot_minted` | server 返回 bot_uid + bot_token，runtime 落字段 | server 200 |
| `provisioned` | runtime 已派 `bot.provision` command；等 daemon ack | heartbeat dispatch |
| `active` | daemon ack 成功，bot 可用 | daemon ack 200 |
| `archived` | 用户在 web 端归档 | user action |
| `mint_failed` | server 调用失败 | server 4xx/5xx |
| `provision_failed` | daemon ack 失败 | daemon ack 'failed' |

### `bot` 表 schema

```sql
CREATE TABLE bot (
  id              char(36)    NOT NULL,
  space_id        varchar(64) NOT NULL,
  owner_uid       varchar(64) NOT NULL,
  runtime_id      bigint      NOT NULL,           -- 关联 agent_runtime.id
  display_name    varchar(120) NOT NULL,
  provider        varchar(32) NOT NULL,           -- 'openclaw' | 'claude' | 'codex' | 'hermes'
  bot_uid         varchar(64) NULL,               -- server mint 后填
  bot_token       varchar(120) NULL,              -- 同上
  agent_id        varchar(64) NULL,               -- openclaw workspace id (仅 openclaw provider)
  status          varchar(32) NOT NULL,
  claim_token     varchar(64) NULL,               -- daemon 取走 provision 时
  error_msg       text NULL,
  created_at      datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at      datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  archived_at     datetime NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_bot_uid (bot_uid),
  KEY idx_runtime_status (runtime_id, status)
);
```

### `bot.provision` heartbeat command

```json
{
  "id": "<bot.id>",
  "action": "bot.provision",
  "provider": "openclaw",
  "agent_id": "<openclaw workspace id, daemon 创建>",
  "bot_uid": "<server minted>",
  "bot_token": "<bf_xxx>",
  "api_url": "<server external base>",
  "claim_token": "<uuid>"
}
```

daemon 收到后：
- 对 openclaw：`openclaw agents add <agent_id>` + patch `channels.octo.accounts.<bot_uid>` + `openclaw agents bind --agent <agent_id> --bind octo:<bot_uid>`
- 对 claude/codex/hermes：当前 PoC 不支持（runtime 端在 createBot 时直接 reject），未来扩展

#### Bot 不变量

- **B1**：bot.bot_uid 全局唯一（server 端约束）
- **B2**：bot.runtime_id 必须 = bot owner 在 runtime 表里能查到的 runtime 行 id
- **B3**：bot.archived_at 非空时，所有 matter 内涉及该 bot 的 dispatch 立即拒绝（matter pull 时 runtime 标记 bot 不再 managed）

---

## 7. Matter 写回：直接 POST（不要 HMAC）

替代设计（push/outbox）需要 HMAC attestation：fleet 写 matter timeline 时签整个 body，matter 验签后才写。本 plan **不做**，因为：

- daemon 用 runtime_token 调 matter，已经过 JWK 验签 → 调用方身份已确认
- HMAC 只防"重放"，可以用 `claim_token` 一次性消费做幂等（§4 已设计）
- HMAC canonical 字符串规范会占 200+ 行设计文档，砍掉减负

### 写回 API（matter 端 internal）

```text
POST /api/v1/matters/:id/timeline
Header: Authorization: Bearer <runtime_token>
Body: {
  actor_uid: <bot_uid>,           # matter 不验"actor 必须是真人"，因为 token 已限定 space
  content:   <bot reply>,
  attachments?: [...],
}
```

matter 端校验：

1. `runtime_token` JWK 验签 + scope check
2. `actor_uid` 必须是有效的 IM user（信任 token 隐含的"调用方有权代理 actor_uid"）
3. 写入 timeline + activity

```text
POST /api/v1/matters/:id/activities
Body: { actor_uid, kind, detail }

POST /api/v1/matters/internal/tasks/:task_id/ack
Body: { claim_token, status, result_summary?, error_msg?, elapsed_ms? }
```

---

## 8. Audit / 对账（简化）

替代设计（push/outbox）需要 audit worker 对账 fleet 是否漏写 timeline。本 plan 不需要：

- matter `bot_task` 表本身就是 ground truth（status='succeeded' 表示写回完成）
- web 端「智能体」详情 → Tasks tab 直接 SELECT matter_bot_task 看历史 + 失败原因
- 不需要 audit worker；如果有 task 卡在 `dispatched` > lease_until → matter sweeper 自动重置回 `queued`

唯一保留的 audit：

- matter sweeper（5min 周期）：
  - reclaim 过期 lease 的 task（status='dispatched' AND lease_until < NOW() AND attempt < max_attempts → status='queued', attempt++）
  - 标 dead-letter（attempt >= max_attempts → status='failed', error_msg='exceeded max_attempts'）

---

## 9. Daemon 执行侧安全约束

- agent run 用 ACP fresh session（每个 task 独立 session_key）
- 子进程 10min hard timeout（exec.CommandContext + ctx.WithTimeout）
- stdout 截 200KB（matter activity.detail.bytes 字段记录原始长度）
- agent 端工具调用（grep / bash / etc）由 openclaw 自己的 approval 系统约束，daemon 不再加层
- claim_token / runtime_token 永不打到日志（用 `***` 替代）

---

## 10. 配置项汇总

### octo-server（不变）

```env
NOTIFY_INTERNAL_TOKEN=<shared with matter & runtime>
```

### octo-matter（新）

```env
FLEET_JWT_PUBKEY_PATH=/etc/octo/fleet-pub.pem         # 共享公钥（JWK 验签）
# 或：直接内嵌
# FLEET_JWT_PUBKEY=-----BEGIN PUBLIC KEY-----\n...
```

### octo-fleet（新）

```env
SERVER_INTERNAL_URL=http://octo-server:8090
NOTIFY_INTERNAL_TOKEN=<shared with server>            # 调 server mint OBO 时用
FLEET_JWT_PRIVKEY_PATH=/etc/octo/fleet-priv.pem       # JWT issuer 私钥
FLEET_JWT_PUBKEY_PATH=/etc/octo/fleet-pub.pem         # 自己验签时也用
```

### octo-daemon-cli（基本不变）

```env
FLEET_URL=http://octo-fleet:8092
MATTER_URL=http://octo-matter:8080
# api_key 已存在 ~/.octo-daemon/config.json
```

---

## 11. 替代设计对比 + cutover 路径

下表展示替代设计（push / outbox）的复杂度差异 — 草稿阶段曾考虑过，因为 fleet 复杂度过高 + matter↔fleet 强耦合放弃。

| 维度 | 替代方案：push / outbox | 本 plan：daemon pull |
|---|---|---|
| 触发模式 | matter outbox → fleet push | daemon pull from matter |
| matter → fleet | webhook | 无 |
| fleet → matter | HMAC writeback | 无 |
| fleet → server | mint OBO + verify api-key | 保留（不可避免） |
| daemon → server | 无 | 无 |
| matter 复杂度 | +outbox 表 +outbox worker +HMAC 验签 | +bot_task 表 +internal endpoints |
| fleet 复杂度 | 高（outbox / HMAC / worker） | 低（daemon mgr + bot mapping） |
| daemon 复杂度 | 中（被推） | 中高（主动 pull + 协调） |
| 失败爆炸半径 | matter outbox 卡死 = 全停 | daemon 挂 = 单机停 |
| 时延 | push 即时 | pull 间隔 5-10s |
| HMAC canonical 规范 | 200+ 行 | 不需要 |

### Cutover 路径（PoC → GA）

PoC 当前代码（一切在 octo-server 里）→ 拆分的迁移步骤：

1. **新建 octo-fleet 服务**：
   - 把 server 里的 `bot_task` 表搬到 matter
   - 把 server 里的 `managed_runtime_agent` 表搬到 fleet（重命名为 `bot`）
   - 把 server 里 `modules/runtime/*` 代码搬过来（目录名后续也改为 `fleet/`）
2. **matter 加 bot_task 表 + internal endpoints**（§4）
3. **fleet 加 JWT issuer**（§2）+ matter 加 JWK 验签
4. **daemon 改为主动 pull**（§5）
5. **PoC 期保留** server 内嵌路径作为 fallback；新 client 全部走 fleet
6. **数据迁移**：server `managed_runtime_agent` → fleet `bot`（一次性脚本）

---

## 12. 契约 PR 三件套

按服务拆 3 个 PR（每个独立可 merge）：

- **PR-A · octo-fleet 独立化**：新服务骨架 + bot CRUD + bot.provision 流程 + JWT issuer
- **PR-B · octo-matter bot_task**：新增表 + 写时入队 + internal pull/ack endpoints + JWK 验签
- **PR-C · octo-daemon claim loop**：从 runtime 拿 managed_bots + matter pull tasks + writeback

PR 顺序：A → B → C（C 依赖 A 的 JWT issuer 和 B 的 matter endpoints）。

---

## 13. 决策记录

- [x] **服务改名**：`octo-runtime` → **`octo-fleet`**（端口仍 :8092；env `FLEET_*`）
- [x] **跨服务鉴权**：分阶段
  - PoC 期：matter / fleet 调 server `POST /v1/auth/verify`（复用现成 endpoint）
  - GA：切共享 JWK 本地验签（fleet 颁发 + matter 持公钥），消除 N+1
  - 切换时机：第一次 matter 或 fleet 上 production / multi-region 时
- [x] **`bot_task` 表归 matter**：写 timeline 同事务入队 · daemon 直接 pull · matter 不知道 fleet 存在
- [x] **lease 续约接口**：PoC 不做（10min lease 覆盖绝大多数 agent run）；GA 加 `POST /matters/internal/tasks/:id/heartbeat`（agent 跑超过 8min 时主动续约）
- [x] **多 daemon 并发 pull 同 bot**：靠 §4 的 atomic UPDATE...WHERE status='queued' 保护；contract test（100 个 daemon × 1 个 bot × 50 个 task → 每个 task 恰好被 claim 一次）列入 GA blocker，PoC 不强制

### 后续讨论项（暂不阻塞 cutover）

- agent 跑 timeout（10min hard）期间 daemon crash 怎么 recover：现行靠 lease 过期 sweeper（§8），但失去了"已部分完成"的进度信息 → GA 阶段可考虑加 `POST /matters/internal/tasks/:id/checkpoint`（写流式进度到 activity.detail）
- 多个 bot 在同一 daemon 上并行 task 的资源限额（openclaw gateway scope upgrade 撞车那个 PoC 实测问题）
- fleet 颁发 JWT 的 key rotation 协调机制（PoC 期无所谓）
