package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 决策三 SSE daemon-cli 测试: dedup 文件 / SSE wire parsing / fetch /
// managed_bots delta / 完整 connectOnce e2e (mock server).
//
// dedup file path uses DataDir() (home/.octo-daemon). 测试 override HOME
// 用 t.Setenv 避免污染真实用户 ~/.octo-daemon.

func tempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return dir
}

// ===== dedupState =====

func TestDedupState_LoadEmpty(t *testing.T) {
	tempHome(t)
	d := newDedupState("dummy-daemon-id")
	if err := d.load(); err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if d.seen(sseEventPing, "ping_1") {
		t.Error("empty state should not contain anything")
	}
}

func TestDedupState_MarkSeenRoundtrip(t *testing.T) {
	tempHome(t)
	d := newDedupState("daemon-A")
	_ = d.load()

	if err := d.mark(sseEventPing, "ping_42"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if !d.seen(sseEventPing, "ping_42") {
		t.Error("mark then seen should return true")
	}
	if d.seen(sseEventPing, "ping_other") {
		t.Error("other id should not be seen")
	}

	// reload from file
	d2 := newDedupState("daemon-A")
	if err := d2.load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !d2.seen(sseEventPing, "ping_42") {
		t.Error("reload should preserve marked id")
	}
}

func TestDedupState_AtomicWrite(t *testing.T) {
	// 验证 mark 用 tmp + rename, file 在中间状态 (tmp) 不影响 load.
	home := tempHome(t)
	d := newDedupState("daemon-X")
	_ = d.load()
	if err := d.mark(sseEventUpgrade, "upg_1"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	// 没 leak .tmp file
	tmpPath := filepath.Join(home, ".octo-daemon", "daemon-X", "events.state.tmp")
	if _, err := os.Stat(tmpPath); err == nil {
		t.Errorf("tmp file %s should not exist after rename", tmpPath)
	}
	// 真 file 存在
	realPath := filepath.Join(home, ".octo-daemon", "daemon-X", "events.state")
	if _, err := os.Stat(realPath); err != nil {
		t.Errorf("real state file should exist: %v", err)
	}
}

func TestDedupState_CorruptFileRecovery(t *testing.T) {
	home := tempHome(t)
	dataDir := filepath.Join(home, ".octo-daemon", "daemon-corrupt")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dataDir, "events.state")
	// 写入不合法 JSON, 模拟 crash-mid-write
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	d := newDedupState("daemon-corrupt")
	if err := d.load(); err != nil {
		t.Fatalf("load 应当 graceful, 不该 return err on corrupt: %v", err)
	}
	if d.seen(sseEventPing, "anything") {
		t.Error("corrupt-state-as-empty: nothing should be seen")
	}
	// 后续 mark 仍能正常工作 (overwrite corrupt file)
	if err := d.mark(sseEventPing, "ping_recovered"); err != nil {
		t.Fatalf("post-recovery mark: %v", err)
	}
	if !d.seen(sseEventPing, "ping_recovered") {
		t.Error("post-recovery mark/seen broken")
	}
}

func TestDedupState_LazyPruneTTL(t *testing.T) {
	tempHome(t)
	d := newDedupState("daemon-prune")
	_ = d.load()
	// 手动 inject 一个过期 entry (24h+1s 前)
	d.mu.Lock()
	d.entries[sseEventPing] = map[string]int64{
		"stale_ping": time.Now().Add(-sseDedupTTL - time.Second).UnixMilli(),
		"fresh_ping": time.Now().UnixMilli(),
	}
	d.mu.Unlock()

	// seen("fresh_ping") 触发 lazy prune
	if !d.seen(sseEventPing, "fresh_ping") {
		t.Error("fresh entry should remain")
	}
	if d.seen(sseEventPing, "stale_ping") {
		t.Error("stale entry should be pruned on read")
	}
}

// H2 caster review fix: per-runtime last_event_id 必须持久化, 重启后
// reload, lastEventID(runtimeID) 返正确值. 防回归到 lastEventID 永
// 返 0 (强制 full TTL replay, 浪费 ~24×).
func TestDedupState_LastEventIDRoundtrip(t *testing.T) {
	tempHome(t)
	d := newDedupState("daemon-lastid")
	_ = d.load()

	if d.lastEventID(42) != 0 {
		t.Error("empty state should return 0")
	}
	if err := d.recordEventID(42, 100); err != nil {
		t.Fatalf("recordEventID 100: %v", err)
	}
	if got := d.lastEventID(42); got != 100 {
		t.Errorf("after record 100, want 100 got %d", got)
	}
	// 严格 > 才更新
	if err := d.recordEventID(42, 50); err != nil {
		t.Fatalf("recordEventID 50: %v", err)
	}
	if got := d.lastEventID(42); got != 100 {
		t.Errorf("record 50 should be no-op (50 < 100), got %d", got)
	}
	// 不同 runtime 独立
	if err := d.recordEventID(43, 7); err != nil {
		t.Fatalf("recordEventID rt=43 id=7: %v", err)
	}
	if got := d.lastEventID(43); got != 7 {
		t.Errorf("rt=43 want 7, got %d", got)
	}
	if got := d.lastEventID(42); got != 100 {
		t.Errorf("rt=42 should still be 100 after rt=43 update, got %d", got)
	}

	// 重 load: file 必须持久化 last_event_id_per_runtime
	d2 := newDedupState("daemon-lastid")
	if err := d2.load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := d2.lastEventID(42); got != 100 {
		t.Errorf("reload rt=42 want 100, got %d", got)
	}
	if got := d2.lastEventID(43); got != 7 {
		t.Errorf("reload rt=43 want 7, got %d", got)
	}
}

// H2: dispatch 后 dedup.recordEventID 必须更新. e2e test 走 dispatch
// 走一遍, 验 lastEventID 推进了.
func TestDispatch_RecordsEventIDForReconnect(t *testing.T) {
	tempHome(t)
	d := newDedupState("daemon-record")
	_ = d.load()
	c := &SSEClient{dedup: d, apiClient: &Client{httpClient: &http.Client{Timeout: 30 * time.Second}}}

	// 走 upgrade event (无网络 IO).
	ev := sseEvent{
		ID:   "500",
		Type: sseEventUpgrade,
		Data: `{"task_id":"upg_500","component":"octo-daemon"}`,
	}
	bp := &mockBP{}
	up := &mockUp{}
	mb := &mockMB{}
	_ = c.dispatch(context.Background(), 99, ev, bp, up, mb)

	if got := d.lastEventID(99); got != 500 {
		t.Errorf("dispatch should recordEventID(99, 500), got %d", got)
	}
}

// H3 caster review fix: handler 返 err 不能 mark dedup. 防回归到
// "handler 失败仍 dedup 跳, replay 永远不重试".
func TestDispatch_HandlerErrorLeavesDedupUnmarked(t *testing.T) {
	tempHome(t)
	d := newDedupState("daemon-h3")
	_ = d.load()
	c := &SSEClient{dedup: d, apiClient: &Client{httpClient: &http.Client{Timeout: 30 * time.Second}}}

	// upgrade handler 返 err — dedup 不能 mark.
	failUp := &mockUp{err: fmt.Errorf("simulated handler panic recovered")}
	bp := &mockBP{}
	mb := &mockMB{}
	ev := sseEvent{
		ID:   "1",
		Type: sseEventUpgrade,
		Data: `{"task_id":"upg_fail","component":"octo-daemon"}`,
	}
	_ = c.dispatch(context.Background(), 1, ev, bp, failUp, mb)
	if d.seen(sseEventUpgrade, "upg_fail") {
		t.Error("handler err should NOT mark dedup (H3 fix: replay 应重试)")
	}

	// 然后改成 ok handler, 再 dispatch — 现在该 mark.
	okUp := &mockUp{}
	_ = c.dispatch(context.Background(), 1, ev, bp, okUp, mb)
	if !d.seen(sseEventUpgrade, "upg_fail") {
		t.Error("handler ok should mark dedup")
	}
}

// H3: bot_provision handler err 同样不 mark.
func TestDispatch_BotProvisionHandlerErrorLeavesDedupUnmarked(t *testing.T) {
	// mock fleet 返成功的 fetch, 让 dispatch 走到 bp.HandleBotProvision.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 42, "bot_uid": "bot_x", "bot_token": "bf_sec",
		})
	}))
	defer srv.Close()

	tempHome(t)
	d := newDedupState("daemon-h3-bp")
	_ = d.load()
	c := &SSEClient{
		fleetURL:  srv.URL,
		apiKey:    "uk_test",
		dedup:     d,
		apiClient: &Client{httpClient: &http.Client{Timeout: 30 * time.Second}},
	}

	failBP := &mockBP{err: fmt.Errorf("simulated handler panic recovered")}
	up := &mockUp{}
	mb := &mockMB{}
	ev := sseEvent{ID: "2", Type: sseEventBotProvision, Data: `{"command_id":"42"}`}
	_ = c.dispatch(context.Background(), 1, ev, failBP, up, mb)
	if d.seen(sseEventBotProvision, "42") {
		t.Error("bot_provision handler err should NOT mark dedup (H3)")
	}
}

// H7 caster review fix: readLoop 收到第一帧后 reset *delay 到 base —
// 防 backoff 长连接稳一段时间后断 仍走上次涨到的值.
func TestReadLoop_ResetsBackoffOnFirstFrame(t *testing.T) {
	tempHome(t)
	d := newDedupState("daemon-h7")
	_ = d.load()
	c := &SSEClient{dedup: d, apiClient: &Client{httpClient: &http.Client{Timeout: 30 * time.Second}}}

	body := `event: managed_bots_changed
id: 1
data: {"added":["bot_a"],"removed":[]}

`
	r := readerString(body + "\n")
	bp := &mockBP{}
	up := &mockUp{}
	mb := &mockMB{}
	delay := 32 * time.Second // simulate exp-backoff 涨到的值
	_ = c.readLoop(context.Background(), 7, r, &delay, bp, up, mb)
	if delay != sseReconnectBaseDelay {
		t.Errorf("delay should reset to base (%v) after first frame, got %v",
			sseReconnectBaseDelay, delay)
	}
}

// F2 caster review fix: persist 必须在 mu 内, 不然并发 mark 慢 goroutine
// snap 后 rename 覆盖快的, 丢失 entry.
//
// 并发 N goroutine 各自 mark (不同 type/id) 万次, 最后 reload file, 断言
// 全部 mark 都在 file 里 (没丢). 跑 -race 必须 clean.
func TestDedupState_ConcurrentMarkPersistsAllEntries(t *testing.T) {
	tempHome(t)
	d := newDedupState("daemon-f2")
	if err := d.load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	// 4 goroutine 模拟 4 runtime SSE goroutine 并发 mark.
	const goroutines = 4
	const perGoroutine = 50

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			eventType := sseEventUpgrade
			if gid%2 == 0 {
				eventType = sseEventBotProvision
			}
			for i := 0; i < perGoroutine; i++ {
				id := fmt.Sprintf("g%d-i%d", gid, i)
				if err := d.mark(eventType, id); err != nil {
					t.Errorf("mark(%s,%s): %v", eventType, id, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	// reload from file — 任何丢失的 mark 都没在重 load 后的 entries 里.
	d2 := newDedupState("daemon-f2")
	if err := d2.load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	missing := 0
	for g := 0; g < goroutines; g++ {
		eventType := sseEventUpgrade
		if g%2 == 0 {
			eventType = sseEventBotProvision
		}
		for i := 0; i < perGoroutine; i++ {
			id := fmt.Sprintf("g%d-i%d", g, i)
			if !d2.seen(eventType, id) {
				missing++
				if missing <= 5 {
					t.Errorf("mark lost after concurrent persist: %s/%s", eventType, id)
				}
			}
		}
	}
	if missing > 0 {
		t.Errorf("total %d / %d marks lost (F2 race regression — persist 必须在 mu 内)",
			missing, goroutines*perGoroutine)
	}
}

// B1 (caster review final from codex): claim atomic, 并发同 source_pk 只
// 有一个 goroutine 返 true. 防 SSE+heartbeat 双路径在 mark-after-handler
// 窗口同时 spawn handler.
func TestDedupState_ConcurrentClaimOnlyOneWins(t *testing.T) {
	tempHome(t)
	d := newDedupState("daemon-b1")
	_ = d.load()

	const concurrency = 8
	const sourceID = "ping_race"
	var winners atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, _, err := d.claim(sseEventPing, sourceID)
			if err != nil {
				t.Errorf("claim err: %v", err)
				return
			}
			if ok {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Errorf("expected exactly 1 winner, got %d (B1: claim 必须 atomic)", got)
	}

	// unclaim 后下次 claim 又能成功
	if err := d.unclaim(sseEventPing, sourceID); err != nil {
		t.Fatalf("unclaim: %v", err)
	}
	ok, alreadyDone, err := d.claim(sseEventPing, sourceID)
	if err != nil || !ok || alreadyDone {
		t.Errorf("post-unclaim claim should succeed (ok=true, alreadyDone=false), got ok=%v alreadyDone=%v err=%v", ok, alreadyDone, err)
	}
}

// R4 (codex round 4 BLOCKER): claim 返三态. inflight (claimed=false,
// alreadyDone=false) 时 caller 不能 advance cursor, owner markDone 后
// 重新 claim 才能 alreadyDone=true.
func TestDedupState_ClaimInflightVsDone(t *testing.T) {
	tempHome(t)
	d := newDedupState("daemon-r4")
	_ = d.load()

	// 第一次 claim → ok
	ok, alreadyDone, err := d.claim(sseEventUpgrade, "upg_r4")
	if err != nil || !ok || alreadyDone {
		t.Fatalf("first claim want ok=true alreadyDone=false, got ok=%v done=%v err=%v", ok, alreadyDone, err)
	}

	// 第二次 claim 同 id → inflight (claimed=false, alreadyDone=false)
	ok, alreadyDone, err = d.claim(sseEventUpgrade, "upg_r4")
	if err != nil || ok || alreadyDone {
		t.Errorf("inflight claim want ok=false alreadyDone=false, got ok=%v done=%v err=%v", ok, alreadyDone, err)
	}

	// markDone 后第二次 claim → alreadyDone=true
	if err := d.markDone(sseEventUpgrade, "upg_r4"); err != nil {
		t.Fatalf("markDone: %v", err)
	}
	ok, alreadyDone, err = d.claim(sseEventUpgrade, "upg_r4")
	if err != nil || ok || !alreadyDone {
		t.Errorf("post-markDone claim want ok=false alreadyDone=true, got ok=%v done=%v err=%v", ok, alreadyDone, err)
	}

	// reload 后 done state 持久化
	d2 := newDedupState("daemon-r4")
	if err := d2.load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	ok, alreadyDone, err = d2.claim(sseEventUpgrade, "upg_r4")
	if err != nil || ok || !alreadyDone {
		t.Errorf("reload + claim done entry want alreadyDone=true, got ok=%v done=%v err=%v", ok, alreadyDone, err)
	}
}

// R4 round 5 (codex review final BLOCKER): daemon 重启后 inflight 必须
// drop (不该跨 restart 持久化), 否则 owner goroutine 死了没法 markDone/
// unclaim, 下次 claim 永久 (false, false, nil) 卡死循环.
//
// 验证: 手动写一个 phaseInflight entry 进 file, reload, claim 该 sourceID
// 必须返 (true, false, nil) — 表示重启后可重新 claim 处理.
func TestDedupState_ReloadDropsInflight(t *testing.T) {
	tempHome(t)
	d := newDedupState("daemon-r4-restart")
	_ = d.load()

	// mock: 直接构造 file with inflight entry (模拟 daemon crash 时 inflight
	// 已 persist 但没 markDone)
	d.mu.Lock()
	d.entries[sseEventUpgrade] = map[string]int64{"upg_orphan": time.Now().UnixMilli()}
	d.phases[sseEventUpgrade] = map[string]entryPhase{"upg_orphan": phaseInflight}
	snap := d.snapshotLocked()
	if err := d.persist(snap); err != nil {
		d.mu.Unlock()
		t.Fatalf("persist inflight: %v", err)
	}
	d.mu.Unlock()

	// reload — load 必须 drop 这个 inflight
	d2 := newDedupState("daemon-r4-restart")
	if err := d2.load(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// 验证 inflight 被 drop: claim 应该成功 (fresh)
	ok, alreadyDone, err := d2.claim(sseEventUpgrade, "upg_orphan")
	if err != nil {
		t.Fatalf("post-reload claim err: %v", err)
	}
	if !ok || alreadyDone {
		t.Errorf("重启后 orphan inflight 必须能重新 claim, got ok=%v alreadyDone=%v (R4 round 5: 防永久卡死)", ok, alreadyDone)
	}
}

// D-2 (lml2468 review): dedup hot-path size-triggered prune. 之前生产
// hot path (claim/markDone/unclaim/recordEventID) 无 prune, 24h 不重启
// file 累积所有 event volume × 4 runtime × 3 type, 每 markDone 全量
// marshal+rename O(N) 成本. seen() 的 lazy prune 是 test-only 零生产调用.
//
// 验证: mark 一堆过期 entry (ts > 24h), 再 markDone 100+ 次, prune
// 应当清掉过期 entries (size 不无限增).
func TestDedupState_HotPathPrunes(t *testing.T) {
	tempHome(t)
	d := newDedupState("daemon-d2")
	_ = d.load()

	// inject 200 过期 (ts 25h 前) done entries (mock 之前 markDone 留的 stale)
	d.mu.Lock()
	staleTS := time.Now().Add(-25 * time.Hour).UnixMilli()
	d.entries[sseEventPing] = make(map[string]int64, 200)
	d.phases[sseEventPing] = make(map[string]entryPhase, 200)
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("stale_%d", i)
		d.entries[sseEventPing][id] = staleTS
		d.phases[sseEventPing][id] = phaseDone
	}
	d.mu.Unlock()

	beforeSize := func() int {
		d.mu.Lock()
		defer d.mu.Unlock()
		return len(d.entries[sseEventPing])
	}
	if beforeSize() != 200 {
		t.Fatalf("setup: expected 200 stale entries, got %d", beforeSize())
	}

	// 触发 dedupHotPrunePeriod 次 markDone — 第 100 次应该 trigger prune
	for i := 0; i < dedupHotPrunePeriod; i++ {
		id := fmt.Sprintf("fresh_%d", i)
		if err := d.markDone(sseEventUpgrade, id); err != nil {
			t.Fatalf("markDone %s: %v", id, err)
		}
	}

	// prune 应当清掉 200 个 stale ping entries
	d.mu.Lock()
	stalePingRemaining := len(d.entries[sseEventPing])
	freshUpgradeCount := len(d.entries[sseEventUpgrade])
	d.mu.Unlock()
	if stalePingRemaining != 0 {
		t.Errorf("hot-path prune 必须清掉所有 stale done entries, 还剩 %d (D-2)", stalePingRemaining)
	}
	if freshUpgradeCount != dedupHotPrunePeriod {
		t.Errorf("fresh entries 不该被 prune 掉, expected %d, got %d", dedupHotPrunePeriod, freshUpgradeCount)
	}
}

// D-2: inflight entry 不能被 prune 掉 (有 owner 处理中)
func TestDedupState_HotPathPreservesInflight(t *testing.T) {
	tempHome(t)
	d := newDedupState("daemon-d2-inflight")
	_ = d.load()

	// inject 一个过期 inflight (ts 25h 前) — 模拟 owner 处理超久
	d.mu.Lock()
	d.entries[sseEventBotProvision] = map[string]int64{"long_inflight": time.Now().Add(-25 * time.Hour).UnixMilli()}
	d.phases[sseEventBotProvision] = map[string]entryPhase{"long_inflight": phaseInflight}
	d.pruneLocked()
	d.mu.Unlock()

	// inflight 应当保留 (即使 ts 过期 — 等 owner 完)
	d.mu.Lock()
	_, exists := d.entries[sseEventBotProvision]["long_inflight"]
	d.mu.Unlock()
	if !exists {
		t.Error("prune 不能动 inflight entry, 即使 ts 过期 (D-2: 防偷走 owner 处理权)")
	}
}

// B2 (caster review final from codex): handler 失败时 lastEventID 不能推进,
// 防失败 event 被 Last-Event-ID 跳过永不 replay.
func TestDispatch_HandlerErrorDoesNotAdvanceLastEventID(t *testing.T) {
	tempHome(t)
	d := newDedupState("daemon-b2")
	_ = d.load()
	c := &SSEClient{dedup: d, apiClient: &Client{httpClient: &http.Client{Timeout: 30 * time.Second}}}

	// upgrade handler 返 err — recordEventID 不能 update lastIDs.
	failUp := &mockUp{err: fmt.Errorf("simulated panic recovered")}
	bp := &mockBP{}
	mb := &mockMB{}
	ev := sseEvent{
		ID:   "100",
		Type: sseEventUpgrade,
		Data: `{"task_id":"upg_b2","component":"octo-daemon"}`,
	}
	derr := c.dispatch(context.Background(), 7, ev, bp, failUp, mb)
	if derr == nil {
		t.Error("dispatch 必须返 err 触发 readLoop 重连 (B2 round 3)")
	}
	if got := d.lastEventID(7); got != 0 {
		t.Errorf("handler err should NOT advance lastEventID, got %d (B2: 失败的 event 必须 replay)", got)
	}

	// 成功的 dispatch 才推进
	okUp := &mockUp{}
	derr = c.dispatch(context.Background(), 7, ev, bp, okUp, mb)
	if derr != nil {
		t.Errorf("成功 dispatch 不该返 err, got %v", derr)
	}
	if got := d.lastEventID(7); got != 100 {
		t.Errorf("handler ok should advance to 100, got %d", got)
	}
}

// B2 round 3 (codex): readLoop 看 dispatch err 必须 return 触发重连. 不
// 继续读后续 frame, 防 max cursor 越过失败 event id.
//
// 模拟: 1 个 fail event + 1 个 success event 在同 stream, readLoop 应该
// 在第一个失败后 return, 第二个 event 不被处理.
func TestReadLoop_DispatchErrorStopsReading(t *testing.T) {
	tempHome(t)
	d := newDedupState("daemon-b2-readloop")
	_ = d.load()
	c := &SSEClient{dedup: d, apiClient: &Client{httpClient: &http.Client{Timeout: 30 * time.Second}}}

	body := `event: upgrade
id: 100
data: {"task_id":"upg_fail","component":"octo-daemon"}

event: upgrade
id: 101
data: {"task_id":"upg_ok","component":"octo-daemon"}

`
	r := readerString(body + "\n")
	bp := &mockBP{}
	failUp := &mockUp{err: fmt.Errorf("simulated panic")}
	mb := &mockMB{}
	err := c.readLoop(context.Background(), 1, r, nil, bp, failUp, mb)
	if err == nil {
		t.Error("readLoop 必须返 err 让 RunForRuntime reconnect (B2 round 3)")
	}
	if failUp.calls.Load() != 1 {
		t.Errorf("dispatch 只该被调一次 (第二个 event 不能处理), got %d", failUp.calls.Load())
	}
	// cursor 应该停在 0 (第一个 event 失败), 第二个 event id=101 不被处理 → 不应推进
	if got := d.lastEventID(1); got != 0 {
		t.Errorf("第二 event 不该 advance cursor, got %d (B2 round 3: 失败后必须断流)", got)
	}
}

// ===== SSE wire format parsing =====

func TestSSEClient_ReadLoop_ParsesFrames(t *testing.T) {
	tempHome(t)
	// 只测 upgrade event — ping/bot_provision dispatch 会触发网络 IO
	// (ReportPing / fetchBotProvision), 单独测. 这里仅验 wire parser
	// 拆帧正确.
	body := `event: upgrade
id: 2
data: {"task_id":"upg_7","component":"octo-daemon","download_url":"https://x","target_version":"1.2.3"}

`
	d := newDedupState("daemon-parse")
	_ = d.load()
	client := &SSEClient{dedup: d, apiClient: &Client{httpClient: &http.Client{Timeout: 30 * time.Second}}}

	bp := &mockBP{}
	up := &mockUp{}
	mb := &mockMB{}
	r := readerString(body + "\n")
	go func() {
		_ = client.readLoop(context.Background(), 7, r, nil, bp, up, mb)
	}()
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case <-deadline:
			if up.calls.Load() == 0 {
				t.Error("upgrade handler not called (parse failed?)")
			}
			return
		default:
			if up.calls.Load() > 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestSSEClient_ReadLoop_IgnoresCommentLines(t *testing.T) {
	tempHome(t)
	body := `: keepalive

: another keepalive

event: managed_bots_changed
id: 3
data: {"added":["bot_a"],"removed":[]}

`
	d := newDedupState("daemon-comments")
	_ = d.load()
	client := &SSEClient{dedup: d, apiClient: &Client{httpClient: &http.Client{Timeout: 30 * time.Second}}}

	bp := &mockBP{}
	up := &mockUp{}
	mb := &mockMB{}
	r := readerString(body + "\n")
	go func() {
		_ = client.readLoop(context.Background(), 7, r, nil, bp, up, mb)
	}()
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case <-deadline:
			t.Error("managed_bots delta not applied")
			return
		default:
			if mb.added.Load() > 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestSSEClient_ReadLoop_CloseEventReconnects(t *testing.T) {
	tempHome(t)
	body := "event: close\ndata: ttl-expired\n\n"
	d := newDedupState("daemon-close")
	_ = d.load()
	client := &SSEClient{dedup: d, apiClient: &Client{httpClient: &http.Client{Timeout: 30 * time.Second}}}
	bp := &mockBP{}
	up := &mockUp{}
	mb := &mockMB{}
	r := readerString(body)
	err := client.readLoop(context.Background(), 7, r, nil, bp, up, mb)
	if err == nil {
		t.Error("close event should return err to trigger reconnect")
	}
}

// ===== fetchBotProvision =====

func TestFetchBotProvision_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/daemon/bot-provisions/42" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer uk_test" {
			t.Errorf("missing/wrong auth: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":           42,
			"action":       "bot.provision",
			"workspace_id": "ws-x",
			"display_name": "test bot",
			"bot_uid":      "bot_abc",
			"bot_token":    "bf_secret",
			"claim_token":  "claim_xyz",
		})
	}))
	defer srv.Close()

	tempHome(t)
	d := newDedupState("daemon-fetch")
	_ = d.load()
	client := &SSEClient{
		fleetURL:  srv.URL,
		apiKey:    "uk_test",
		apiClient: &Client{httpClient: &http.Client{Timeout: 30 * time.Second}},
		dedup:     d,
	}
	cmd, err := client.fetchBotProvision(context.Background(), "42")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if cmd == nil {
		t.Fatal("nil cmd")
	}
	if cmd.ID != 42 || cmd.BotUID != "bot_abc" || cmd.BotToken != "bf_secret" {
		t.Errorf("unexpected cmd: %+v", cmd)
	}
}

func TestFetchBotProvision_410Gone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"msg":"not provisionable"}`))
	}))
	defer srv.Close()

	tempHome(t)
	d := newDedupState("daemon-410")
	_ = d.load()
	client := &SSEClient{
		fleetURL:  srv.URL,
		apiKey:    "uk_test",
		apiClient: &Client{httpClient: &http.Client{Timeout: 30 * time.Second}},
		dedup:     d,
	}
	cmd, err := client.fetchBotProvision(context.Background(), "99")
	if err != nil {
		t.Errorf("410 should return (nil, nil), not err: %v", err)
	}
	if cmd != nil {
		t.Error("410 should return nil cmd")
	}
}

func TestFetchBotProvision_OtherError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	tempHome(t)
	d := newDedupState("daemon-500")
	_ = d.load()
	client := &SSEClient{
		fleetURL:  srv.URL,
		apiKey:    "uk_test",
		apiClient: &Client{httpClient: &http.Client{Timeout: 30 * time.Second}},
		dedup:     d,
	}
	cmd, err := client.fetchBotProvision(context.Background(), "1")
	if err == nil {
		t.Error("500 should error")
	}
	if cmd != nil {
		t.Error("500 should not return cmd")
	}
}

// ===== ApplyManagedBotsDelta =====

// D-1 (lml2468 review): Phase A 不接 added — 防 BotUID 当 WorkspaceID
// placeholder 导致 pollMatterTasksForManagedBots → handleMatterBotTask
// 跑错 openclaw workspace (1s SSE 到 vs 5-7s heartbeat snapshot 窗口).
// added 入参丢弃, 走 heartbeat snapshot 拿真 workspace_id.
func TestApplyManagedBotsDelta_PhaseAIgnoresAdded(t *testing.T) {
	d := &Daemon{}
	d.ApplyManagedBotsDelta([]string{"bot_a", "bot_b"}, nil)
	if len(d.managedBots) != 0 {
		t.Errorf("Phase A 必须忽略 added (走 heartbeat baseline), got %d bots: %+v (D-1: 防 placeholder WorkspaceID 跑错 workspace)",
			len(d.managedBots), d.managedBots)
	}
}

// D-1: added=[已存在] removed=[] 也应该 no-op (并非 idempotent, 是直接
// ignore). 防回归到 "为了兼容老 test 又加回 added 路径".
func TestApplyManagedBotsDelta_PhaseAAddedNoOpEvenIfExists(t *testing.T) {
	d := &Daemon{
		managedBots: []ManagedBot{{BotUID: "bot_a", WorkspaceID: "ws_a"}},
	}
	d.ApplyManagedBotsDelta([]string{"bot_a", "bot_b"}, nil)
	if len(d.managedBots) != 1 {
		t.Errorf("Phase A added 全 ignore, 已有 bot_a 不动, bot_b 不加, got %d: %+v",
			len(d.managedBots), d.managedBots)
	}
}

func TestApplyManagedBotsDelta_Remove(t *testing.T) {
	d := &Daemon{
		managedBots: []ManagedBot{
			{BotUID: "bot_a", WorkspaceID: "ws_a"},
			{BotUID: "bot_b", WorkspaceID: "ws_b"},
		},
	}
	d.ApplyManagedBotsDelta(nil, []string{"bot_a"})
	if len(d.managedBots) != 1 || d.managedBots[0].BotUID != "bot_b" {
		t.Errorf("expected only bot_b, got %+v", d.managedBots)
	}
}

func TestApplyManagedBotsDelta_RemoveMissingNoOp(t *testing.T) {
	d := &Daemon{
		managedBots: []ManagedBot{{BotUID: "bot_a", WorkspaceID: "ws_a"}},
	}
	d.ApplyManagedBotsDelta(nil, []string{"bot_zzz"})
	if len(d.managedBots) != 1 {
		t.Errorf("removing nonexistent should be no-op, got %d", len(d.managedBots))
	}
}

// ===== SSE end-to-end via httptest =====

func TestSSEClient_ConnectOnce_DispatchUpgradeEvent(t *testing.T) {
	tempHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/daemon/events" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("runtime_id") != "7" {
			t.Errorf("missing runtime_id query: %v", r.URL.Query())
		}
		if r.Header.Get("Authorization") != "Bearer uk_test" {
			t.Errorf("missing/wrong auth header")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "event: upgrade\nid: 100\ndata: {\"task_id\":\"upg_99\",\"component\":\"octo-daemon\",\"download_url\":\"https://example\",\"target_version\":\"2.0.0\"}\n\n")
		flusher.Flush()
		_, _ = fmt.Fprint(w, "event: close\ndata: bye\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	d := newDedupState("daemon-e2e")
	_ = d.load()
	c := &SSEClient{
		fleetURL:  srv.URL,
		apiKey:    "uk_test",
		apiClient: &Client{httpClient: &http.Client{Timeout: 30 * time.Second}},
		dedup:     d,
	}
	bp := &mockBP{}
	up := &mockUp{}
	mb := &mockMB{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := c.connectOnce(ctx, 7, nil, bp, up, mb)
	if err == nil {
		t.Error("expected err on close-event triggered EOF")
	}
	if up.calls.Load() != 1 {
		t.Errorf("expected 1 upgrade dispatch, got %d", up.calls.Load())
	}
	// dedup should record it
	if !d.seen(sseEventUpgrade, "upg_99") {
		t.Error("upgrade should be marked in dedup")
	}
}

// ===== mocks =====

type mockBP struct {
	calls atomic.Int64
	err   error // set non-nil to simulate handler panic-recover
}

func (m *mockBP) HandleBotProvision(ctx context.Context, cmd *PendingAgentCommand) error {
	m.calls.Add(1)
	return m.err
}

type mockUp struct {
	calls atomic.Int64
	err   error
}

func (m *mockUp) HandleUpgrade(ctx context.Context, up *PendingUpgrade) error {
	m.calls.Add(1)
	return m.err
}

type mockMB struct {
	mu      sync.Mutex
	added   atomic.Int64
	removed atomic.Int64
}

func (m *mockMB) ApplyManagedBotsDelta(added, removed []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.added.Add(int64(len(added)))
	m.removed.Add(int64(len(removed)))
}

// readerString 包成 ReadCloser 用作 readLoop 的 io.Reader 输入.
func readerString(s string) io.Reader {
	return &stringReader{s: s}
}

type stringReader struct {
	s   string
	pos int
}

func (r *stringReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.pos:])
	r.pos += n
	return n, nil
}
