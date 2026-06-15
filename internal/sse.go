package internal

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 决策三 SSE 反向派发 daemon 端: long-lived 收 fleet push, 替代 heartbeat
// pending_* 拉模式 (延迟 5-7s → <500ms).
//
// 每个 runtime 一条 SSE goroutine (plan v6 §Q2 决策 A) — daemon 跑 4
// runtime 就开 4 条独立连接, 单 runtime 问题不影响其它. fleet 端 SSE
// channel 也是 per-runtime, key = runtime_id.
//
// 事件类型 (跟 fleet sse.go eventType* 常量对齐):
//   - upgrade         → call d.handleUpgrade
//   - bot_provision   → fetch GET /v1/daemon/bot-provisions/:id → handleBotProvision
//   - managed_bots_changed → apply delta (idempotent, 不走 dedup)
//
// 关键设计:
//
//   A5 dedup 文件持久化 (~/.octo-daemon/events-<daemon_id>.state):
//     - 防 SSE+heartbeat 双跑期 同事件双次处理
//     - 防 daemon 重启重做 (file atomic write tmp + rename, crash-safe)
//     - dedup key = (event_type, source_pk) 不用 event_log id, 因为
//       同 source 可能来 multiple runtimes 各自的 event_log row
//     - lazy prune on read: 读 file 时清 ts < now-24h, 不开后台 ticker
//
//   A3 bot_provision secret fetch:
//     - SSE event 只含 command_id, secret 不进 stream
//     - 收到后 GET /v1/daemon/bot-provisions/:id 拿完整 payload
//     - 410 Gone (bot 已 active/archived) → log skip, 不重试
//
//   G7 reconnect jitter (caster review fleet round 1):
//     - 60s server TTL close 后所有 daemon 同时重连 → fleet 端 verify
//       请求 spike. base sleep + 10% jitter 抹平到 ~6s window.
//
//   G11 handleBotProvision idempotency:
//     - dedup 是 first line defense (key by command_id 跳重复 fetch)
//     - 万一 dedup file 丢/未写 → fleet bot.status=active 返 410, 真正
//       的 double-mint 被 fleet 端 atomic 状态机挡住

const (
	sseReconnectBaseDelay = 1 * time.Second
	sseReconnectMaxDelay  = 60 * time.Second
	// reconnect 后加 ±10% jitter, 防 100 daemon 60s TTL 同时 expire 后
	// 同时打 verify-api-key (G7).
	sseReconnectJitterPct = 0.10

	sseDedupTTL = 24 * time.Hour

	// SSE 事件类型字符串 — 必须跟 fleet modules/runtime/sse.go 的
	// eventType* 常量字面值一致.
	sseEventPing               = "ping"
	sseEventUpgrade            = "upgrade"
	sseEventBotProvision       = "bot_provision"
	sseEventManagedBotsChanged = "managed_bots_changed"

	// SSE event indicating server proactively closed conn for TTL
	// re-verification (60s align verifyCache). daemon 收到立即重连.
	sseEventClose = "close"
)

// sseEvent 是 SSE 帧解析后的内存表示.
type sseEvent struct {
	ID   string // event id (event_log row id, 数字字符串)
	Type string // event: 字段
	Data string // data: 字段 (JSON payload)
}

// dedupState 是落盘的 dedup 数据结构. JSON 文件 schema (跟 plan v6 §3.5
// + R4 round 4 inflight/done 拆分):
//
//	{
//	  "upgrade": [...],
//	  "bot_provision": [...],
//	  "last_event_id_per_runtime": {"42": 123, "43": 456}
//	}
//
// phase 缺省 = "done" (老 schema 向下兼容: 旧 entry 没 phase 字段, load
// 时按 done 算; 旧 entry 都是 mark() 后写入的, 语义上就是 done).
//
// 内存 map 化以便 O(1) lookup, 写盘时再 marshal 成 plan 的 list 形式.
//
// R4 round 4 (codex review final): claim 返 (claimed, alreadyDone, err).
// alreadyDone=true 时 caller 可 advance (其他 path 已成功). inflight 时
// (claimed=false, alreadyDone=false) caller **不能 advance**, 必须断流
// 让 owner 完成. 这一层是 H3+B1+B2 之后剩下的 race 闭环.
//
// last_event_id_per_runtime (H2 caster review daemon round 1): 重连时
// Last-Event-ID = lastIDs[runtimeID], fleet replay 只推 id > 上次最大.
type dedupState struct {
	mu sync.Mutex
	// entries[eventType][sourceID] = ts(ms)
	entries map[string]map[string]int64
	// phases[eventType][sourceID] = phaseInflight | phaseDone
	// 缺失视为 phaseDone (老 schema 向下兼容).
	phases map[string]map[string]entryPhase
	// lastIDs[runtimeID] = max event_log id seen (用作 Last-Event-ID
	// 重连 header).
	lastIDs map[int64]int64
	// markDoneCount: 自上次 hot-path prune 起累计 markDone 次数, 触发
	// size-triggered prune (D-2 lml2468 review). 不开后台 ticker, 不依赖
	// seen() (test-only) 的 lazy prune.
	markDoneCount int
	path          string
}

// dedupHotPrunePeriod: markDone 每 N 次触发一次 hot-path prune. 太小
// 浪费 CPU, 太大 file 长得太快. 100 是 balance: daemon 跑稳定状态
// 平均 ~1 markDone/s/runtime × 4 runtime = 4/s → ~25s 一次 prune.
const dedupHotPrunePeriod = 100

// entryPhase 标识 dedup entry 是 "正在处理" 还是 "已完成". inflight 时
// 其它 caller 不能 advance cursor (R4 codex BLOCKER), 必须断流让 owner
// 完成.
type entryPhase string

const (
	phaseInflight entryPhase = "inflight"
	phaseDone     entryPhase = "done"
)

// dedupEntry JSON 序列化形式. Phase 字段 omitempty: 缺失时 load 默认 done.
type dedupEntry struct {
	ID    string     `json:"id"`
	TS    int64      `json:"ts"`              // unix ms
	Phase entryPhase `json:"phase,omitempty"` // R4: missing/empty = phaseDone
}

// dedupFile 是落盘 JSON schema (用 string-keyed map 因为 JSON object
// key 必须是 string; load/persist 时转 int64 ↔ string).
type dedupFile struct {
	Upgrade          []dedupEntry     `json:"upgrade,omitempty"`
	BotProvision     []dedupEntry     `json:"bot_provision,omitempty"`
	LastEventIDPerRT map[string]int64 `json:"last_event_id_per_runtime,omitempty"`
}

func newDedupState(daemonID string) *dedupState {
	path := filepath.Join(DataDir(), "events-"+daemonID+".state")
	return &dedupState{
		entries: make(map[string]map[string]int64),
		phases:  make(map[string]map[string]entryPhase),
		lastIDs: make(map[int64]int64),
		path:    path,
	}
}

// load 读 file 进内存. file 不存在返 nil (空 state). lazy prune on read:
// drop entries with ts < now-24h.
func (d *dedupState) load() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	data, err := os.ReadFile(d.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read dedup state: %w", err)
	}
	var df dedupFile
	if err := json.Unmarshal(data, &df); err != nil {
		// fallback: 老 schema (无 last_event_id_per_runtime 字段) 解为
		// 旧 map[string][]dedupEntry. 如果两个 schema 都失败 = 真损坏.
		var raw map[string][]dedupEntry
		if err2 := json.Unmarshal(data, &raw); err2 == nil {
			df.Upgrade = raw[sseEventUpgrade]
			df.BotProvision = raw[sseEventBotProvision]
		} else {
			// 损坏的 state file (crash-mid-write 等) 不阻塞启动, log + 当空对待.
			log.Printf("[WARN] dedup state file corrupted (%v) — starting empty", err)
			d.entries = make(map[string]map[string]int64)
			d.phases = make(map[string]map[string]entryPhase)
			d.lastIDs = make(map[int64]int64)
			return nil
		}
	}

	cutoff := time.Now().Add(-sseDedupTTL).UnixMilli()
	loadBucket := func(et string, list []dedupEntry) {
		if len(list) == 0 {
			return
		}
		bucket := make(map[string]int64, len(list))
		phaseBucket := make(map[string]entryPhase, len(list))
		for _, e := range list {
			if e.TS < cutoff {
				continue // TTL prune
			}
			// R4 round 5 (codex review final): inflight 是进程内瞬态状态,
			// 不该跨 daemon restart. 没有 owner 会 markDone/unclaim 它,
			// 否则下次 claim 永远 (false, false, nil) 断流死循环. drop
			// inflight entries on load → 重启后 fresh claim 重跑 handler
			// (handler 应幂等, fleet state machine 兜底).
			phase := e.Phase
			if phase == "" {
				phase = phaseDone // back-compat: 老 schema 缺 phase → done
			}
			if phase == phaseInflight {
				continue // drop, allow fresh claim after restart
			}
			bucket[e.ID] = e.TS
			phaseBucket[e.ID] = phase
		}
		if len(bucket) > 0 {
			d.entries[et] = bucket
			d.phases[et] = phaseBucket
		}
	}
	loadBucket(sseEventUpgrade, df.Upgrade)
	loadBucket(sseEventBotProvision, df.BotProvision)

	for rtStr, id := range df.LastEventIDPerRT {
		rtID, err := strconv.ParseInt(rtStr, 10, 64)
		if err != nil || rtID <= 0 {
			continue
		}
		d.lastIDs[rtID] = id
	}
	return nil
}

// seen 检查 (eventType, sourceID) 是否已处理过 (任意 phase). 不持锁返
// false 表示 caller 应当处理这条 event, 处理完调 mark + persist 落盘.
//
// 同时做 lazy on-read prune: 该 eventType 的 set 内 ts < now-24h 的
// entry 顺便删掉 — 不开后台 ticker (plan v6 §3.5 E1).
//
// 注: 此方法只用于测试. 生产路径用 claim/markDone (R4 round 4).
func (d *dedupState) seen(eventType, sourceID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	bucket, ok := d.entries[eventType]
	if !ok {
		return false
	}
	// lazy prune
	cutoff := time.Now().Add(-sseDedupTTL).UnixMilli()
	for id, ts := range bucket {
		if ts < cutoff {
			delete(bucket, id)
			if d.phases[eventType] != nil {
				delete(d.phases[eventType], id)
			}
		}
	}
	if len(bucket) == 0 {
		delete(d.entries, eventType)
		delete(d.phases, eventType)
		return false
	}
	_, found := bucket[sourceID]
	return found
}

// mark 添加 (eventType, sourceID) 到 set as phaseDone 并 atomic 落盘
// (write tmp + rename, crash-safe). 测试用; 生产路径用 claim+markDone.
//
// persist 必须在 mu 内 (caster review F2 fix): 不然两 goroutine 并发 mark
// 各自出锁后 persist 会有 race — 慢的 goroutine snap 后到 rename 覆盖快的,
// 丢失 entry. 4 runtime SSE goroutine 并发 mark 时真撞.
func (d *dedupState) mark(eventType, sourceID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	bucket, ok := d.entries[eventType]
	if !ok {
		bucket = make(map[string]int64)
		d.entries[eventType] = bucket
	}
	bucket[sourceID] = time.Now().UnixMilli()
	if d.phases[eventType] == nil {
		d.phases[eventType] = make(map[string]entryPhase)
	}
	d.phases[eventType][sourceID] = phaseDone
	snap := d.snapshotLocked()
	return d.persist(snap)
}

// claim atomic check + mark inflight + persist (B1 + R4 round 4).
// 返回三元组:
//   - (true, false, nil)  caller 拿到独占处理权, 处理完必须调 markDone
//     (成功) 或 unclaim (失败). entry 已 persist as inflight.
//   - (false, true, nil)  alreadyDone — 其它 path 之前成功处理过 (或老
//     schema mark). caller 可 advance cursor.
//   - (false, false, nil) inflight — 其它 path 正在处理 (handler 没回).
//     caller **不能 advance cursor**, 应返 err 让 readLoop 断流重连;
//     重连后 owner 已完成 (alreadyDone) 或失败 unclaim (caller 重新 claim).
//     R4 fix 防 claim-skip → unclaim 后 cursor 已越过的永丢 race.
//   - (false, false, err) persist 失败 — caller 当 race-lost / 断流处理.
func (d *dedupState) claim(eventType, sourceID string) (claimed, alreadyDone bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	bucket, ok := d.entries[eventType]
	if !ok {
		bucket = make(map[string]int64)
		d.entries[eventType] = bucket
	}
	if d.phases[eventType] == nil {
		d.phases[eventType] = make(map[string]entryPhase)
	}
	if _, exists := bucket[sourceID]; exists {
		// already in flight or done
		if d.phases[eventType][sourceID] == phaseDone {
			return false, true, nil
		}
		return false, false, nil // inflight elsewhere
	}
	bucket[sourceID] = time.Now().UnixMilli()
	d.phases[eventType][sourceID] = phaseInflight
	snap := d.snapshotLocked()
	if err := d.persist(snap); err != nil {
		delete(bucket, sourceID)
		delete(d.phases[eventType], sourceID)
		return false, false, err
	}
	return true, false, nil
}

// markDone 把 inflight entry 转为 done — handler 成功完成时调.
// 之后 dup claim 返 (false, true, nil), caller 可安全 advance.
//
// D-2 (lml2468 review): 每 dedupHotPrunePeriod 次 markDone 触发一次 hot-path
// prune. 之前生产 hot path 没 prune (seen() 的 lazy prune 是 test-only,
// 零生产调用), 24h 不重启 file 累积所有 event volume × 4 runtime × 3 type,
// 每 markDone 全量 marshal+rename 成本 O(N). size-triggered + lazy 不开
// 后台 ticker.
func (d *dedupState) markDone(eventType, sourceID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.phases[eventType] == nil {
		d.phases[eventType] = make(map[string]entryPhase)
	}
	d.phases[eventType][sourceID] = phaseDone
	// 确保 entries 里也有 (理论上 claim 已加, 但 mark 路径可能直接进).
	if d.entries[eventType] == nil {
		d.entries[eventType] = make(map[string]int64)
	}
	if _, ok := d.entries[eventType][sourceID]; !ok {
		d.entries[eventType][sourceID] = time.Now().UnixMilli()
	}
	// D-2 hot-path prune trigger
	d.markDoneCount++
	if d.markDoneCount >= dedupHotPrunePeriod {
		d.pruneLocked()
		d.markDoneCount = 0
	}
	snap := d.snapshotLocked()
	return d.persist(snap)
}

// pruneLocked 删除 ts < now-24h 的 done entries. caller 必须持 mu.
// inflight 不动 (有 owner 在处理, owner 完成后会 markDone 转 done 再
// 等下一轮 prune; load() 时 inflight 已 drop 所以 prune 不需要担心
// orphan inflight).
//
// 为啥不 prune inflight: inflight 是进程内瞬态 (R5 fix, restart 时 load
// drop), TTL 不适用; 而且强行删 inflight 等于把 owner 处理权偷走, owner
// markDone 时会找不到 entry (markDone 重建 entry, OK 但混淆语义).
func (d *dedupState) pruneLocked() {
	cutoff := time.Now().Add(-sseDedupTTL).UnixMilli()
	for et, bucket := range d.entries {
		for id, ts := range bucket {
			if ts >= cutoff {
				continue
			}
			// 只 prune done 的, inflight 留给 owner
			if d.phases[et] != nil && d.phases[et][id] == phaseInflight {
				continue
			}
			delete(bucket, id)
			if d.phases[et] != nil {
				delete(d.phases[et], id)
			}
		}
		if len(bucket) == 0 {
			delete(d.entries, et)
			delete(d.phases, et)
		}
	}
}

// unclaim 撤销 claim — caller 在 handler 失败 (panic recover / network
// error) 时调, 让下次 replay 能重试 (匹配 H3 设计: panic catastrophic →
// replay 兜底). handler 内部 ack-fail (download URL 坏) 不调 unclaim —
// leave claimed (markDone 标 done), replay 重做坏 URL 无意义.
func (d *dedupState) unclaim(eventType, sourceID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bucket, ok := d.entries[eventType]; ok {
		delete(bucket, sourceID)
		if len(bucket) == 0 {
			delete(d.entries, eventType)
		}
	}
	if d.phases[eventType] != nil {
		delete(d.phases[eventType], sourceID)
		if len(d.phases[eventType]) == 0 {
			delete(d.phases, eventType)
		}
	}
	snap := d.snapshotLocked()
	return d.persist(snap)
}

// recordEventID 更新 runtimeID 上看到的最大 event_log id (用作 Last-Event-ID
// 重连 header). 仅在严格 > 当前值时更新 + 落盘, 不变则跳过 IO.
//
// caller 在 dispatch 一帧后调 (无论 dedup 是否跳过), 这样即便 daemon
// 收到 dup event 也推进 Last-Event-ID, 下次重连 server replay 量更小.
//
// persist 必须在 mu 内 (F2 fix, 同 mark 注释).
func (d *dedupState) recordEventID(runtimeID, eventID int64) error {
	if runtimeID <= 0 || eventID <= 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	cur := d.lastIDs[runtimeID]
	if eventID <= cur {
		return nil
	}
	d.lastIDs[runtimeID] = eventID
	snap := d.snapshotLocked()
	return d.persist(snap)
}

// snapshotLocked 在 caller 已持 mu 时返回当前 state 的 file-shape 拷贝.
func (d *dedupState) snapshotLocked() dedupFile {
	mkList := func(et string, bucket map[string]int64) []dedupEntry {
		list := make([]dedupEntry, 0, len(bucket))
		for id, ts := range bucket {
			phase := phaseDone
			if d.phases[et] != nil {
				if p, ok := d.phases[et][id]; ok {
					phase = p
				}
			}
			list = append(list, dedupEntry{ID: id, TS: ts, Phase: phase})
		}
		return list
	}
	df := dedupFile{}
	if b := d.entries[sseEventUpgrade]; len(b) > 0 {
		df.Upgrade = mkList(sseEventUpgrade, b)
	}
	if b := d.entries[sseEventBotProvision]; len(b) > 0 {
		df.BotProvision = mkList(sseEventBotProvision, b)
	}
	if len(d.lastIDs) > 0 {
		df.LastEventIDPerRT = make(map[string]int64, len(d.lastIDs))
		for rt, id := range d.lastIDs {
			df.LastEventIDPerRT[strconv.FormatInt(rt, 10)] = id
		}
	}
	return df
}

// persist 写 tmp + rename. 跟 identity.go EnsureDaemonID 同 pattern.
func (d *dedupState) persist(snap dedupFile) error {
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal dedup state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(d.path), 0700); err != nil {
		return fmt.Errorf("mkdir dedup state: %w", err)
	}
	tmp := d.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("write tmp dedup state: %w", err)
	}
	if err := os.Rename(tmp, d.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename dedup state: %w", err)
	}
	return nil
}

// lastEventID 返回该 runtime 上最大的 event_log id (上次断连前 daemon
// 处理到的最后一个). 用作 SSE 重连 Last-Event-ID header — server replay
// 只推 id > 这个值的 event, 不再 24h 全推 (H2 caster review fix).
//
// 0 表示从未收过 / 24h 内 first connect, server 走 full replay (TTL 内
// 全量, dedup 跳重复).
func (d *dedupState) lastEventID(runtimeID int64) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastIDs[runtimeID]
}

// ===== SSE client =====

// SSEClient 单 daemon 一份, 内部维护 dedup state 和 HTTP client. 每个
// runtime 一条 SSE goroutine 共享同一份 dedup.
type SSEClient struct {
	fleetURL string
	apiKey   string
	dedup    *dedupState

	// 复用 daemon 现有 *Client 拿 secret + ack endpoint 引用; 不重新
	// 建一个 client 避免 config drift.
	apiClient *Client
}

// NewSSEClient 在 NewDaemon 时构造. 调用方传 daemon_id (已 EnsureDaemonID).
func NewSSEClient(fleetURL, apiKey, daemonID string, apiClient *Client) (*SSEClient, error) {
	d := newDedupState(daemonID)
	if err := d.load(); err != nil {
		return nil, err
	}
	return &SSEClient{
		fleetURL:  strings.TrimRight(fleetURL, "/"),
		apiKey:    apiKey,
		dedup:     d,
		apiClient: apiClient,
	}, nil
}

// botProvisionDispatcher 是 daemon.handleBotProvision 的接口抽象, 方便
// 测试 mock. real daemon 传 d.HandleBotProvision 进来.
//
// err 返回 (H3 caster review fix): 仅当 handler crash/panic 时返非 nil,
// dispatcher 据此决定是否 mark dedup. handler 内部 ack-fail (download
// bad / install crash 但 ack 到 fleet) 仍返 nil — 视为"dispatch 完成,
// 不要 replay 怕死循环坏 download URL".
type botProvisionDispatcher interface {
	HandleBotProvision(ctx context.Context, cmd *PendingAgentCommand) error
}

// upgradeDispatcher 是 daemon.handleUpgrade 的接口抽象.
type upgradeDispatcher interface {
	HandleUpgrade(ctx context.Context, up *PendingUpgrade) error
}

// managedBotsDispatcher 接受 add/remove delta, 由 daemon 端 apply 到
// 本地 managedBots 缓存 (跟 heartbeat managed_bots snapshot 合并).
type managedBotsDispatcher interface {
	ApplyManagedBotsDelta(added, removed []string)
}

// RunForRuntime 单 runtime 一条 SSE goroutine 主循环.
// ctx.Done() 后退出.
func (c *SSEClient) RunForRuntime(
	ctx context.Context,
	runtimeID int64,
	bp botProvisionDispatcher,
	up upgradeDispatcher,
	mb managedBotsDispatcher,
) {
	delay := sseReconnectBaseDelay
	for {
		// connectOnce 收到 first frame 后 reset delay (via *delay) — H7
		// caster review fix: 之前 resetBackoff no-op, 连接成功 30s 后
		// 网络瞬断仍按上次 backoff (可能已涨到 60s) 等, 体感很糟.
		err := c.connectOnce(ctx, runtimeID, &delay, bp, up, mb)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("[WARN] SSE runtime=%d connection ended: %v", runtimeID, err)
		}
		// G7 jitter: ±10% 防 N daemon 同时 reconnect 撞 verify-api-key spike.
		jitter := time.Duration(float64(delay) * sseReconnectJitterPct * (rand.Float64()*2 - 1))
		sleep := delay + jitter
		if sleep < 0 {
			sleep = sseReconnectBaseDelay
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(sleep):
		}
		// exp backoff cap at sseReconnectMaxDelay
		delay *= 2
		if delay > sseReconnectMaxDelay {
			delay = sseReconnectMaxDelay
		}
	}
}

// connectOnce 建一次 SSE 连接, 一路读到 close/EOF/err 才 return.
//
// delay 是 RunForRuntime 共享的 backoff 计数器, connectOnce 收到 first
// 完整 frame 后置回 base (H7 fix: 长连接稳定一段时间后再断, backoff
// 应回到 1s 而非延续上次涨到的值).
func (c *SSEClient) connectOnce(
	ctx context.Context,
	runtimeID int64,
	delay *time.Duration,
	bp botProvisionDispatcher,
	up upgradeDispatcher,
	mb managedBotsDispatcher,
) error {
	url := fmt.Sprintf("%s/v1/daemon/events?runtime_id=%d", c.fleetURL, runtimeID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	// H2 fix: Last-Event-ID per-runtime, server replay 只推 id > 这个值.
	// 0 = 24h 内首次连 / fresh state, server 走 full replay (dedup 跳重复).
	if lastID := c.dedup.lastEventID(runtimeID); lastID > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(lastID, 10))
	}

	// SSE 是长连接, 默认 http.Client 30s timeout 会切. 用 90s timeout
	// (略大于 server 60s TTL close 周期), 保证 silent TCP drop (server
	// 接受 connection 但卡死, 不发 FIN/RST, 60s close frame 也不来) 时
	// 在合理时间自愈成 reconnect, 不靠 kernel TCP keepalive 兜底.
	// P2-1 (yujiawei review).
	//
	// CROSS-PR DEPENDENCY (cc N1): 这个 90s 跟 fleet 端 sseTTL 常量 (当前
	// 60s, 见 octo-fleet/modules/runtime/sse.go) 隐式耦合. 改 fleet sseTTL
	// 必须同步本处 daemon Timeout (建议 daemon = fleet TTL + 30s buffer).
	// Phase B 若 fleet 调成 600s 减少 reconnect spike, 本处也得跟着调,
	// 否则健康 connection 每 90s 强杀, reconnect rate 6x 暴涨.
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	return c.readLoop(ctx, runtimeID, resp.Body, delay, bp, up, mb)
}

// readLoop 解析 SSE wire format. 帧格式:
//
//	event: <type>\n
//	id: <id>\n
//	data: <json>\n
//	\n
//
// 空行 = 一帧结束. ":" 开头 = 注释 (keepalive), 跳过.
//
// runtimeID 用于 dispatch 时更新 lastIDs[runtimeID] (Last-Event-ID 重连
// header source). delay 在第一帧读到时 reset (H7 fix).
func (c *SSEClient) readLoop(
	ctx context.Context,
	runtimeID int64,
	body io.Reader,
	delay *time.Duration,
	bp botProvisionDispatcher,
	up upgradeDispatcher,
	mb managedBotsDispatcher,
) error {
	scanner := bufio.NewScanner(body)
	// SSE data 可能比较长 (upgrade event 含 download_url + metadata).
	// 默认 64KB 应该够; bump 到 1MB 保险.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	frameSeen := false
	var cur sseEvent
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Text()
		switch {
		case line == "":
			// 帧结束
			if cur.Type != "" {
				// 第一帧读到 = 连接稳了, reset backoff (H7).
				if !frameSeen && delay != nil {
					*delay = sseReconnectBaseDelay
					frameSeen = true
				}
				if cur.Type == sseEventClose {
					// server 主动 close (60s TTL re-verify) — 退出
					// readLoop, 让 RunForRuntime 走 reconnect.
					return errors.New("server sent close event (TTL re-verify)")
				}
				// B2 round 3 (codex): dispatch 返 err 必须断流重连. 不能
				// 继续读后续 frame, 否则成功 event 用 max 语义推进 cursor
				// 越过失败 id, 失败 event 永丢 (尤其 bot_provision: fetch
				// 已 transition row, heartbeat 看不到).
				if derr := c.dispatch(ctx, runtimeID, cur, bp, up, mb); derr != nil {
					return fmt.Errorf("sse dispatch failed, reconnect: %w", derr)
				}
			}
			cur = sseEvent{}
		case strings.HasPrefix(line, ":"):
			// SSE comment (keepalive). 第一行 keepalive 也算"连接稳"
			// (server 在推, 走在 verifyMW 之后了) — reset backoff.
			if !frameSeen && delay != nil {
				*delay = sseReconnectBaseDelay
				frameSeen = true
			}
		case strings.HasPrefix(line, "event: "):
			cur.Type = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			cur.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			// 一帧多 data 字段时按 \n join (SSE 标准). 当前 fleet 端单
			// data 行, 但保险起见兼容.
			if cur.Data == "" {
				cur.Data = strings.TrimPrefix(line, "data: ")
			} else {
				cur.Data += "\n" + strings.TrimPrefix(line, "data: ")
			}
		default:
			// 未知 line — 忽略 (SSE spec 要求 unknown field 跳过).
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanner: %w", err)
	}
	return io.EOF
}

// dispatch 处理一帧 event: dedup claim → fetch (bot_provision) → 调对应
// handler → 失败 unclaim 让 replay 重试 (H3 + B2 fix from codex review).
//
// 返回 err 表示"这条 event 处理失败, 不能继续读后续 frame" — caller (readLoop)
// 收到 err 应当 return 让 RunForRuntime 重连. 不返 err 才意味着此 event
// 处理完毕 (含成功 / dedup-skip / 410 终态), readLoop 继续读下一帧.
//
// 为啥失败必须断流重连 (B2 round 3 codex review BLOCKER):
//   - handler 失败 dispatch 不推进 lastEventID, 但 readLoop 继续读后续帧
//   - 后续成功 event 用 max 语义推进 cursor 越过失败 id
//   - bot_provision 尤其危险: fetchBotProvision 已把 fleet row 转 dispatched
//     (atomic 在 endpoint 内), heartbeat 看不到 (WHERE bot_minted), 失败 event
//     永丢 — 只能 SSE replay 救
//   - 失败即断流: cursor 不动, 重连 Last-Event-ID 还是失败前值, fleet replay
//     从该 event 起重发, dedup file 跳已 ack 的, 失败的 event 重试
//
// claim atomic check + persist: 跟 heartbeat path 共享同一 dedup state,
// SSE/heartbeat 双跑窗口 (e.g. SSE reconnect 中 heartbeat 抢先) 任一路径
// 只有一个 claim 成功, 另一个 silently 跳 (B1 fix).
//
// recordEventID 推进 Last-Event-ID 时机 (B2):
//   - claim 成功 + handler 成功 → mark + 推进
//   - claim 被抢 (race-lost, dup) → 推进 (避免反复 replay 同 dup)
//   - 410 终态 (bot_provision 已 active/archived) → claim + 推进
//   - **handler 失败 → unclaim + 不推进 + 返回 err 让 readLoop 重连**
func (c *SSEClient) dispatch(
	ctx context.Context,
	runtimeID int64,
	ev sseEvent,
	bp botProvisionDispatcher,
	up upgradeDispatcher,
	mb managedBotsDispatcher,
) error {
	advance := func() {
		if id, err := strconv.ParseInt(ev.ID, 10, 64); err == nil && id > 0 {
			if perr := c.dedup.recordEventID(runtimeID, id); perr != nil {
				log.Printf("[WARN] SSE recordEventID (rt=%d, id=%d): %v", runtimeID, id, perr)
			}
		}
	}

	switch ev.Type {
	case sseEventUpgrade:
		var u PendingUpgrade
		if err := json.Unmarshal([]byte(ev.Data), &u); err != nil {
			log.Printf("[WARN] SSE upgrade (runtime=%d): bad payload: %v", runtimeID, err)
			advance()
			return nil
		}
		if u.TaskID == "" {
			log.Printf("[WARN] SSE upgrade (runtime=%d): empty task_id", runtimeID)
			advance()
			return nil
		}
		claimed, alreadyDone, cerr := c.dedup.claim(ev.Type, u.TaskID)
		if cerr != nil {
			log.Printf("[WARN] SSE upgrade (runtime=%d task_id=%s) claim persist failed: %v", runtimeID, u.TaskID, cerr)
			return cerr
		}
		if alreadyDone {
			advance()
			return nil
		}
		if !claimed {
			return fmt.Errorf("upgrade %s in-flight by another path, reconnect to retry", u.TaskID)
		}
		if err := up.HandleUpgrade(ctx, &u); err != nil {
			log.Printf("[WARN] SSE upgrade (runtime=%d task_id=%s) handler error: %v — unclaim + reconnect", runtimeID, u.TaskID, err)
			if uerr := c.dedup.unclaim(ev.Type, u.TaskID); uerr != nil {
				log.Printf("[WARN] SSE upgrade (runtime=%d task_id=%s) unclaim: %v", runtimeID, u.TaskID, uerr)
			}
			return err
		}
		if merr := c.dedup.markDone(ev.Type, u.TaskID); merr != nil {
			log.Printf("[WARN] SSE upgrade (runtime=%d task_id=%s) markDone: %v", runtimeID, u.TaskID, merr)
		}
		advance()
		return nil

	case sseEventBotProvision:
		var p struct {
			CommandID string `json:"command_id"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &p); err != nil {
			log.Printf("[WARN] SSE bot_provision (runtime=%d): bad payload: %v", runtimeID, err)
			advance()
			return nil
		}
		if p.CommandID == "" {
			log.Printf("[WARN] SSE bot_provision (runtime=%d): empty command_id", runtimeID)
			advance()
			return nil
		}
		claimed, alreadyDone, cerr := c.dedup.claim(ev.Type, p.CommandID)
		if cerr != nil {
			log.Printf("[WARN] SSE bot_provision (runtime=%d id=%s) claim persist failed: %v", runtimeID, p.CommandID, cerr)
			return cerr
		}
		if alreadyDone {
			advance()
			return nil
		}
		if !claimed {
			return fmt.Errorf("bot_provision %s in-flight by another path, reconnect to retry", p.CommandID)
		}
		cmd, err := c.fetchBotProvision(ctx, p.CommandID)
		if err != nil {
			log.Printf("[WARN] SSE bot_provision (runtime=%d id=%s) fetch: %v — unclaim + reconnect", runtimeID, p.CommandID, err)
			if uerr := c.dedup.unclaim(ev.Type, p.CommandID); uerr != nil {
				log.Printf("[WARN] SSE bot_provision (runtime=%d id=%s) unclaim: %v", runtimeID, p.CommandID, uerr)
			}
			return err
		}
		if cmd == nil {
			// 410 — bot 已 active/archived. 标 done 跳过.
			log.Printf("[INFO] SSE bot_provision (runtime=%d) command %s no longer provisionable (410)", runtimeID, p.CommandID)
			if merr := c.dedup.markDone(ev.Type, p.CommandID); merr != nil {
				log.Printf("[WARN] SSE bot_provision (runtime=%d id=%s) markDone (410): %v", runtimeID, p.CommandID, merr)
			}
			advance()
			return nil
		}
		if herr := bp.HandleBotProvision(ctx, cmd); herr != nil {
			log.Printf("[WARN] SSE bot_provision (runtime=%d id=%s) handler error: %v — unclaim + reconnect", runtimeID, p.CommandID, herr)
			if uerr := c.dedup.unclaim(ev.Type, p.CommandID); uerr != nil {
				log.Printf("[WARN] SSE bot_provision (runtime=%d id=%s) unclaim: %v", runtimeID, p.CommandID, uerr)
			}
			return herr
		}
		if merr := c.dedup.markDone(ev.Type, p.CommandID); merr != nil {
			log.Printf("[WARN] SSE bot_provision (runtime=%d id=%s) markDone: %v", runtimeID, p.CommandID, merr)
		}
		advance()
		return nil

	case sseEventManagedBotsChanged:
		var p struct {
			Added   []string `json:"added"`
			Removed []string `json:"removed"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &p); err != nil {
			log.Printf("[WARN] SSE managed_bots_changed (runtime=%d): bad payload: %v", runtimeID, err)
			advance()
			return nil
		}
		mb.ApplyManagedBotsDelta(p.Added, p.Removed)
		advance()
		return nil

	default:
		log.Printf("[WARN] SSE (runtime=%d) unknown event type: %q", runtimeID, ev.Type)
		advance() // 未知 event 也 advance, 防 future 新 type 让旧 daemon 卡住
		return nil
	}
}

// fetchBotProvision GET /v1/daemon/bot-provisions/:id. 返回 (nil, nil) 表
// 410 Gone (bot 不再 provisionable).
//
// M5 fix (caster review final from codex): 用 apiClient.httpClient (30s
// timeout), 不用 http.DefaultClient — fetch 在 readLoop goroutine 内, fleet
// 卡死会无限阻塞后续 SSE frame 处理.
func (c *SSEClient) fetchBotProvision(ctx context.Context, commandID string) (*PendingAgentCommand, error) {
	url := fmt.Sprintf("%s/v1/daemon/bot-provisions/%s", c.fleetURL, commandID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.apiClient.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode == http.StatusGone {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	// fleet wkhttp c.Response(data) 直接 JSON 数据, 无 outer envelope —
	// 跟 daemon postJSON unmarshal 路径一致.
	//
	// 注意 (MI7 doc clarification): fleet 返的 payload 含 bot_uid + claim_token +
	// workspace_id 但**不含 bot_token** — fleet bot 表不存 token. daemon
	// 收到后若 cmd.BotToken == "" 会另起 GET /v1/bot/:bot_uid/token 问
	// octo-server 拿 (exec_openclaw.go handleBotProvision line 42-50).
	// 所以 A3 secret 不进 SSE stream 是因为 fleet 本来就没 secret, 这里
	// fetch 拿的是 "bot identifying info + claim_token" 不是 secret 本身.
	var cmd PendingAgentCommand
	if err := json.Unmarshal(body, &cmd); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if cmd.ID == 0 {
		return nil, fmt.Errorf("empty payload")
	}
	return &cmd, nil
}
