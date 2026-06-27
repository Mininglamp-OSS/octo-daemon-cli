package internal

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mininglamp-OSS/octo-daemon-cli/internal/adapter"
)

// Daemon is the per-space backend runner: one Client + optional SSEClient +
// heartbeat/register loops bound to a single profile (space_id). The process
// runs one Daemon per profile under a Supervisor; the single-instance lock and
// the shared adapter registry live on the Supervisor, not here.
type Daemon struct {
	cfg       Config
	client    *Client
	registry  *adapter.Registry
	sseClient *SSEClient
	daemonID  string
	deviceID  string
	// gwLock serializes this daemon's cc-channel-octo lifecycle subprocess calls
	// (upgrade/restart/start) against the machine-level auto-start watchdog.
	gwLock *adapter.GatewayLock
	// providers is this daemon's own runtime-provider snapshot. Per-daemon (not
	// package-global) so multi-profile fan-out doesn't let one space's refresh
	// clobber another's.
	providers *providerStore

	mu                 sync.Mutex
	registeredRuntimes []RegisteredRuntime
	lastRuntimes       []RuntimeInfo
	lastComponents     []DeviceComponent
	generation         uint64
	// managedBots is refreshed by the heartbeat handler and the SSE
	// managed_bots_changed delta. TODO: re-add a consumer when matter-driven
	// task pull is reintroduced (the pull loop was removed in this pass).
	managedBots []ManagedBot
	// exitErr is set-once by requestExit; readExitErr consumes under d.mu.
	// Populated when the daemon wants Run() to return a specific ExitError
	// (403 → 78; 75 remains reserved for respawn requests). Nil means "plain
	// graceful shutdown, exit 0".
	exitErr *ExitError

	slowDetectRunning atomic.Bool
	slowDetectPending atomic.Bool
	cancel            context.CancelFunc
}

// newBackendRunner builds a per-space Daemon. The caller (Supervisor) supplies
// the shared adapter registry, the space's daemonID (ensured via EnsureDaemonID),
// and the machine-level deviceID (ensured once before the per-profile fan-out so
// concurrent profiles can't mint divergent ids). Backend URLs come from the
// profile config — there is no OCTO_FLEET_URL/OCTO_SERVER_URL env routing anymore.
func newBackendRunner(cfg Config, registry *adapter.Registry, daemonID, deviceID string, gwLock *adapter.GatewayLock) *Daemon {
	cfg.withDefaults()

	client := NewClient(cfg.FleetURL, cfg.APIKey, cfg.CLIVersion)
	client.SetServerURL(cfg.ServerURL)

	return &Daemon{
		cfg:       cfg,
		client:    client,
		registry:  registry,
		daemonID:  daemonID,
		deviceID:  deviceID,
		gwLock:    gwLock,
		providers: newProviderStore(),
	}
}

// runtimeAdapter resolves the adapter for a command's runtime_kind. An empty
// kind (older fleet builds, or task payloads that don't carry it yet) is
// normalized to openclaw by Registry.Get.
func (d *Daemon) runtimeAdapter(kind string) (adapter.RuntimeAdapter, error) {
	return d.registry.Get(kind)
}

// initSSEClient 构造 SSE client (在 newBackendRunner 之外, lazy 因为
// SSEClient.load 读 dedup file 失败不应阻塞构造 — daemon 还能跑只是无 SSE).
// Run() 之前调一次, 失败则 SSE 不启 daemon 走 heartbeat 兜底.
//
// 决策三 §D3 SSE endpoint URL config: 直接复用 profile 的 fleet_url, SSE
// endpoint 是 fleet 子路径 /v1/daemon/events. 无需单独 SSE base URL. 紧急
// rollback: set OCTO_SSE_DISABLED=1 跳过 init, daemon 走 heartbeat 兜底
// (Phase A 主路径 graceful; heartbeat-only rollback 存在 terminal ack/report
// 失败后无 retry 路径的已知 caveat, task 会卡到 sweeper timeout — 见
// runHeartbeatUpgrade / runHeartbeatBotProvision sseClient==nil 分支 [WARN]
// log 和 TODO Phase B 注释). Phase B 后谨慎 — 那时 heartbeat 已不带 pending.
func (d *Daemon) initSSEClient() {
	if v := os.Getenv("OCTO_SSE_DISABLED"); v == "1" || v == "true" {
		log.Printf("[INFO] SSE disabled by OCTO_SSE_DISABLED env, falling back to heartbeat pending")
		return
	}
	sse, err := NewSSEClient(d.cfg.FleetURL, d.cfg.APIKey, d.cfg.SpaceID, d.client)
	if err != nil {
		log.Printf("[WARN] SSE init failed (%v) — heartbeat 兜底继续工作", err)
		return
	}
	d.sseClient = sse
}

// ===== 决策三 SSE dispatcher adapter methods =====
//
// SSEClient 内部声明的 dispatcher interfaces (botProvisionDispatcher /
// upgradeDispatcher / managedBotsDispatcher) 由 Daemon 实现 — 这里是
// adapter 方法, 把 SSE 收到的 event 转给现有 handleXxx.
//
// err 返回语义 (H3 caster review fix):
//   - nil = handler 跑完 (即便内部 ack-failed 也算 nil — handler 已 ack
//     到 fleet, 不要 dedup-replay 死循环坏 download URL)
//   - non-nil = handler panic (我们 defer recover 捕获) 或 framework 级
//     dispatching 失败. dedup 不 mark, 下次 SSE replay 重试.

// HandleBotProvision 实现 botProvisionDispatcher. 复用现有
// handleBotProvision (内部已处理 missing bot_token / APIURL).
//
// Jerry-Xin Critical fix: handleBotProvision 改返 error, 这里透传.
// terminal ack (AckBot active/failed) 失败时 err != nil → dispatcher
// 不 markDone → SSE replay 兜底重试.
func (d *Daemon) HandleBotProvision(ctx context.Context, cmd *PendingAgentCommand) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handleBotProvision panic: %v", r)
			log.Printf("[ERROR] SSE bot_provision handler panic (id=%d): %v", cmd.ID, r)
		}
	}()
	return d.handleBotProvision(ctx, cmd)
}

// HandleUpgrade 实现 upgradeDispatcher. 复用现有 handleUpgrade.
// Jerry-Xin Critical fix: handleUpgrade 改返 error, 这里透传.
func (d *Daemon) HandleUpgrade(ctx context.Context, up *PendingUpgrade) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handleUpgrade panic: %v", r)
			log.Printf("[ERROR] SSE upgrade handler panic (task_id=%s): %v", up.TaskID, r)
		}
	}()
	return d.handleUpgrade(ctx, up)
}

// ApplyManagedBotsDelta 实现 managedBotsDispatcher. SSE delta 推到本地
// managedBots 缓存 — heartbeat snapshot 是 baseline (Phase A 双跑).
//
// **D-1 (lml2468 review) Phase A 重要约束**: 只 apply `removed`, **不 apply
// `added`**. 原因:
//   - fleet 端 dispatchManagedBotsChanged payload 是 `[]string` (只 bot_uid,
//     没 workspace_id). 如果 daemon 端 added 路径用 BotUID 当 WorkspaceID
//     placeholder, pollMatterTasksForManagedBots → handleMatterBotTask 会把
//     placeholder 当真 workspace 跑 openclaw agent → **data correctness
//     risk** (1s SSE 到 vs 5-7s heartbeat snapshot 之间的窗口跑错 workspace).
//   - mint → bot active 用户感知 latency 主要在 daemon fetch + openclaw
//     workspace create (秒级), 早 5s 加进 managedBots 不缩短 e2e latency.
//   - removed 是 correctness-positive (清掉 phantom bot 防 daemon 一直
//     poll matter 拿不存在 bot 的 task), 立刻 apply 安全.
//
// **Phase B 启动条件**: fleet SSE payload schema 改 `[{bot_uid, workspace_id}]`
// 对象后, daemon 端再开启 added 路径 (拿真 workspace_id, 不再用 placeholder).
//
// 幂等: removed bot 不存在 = no-op.
func (d *Daemon) ApplyManagedBotsDelta(added, removed []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// D-1 Phase A: added 列表丢弃, 不进 managedBots (防 placeholder
	// WorkspaceID 跑错 openclaw workspace). 走 heartbeat snapshot 拿真 workspace_id.
	_ = added
	if len(removed) == 0 {
		if len(added) > 0 {
			log.Printf("[INFO] SSE managed_bots delta: ignoring +%d added (Phase A, heartbeat baseline), 0 removed", len(added))
		}
		return
	}
	removeSet := make(map[string]struct{}, len(removed))
	for _, u := range removed {
		removeSet[u] = struct{}{}
	}
	filtered := d.managedBots[:0]
	for _, b := range d.managedBots {
		if _, drop := removeSet[b.BotUID]; !drop {
			filtered = append(filtered, b)
		}
	}
	d.managedBots = filtered
	addedSamples := truncateBotUIDList(added, 10)
	removedSamples := truncateBotUIDList(removed, 10)
	log.Printf("[INFO] SSE managed_bots delta applied (+%d ignored %v, -%d %v, now %d)",
		len(added), addedSamples, len(removed), removedSamples, len(d.managedBots))
}

// truncateBotUIDList 截断 bot_uid 列表用于 log (防 spam). 前 N 个 + 省略号.
// log 加具体 bot_uid 列表帮 oncall 定位.
func truncateBotUIDList(uids []string, max int) []string {
	if len(uids) <= max {
		return uids
	}
	out := make([]string, 0, max+1)
	out = append(out, uids[:max]...)
	out = append(out, fmt.Sprintf("...+%d more", len(uids)-max))
	return out
}

// startSSEForRuntimes 为每个 registeredRuntime 起一条 SSE goroutine
// (Q2: per-runtime conn). goroutine 自管 reconnect, ctx cancel 退出.
//
// M6 known limitation (caster review final, document only):
// 当前只在初始 Run() 后 snapshot 一次 registeredRuntimes 起 SSE goroutine.
// 后续 re-register (slowDetectLoop 检测到新 CLI / runtime crash 重 register)
// 不会动 SSE goroutine 池 — 新 runtime 走 heartbeat 兜底 (Phase A 双跑设计),
// 移除的 runtime SSE goroutine 也会继续跑 (fleet 端 ownership gate 会 403
// 让它无限重连退避). Phase B 加 reconcile (map[runtimeID]cancel, 每次
// re-register diff 启/停).
func (d *Daemon) startSSEForRuntimes(ctx context.Context) {
	d.mu.Lock()
	runtimes := append([]RegisteredRuntime(nil), d.registeredRuntimes...)
	d.mu.Unlock()
	for _, rt := range runtimes {
		go d.sseClient.RunForRuntime(ctx, rt.ID, d, d, d)
		log.Printf("[INFO] SSE started for runtime %d (%s)", rt.ID, rt.Provider)
	}
}

// runHeartbeatUpgrade / runHeartbeatBotProvision
// (B1 caster review final): heartbeat dispatch 加 dedup claim
// 跟 SSE path 共享同一 dedupState. 防 SSE+heartbeat 双跑窗口同一 event
// 被两条路径双处理.
//

func (d *Daemon) runHeartbeatUpgrade(ctx context.Context, up *PendingUpgrade) {
	if d.sseClient != nil {
		claimed, alreadyDone, cerr := d.sseClient.dedup.claim(sseEventUpgrade, up.TaskID)
		if cerr != nil {
			log.Printf("[WARN] heartbeat upgrade claim persist failed (task_id=%s): %v", up.TaskID, cerr)
			return
		}
		if alreadyDone || !claimed {
			return // SSE 已处理完 / 正在处理
		}
		// Jerry-Xin Critical fix: 不能无脑 markDone, 看 handler 返不返 error.
		// terminal ack failure (handler 内 reportUpgrade(failed) 失败) → handlerErr != nil
		// → unclaim (不 markDone), 下次 heartbeat 复跑.
		var handlerErr error
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[ERROR] heartbeat upgrade handler panic (task_id=%s): %v", up.TaskID, r)
				_ = d.sseClient.dedup.unclaim(sseEventUpgrade, up.TaskID)
				return
			}
			if handlerErr != nil {
				log.Printf("[WARN] heartbeat upgrade handler error (task_id=%s): %v — unclaim + retry next heartbeat", up.TaskID, handlerErr)
				_ = d.sseClient.dedup.unclaim(sseEventUpgrade, up.TaskID)
				return
			}
			_ = d.sseClient.dedup.markDone(sseEventUpgrade, up.TaskID)
		}()
		handlerErr = d.handleUpgrade(ctx, up)
		return
	}
	// sseClient == nil — emergency heartbeat-only rollback (OCTO_SSE_DISABLED=1
	// 或 SSE init 失败). 无 dedup, 无 SSE replay.
	//
	// 已知 pre-existing silent-drop: fleet `claimPendingUpgrade` 是 atomic
	// `UPDATE WHERE status='pending'`, 一旦转 dispatched 就不再推. 这里 handler
	// 内部 terminal report 失败时, fleet row 还是 dispatched 但 daemon 不知道
	// 要重试, task 卡到 sweeper timeout. 跟 SSE 之前老 heartbeat-only 代码
	// behavior 一致 — 不是 SSE PR 引入的回归.
	//
	// Jerry-Xin Critical fix (sseClient != nil 主路径) 已修. 紧急回退 path
	// 是 emergency-only (用户主动开 OCTO_SSE_DISABLED 接受 trade-off),
	// 不在本 fix scope. 这里 explicit [WARN] log 让 operator 可见.
	// TODO Phase B: 加 in-process retry queue + idempotency check 修复 silent
	// drop, 一并把 handler 拆成 install vs ack 两段 (避免 retry 整个 handler
	// 时 install 重复跑 — bot_provision idempotent OK 但 upgrade restart 后无法 retry).
	if err := d.handleUpgrade(ctx, up); err != nil {
		log.Printf("[WARN] heartbeat-only upgrade handler error (task_id=%s): %v — no retry path in OCTO_SSE_DISABLED mode, task may stick until sweeper timeout (Phase B fix pending)", up.TaskID, err)
	}
}

func (d *Daemon) runHeartbeatBotProvision(ctx context.Context, cmd *PendingAgentCommand) {
	commandID := fmt.Sprintf("%d", cmd.ID)
	if d.sseClient != nil {
		claimed, alreadyDone, cerr := d.sseClient.dedup.claim(sseEventBotProvision, commandID)
		if cerr != nil {
			log.Printf("[WARN] heartbeat bot_provision claim persist failed (id=%s): %v", commandID, cerr)
			return
		}
		if alreadyDone || !claimed {
			return
		}
		// Jerry-Xin Critical fix: 同 runHeartbeatUpgrade, terminal ack failure 不 markDone.
		var handlerErr error
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[ERROR] heartbeat bot_provision handler panic (id=%s): %v", commandID, r)
				_ = d.sseClient.dedup.unclaim(sseEventBotProvision, commandID)
				return
			}
			if handlerErr != nil {
				log.Printf("[WARN] heartbeat bot_provision handler error (id=%s): %v — unclaim + retry next heartbeat", commandID, handlerErr)
				_ = d.sseClient.dedup.unclaim(sseEventBotProvision, commandID)
				return
			}
			_ = d.sseClient.dedup.markDone(sseEventBotProvision, commandID)
		}()
		handlerErr = d.handleBotProvision(ctx, cmd)
		return
	}
	// sseClient == nil — emergency heartbeat-only rollback. 同 runHeartbeatUpgrade
	// 已知 pre-existing silent-drop (fleet claim 转 dispatched 后不再推, terminal
	// ack 失败 = task 卡到 sweeper). 不在 Jerry-Xin Critical fix scope.
	// TODO Phase B: 加 in-process retry queue 修复 (bot_provision idempotent, 简单些).
	if err := d.handleBotProvision(ctx, cmd); err != nil {
		log.Printf("[WARN] heartbeat-only bot_provision handler error (id=%s): %v — no retry path in OCTO_SSE_DISABLED mode, task may stick until sweeper timeout (Phase B fix pending)", commandID, err)
	}
}

// Run drives one space's register + heartbeat/SSE loops until ctx is cancelled
// or a fatal ExitError (403 → 78; 75 remains reserved for respawn requests) is
// recorded. The single-instance lock is held by the Supervisor, not acquired
// here.
func (d *Daemon) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	d.cancel = cancel

	log.Printf("[INFO] backend runner starting (space=%s id=%s device=%s)", d.cfg.SpaceID, d.daemonID, d.cfg.DeviceName)

	if err := d.register(ctx); err != nil {
		// If register tripped checkForbidden, an ExitError{78} is already
		// recorded via requestExit — return that in preference to the raw
		// initial-registration error so main maps exit code correctly.
		if ee := d.readExitErr(); ee != nil {
			return ee
		}
		return fmt.Errorf("initial registration: %w", err)
	}

	defer d.deregister()

	go d.slowDetectLoop(ctx)
	// 决策三 SSE 反向派发 (Phase A 双跑): 每 runtime 一条独立 SSE goroutine.
	// 启失败不 fatal — heartbeat pending_* 仍兜底.
	d.initSSEClient()
	if d.sseClient != nil {
		d.startSSEForRuntimes(ctx)
	}
	hbErr := d.heartbeatLoop(ctx)

	// Prefer the set-once ExitError (403 → 78; 75 remains reserved for respawn
	// requests) over the heartbeat loop's return. heartbeatLoop only returns on
	// ctx.Done() today which yields nil, but keep this explicit for safety.
	if ee := d.readExitErr(); ee != nil {
		return ee
	}
	return hbErr
}

// requestExit records an ExitError to be returned from Run(). Set-once:
// subsequent calls are ignored so the first signal (e.g. 403) isn't
// overridden by a later one. Always paired with d.cancel() so the run loop
// unwinds and returns via Run()'s tail.
func (d *Daemon) requestExit(err *ExitError) {
	if err == nil {
		return
	}
	d.mu.Lock()
	if d.exitErr == nil {
		d.exitErr = err
	}
	d.mu.Unlock()
	if d.cancel != nil {
		d.cancel()
	}
}

func (d *Daemon) readExitErr() *ExitError {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.exitErr
}

func (d *Daemon) addDeviceName(runtimes []RuntimeInfo) {
	for i := range runtimes {
		runtimes[i].Name = fmt.Sprintf("%s (%s)", capitalize(runtimes[i].Provider), d.cfg.DeviceName)
	}
}

// fastDetectAndRegister does quick detection + immediate register.
// Returns the generation after successful register.
func (d *Daemon) fastDetectAndRegister(ctx context.Context) (uint64, error) {
	if err := d.refreshProviders(ctx); err != nil {
		return 0, err // 403 终态:不再 detect/register
	}
	runtimes := DetectRuntimesFast(d.providers.current())
	d.addDeviceName(runtimes)
	comps := d.probeComponents()

	resp, err := d.client.Register(ctx, d.buildRegisterRequest(runtimes, comps))
	if err != nil {
		d.checkForbidden(err)
		return 0, err
	}

	d.mu.Lock()
	d.generation++
	d.lastRuntimes = runtimes
	d.lastComponents = comps
	d.registeredRuntimes = resp.Runtimes
	gen := d.generation
	d.mu.Unlock()

	return gen, nil
}

// enrichDetectAndRegister does full detection (fast + slow openclaw enrich)
// and unconditionally re-registers. Used after plugin upgrades so the server
// sees the new plugin version in metadata.plugins immediately (required by the
// close-out path in modules/runtime/api.go::completeUpgradeIfMatchedWithRuntime).
func (d *Daemon) enrichDetectAndRegister(ctx context.Context) (uint64, error) {
	if err := d.refreshProviders(ctx); err != nil {
		return 0, err // 403 终态:不再 detect/register(本路径可能用 background ctx)
	}
	runtimes := DetectRuntimesFast(d.providers.current())
	d.addDeviceName(runtimes)
	runtimes = EnrichOpenclawRuntime(runtimes)
	runtimes = EnrichClaudeRuntime(runtimes)
	comps := d.probeComponents()

	resp, err := d.client.Register(ctx, d.buildRegisterRequest(runtimes, comps))
	if err != nil {
		d.checkForbidden(err)
		return 0, err
	}

	d.mu.Lock()
	d.generation++
	d.lastRuntimes = runtimes
	d.lastComponents = comps
	d.registeredRuntimes = resp.Runtimes
	gen := d.generation
	d.mu.Unlock()

	return gen, nil
}

// refreshProviders 从 fleet 拉 active provider 列表刷新本地快照。
//   - 403:API key 被撤销 → checkForbidden 触发退出;返回 err,挂点应中止
//     后续 detect/register(尤其 handleComponentUpgrade 用脱离 daemon ctx 的
//     background context,不靠 ctx cancel 兜底)。
//   - 404 / 网络错误:老 fleet 无端点或抖动 → 保留上次快照,返回 nil(非终态)。
//   - 200(含空列表):以 fleet 为权威整体替换(按本地支持集过滤);空列表清空
//     快照(fleet 把全部 provider disable 的合法语义,不能退化成旧快照)。
//
// 并发刷新用版本门禁:发请求前拿递增序号,只有最新开始的刷新允许写快照,
// 避免较早发出但较晚返回的响应覆盖更新的快照。
func (d *Daemon) refreshProviders(ctx context.Context) error {
	mySeq := d.providers.nextSeq()
	rows, err := d.client.ListProviders(ctx)
	if err != nil {
		if d.checkForbidden(err) {
			return err // 已 requestExit,挂点据此中止后续 detect/register
		}
		log.Printf("[INFO] refresh providers skipped (keep last snapshot): %v", err)
		return nil
	}
	if d.providers.setIfNewer(providersFromFleet(rows, d.registry.Kinds()), mySeq) {
		log.Printf("[INFO] provider snapshot refreshed: %d active", len(rows))
	}
	return nil
}

func (d *Daemon) register(ctx context.Context) error {
	if err := d.refreshProviders(ctx); err != nil {
		return err // 403 终态:不再 detect/register
	}
	runtimes := DetectRuntimesFast(d.providers.current())
	d.addDeviceName(runtimes)

	if len(runtimes) == 0 {
		log.Printf("[WARN] no agent runtimes detected on this machine")
	}

	for _, r := range runtimes {
		log.Printf("[INFO] detected: %s %s (%s)", r.Provider, r.Version, r.Path)
		if r.Status != "online" {
			log.Printf("[INFO]   %s status: %s (will skip heartbeat)", r.Provider, r.Status)
		}
	}

	comps := d.probeComponents()

	resp, err := d.client.Register(ctx, d.buildRegisterRequest(runtimes, comps))
	if err != nil {
		d.checkForbidden(err)
		return err
	}

	d.mu.Lock()
	d.generation++
	d.lastRuntimes = runtimes
	d.lastComponents = comps
	d.registeredRuntimes = resp.Runtimes
	gen := d.generation
	d.mu.Unlock()
	log.Printf("[INFO] registered %d runtime(s) with server (gen=%d)", len(resp.Runtimes), gen)

	d.requestSlowDetect(ctx)
	return nil
}

func (d *Daemon) heartbeatLoop(ctx context.Context) error {
	ticker := time.NewTicker(d.cfg.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			d.sendHeartbeats(ctx)
		}
	}
}

// slowDetectLoop fires runtime re-detection on its own cadence. It used
// to piggy-back on heartbeatLoop (every 4 ticks) which silently coupled
// detection frequency to heartbeat tuning — when heartbeat dropped 15s→5s
// detection accelerated from 60s to 20s by accident. Own ticker pins
// detection to SlowDetectInterval (default 60s).
func (d *Daemon) slowDetectLoop(ctx context.Context) {
	ticker := time.NewTicker(d.cfg.SlowDetectInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.requestSlowDetect(ctx)
		}
	}
}

// requestSlowDetect asks for a slow detection round. If one is already running,
// sets pending flag so it will re-run when the current one finishes.
func (d *Daemon) requestSlowDetect(ctx context.Context) {
	if !d.slowDetectRunning.CompareAndSwap(false, true) {
		d.slowDetectPending.Store(true)
		return
	}

	go d.runSlowDetect(ctx)
}

func (d *Daemon) runSlowDetect(ctx context.Context) {
	defer func() {
		d.slowDetectRunning.Store(false)

		// If someone requested while we were busy, do another round
		if d.slowDetectPending.CompareAndSwap(true, false) {
			if d.slowDetectRunning.CompareAndSwap(false, true) {
				go d.runSlowDetect(ctx)
			}
		}
	}()

	d.mu.Lock()
	baseGen := d.generation
	d.mu.Unlock()

	if err := d.refreshProviders(ctx); err != nil {
		return // 403 终态:已 requestExit,不再 detect/register
	}
	current := DetectRuntimesFast(d.providers.current())
	d.addDeviceName(current)
	current = EnrichOpenclawRuntime(current)
	current = EnrichClaudeRuntime(current)
	comps := d.probeComponents()

	// Early exit if generation advanced during detection
	d.mu.Lock()
	if d.generation != baseGen {
		d.mu.Unlock()
		log.Printf("[DEBUG] slow detect discarded (gen %d → %d)", baseGen, d.generation)
		return
	}
	rtChanged := runtimesChanged(d.lastRuntimes, current)
	compChanged := componentsChanged(d.lastComponents, comps)
	d.mu.Unlock()

	if rtChanged || compChanged {
		var reasons []string
		if rtChanged {
			reasons = append(reasons, "runtime")
		}
		if compChanged {
			reasons = append(reasons, "device components")
		}
		log.Printf("[INFO] changes detected, re-registering (reason: %s)", joinStrings(reasons, ", "))
		d.doRegister(ctx, current, comps, baseGen)
	}
}

// doRegister sends runtimes to server. Only commits if generation still matches.
func (d *Daemon) doRegister(ctx context.Context, runtimes []RuntimeInfo, components []DeviceComponent, expectedGen uint64) {
	resp, err := d.client.Register(ctx, d.buildRegisterRequest(runtimes, components))
	if err != nil {
		if d.checkForbidden(err) {
			return
		}
		log.Printf("[WARN] register failed: %v", err)
		return
	}

	d.mu.Lock()
	if d.generation != expectedGen {
		d.mu.Unlock()
		log.Printf("[DEBUG] register result discarded (gen %d → %d)", expectedGen, d.generation)
		return
	}
	d.generation++
	d.lastRuntimes = runtimes
	d.lastComponents = components
	d.registeredRuntimes = resp.Runtimes
	d.mu.Unlock()
	log.Printf("[INFO] registered %d runtime(s) (gen=%d)", len(resp.Runtimes), expectedGen+1)
}

func (d *Daemon) checkForbidden(err error) bool {
	var forbiddenErr *ForbiddenError
	if errors.As(err, &forbiddenErr) {
		log.Printf("[ERROR] API key rejected (403): user is no longer a member of this space. Stopping daemon.")
		// Record exit 78 (config-level fatal) for a permanently bad api key.
		// The npm-generated pm2 ecosystem treats this as a stop code, avoiding
		// a restart loop for credentials that require operator action.
		d.requestExit(&ExitError{Code: 78, Message: "API key rejected: user is no longer a member of this space"})
		return true
	}
	return false
}

// probeComponents detects installed device components for a register payload.
// On probe failure (npm missing/timeout/garbage output) it reuses the last
// successfully-probed inventory instead of an empty slice, so a transient `npm
// ls` failure isn't reported to the server as an authoritative "everything was
// uninstalled" (which would flap component records). A genuine empty inventory
// (probe succeeded with nothing installed) is returned as-is.
//
// Always returns a non-nil slice: before any successful probe lastComponents is
// nil, and a nil slice would marshal as JSON null rather than the [] the
// device_components contract expects, so the nil case is normalized to [].
func (d *Daemon) probeComponents() []DeviceComponent {
	comps, err := DetectDeviceComponents()
	if err != nil {
		d.mu.Lock()
		comps = d.lastComponents
		d.mu.Unlock()
		log.Printf("[WARN] device component probe failed, reusing last known inventory: %v", err)
	}
	if comps == nil {
		comps = []DeviceComponent{}
	}
	return comps
}

func (d *Daemon) buildRegisterRequest(runtimes []RuntimeInfo, components []DeviceComponent) RegisterRequest {
	return RegisterRequest{
		DaemonID:            d.daemonID,
		DeviceName:          d.cfg.DeviceName,
		DeviceInfo:          GetDeviceInfo(d.deviceID),
		CLIVersion:          d.cfg.CLIVersion,
		HeartbeatIntervalMs: d.cfg.HeartbeatInterval.Milliseconds(),
		Runtimes:            runtimes,
		// Machine-level npm components, probed by the caller (DetectDeviceComponents)
		// so the same snapshot drives both the payload and lastComponents change
		// tracking. Probed on register paths (minutes apart at most), never on the
		// per-runtime heartbeat hot path.
		DeviceComponents: components,
	}
}

func (d *Daemon) sendHeartbeats(ctx context.Context) {
	d.mu.Lock()
	offlineProviders := make(map[string]bool)
	for _, r := range d.lastRuntimes {
		if r.Status == "offline" {
			offlineProviders[r.Provider] = true
		}
	}
	registered := make([]RegisteredRuntime, len(d.registeredRuntimes))
	copy(registered, d.registeredRuntimes)
	d.mu.Unlock()

	needReRegister := false
	// Accumulate managed_bots across runtimes within this cycle, dedup by
	// bot_uid, then assign once at the end. Without this, an empty
	// resp.ManagedBots from a non-hosting runtime would overwrite the bots
	// reported by the hosting runtime within the same cycle (runtime order
	// is incidental). Guarded by anyHeartbeatOK so a fully-failed cycle
	// doesn't clobber the prior snapshot — matterPullLoop keeps using the
	// last known good set until heartbeats recover.
	var collectedBots []ManagedBot
	seenBots := make(map[string]bool)
	anyHeartbeatOK := false

	for _, rt := range registered {
		if offlineProviders[rt.Provider] {
			continue
		}
		resp, err := d.client.Heartbeat(ctx, rt.ID)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if d.checkForbidden(err) {
				return
			}
			log.Printf("[WARN] heartbeat failed for runtime %d (%s): %v", rt.ID, rt.Provider, err)
			needReRegister = true
			continue
		}
		anyHeartbeatOK = true
		// Handle pending upgrade task from server
		if resp.PendingUpgrade != nil {
			go d.runHeartbeatUpgrade(ctx, resp.PendingUpgrade)
		}
		// Handle pending managed-agent provisioning command from server.
		// Off-loaded to a goroutine so the heartbeat loop is not blocked by
		// openclaw CLI subprocess time (can take ~5s for agents add).
		if resp.PendingCommand != nil {
			// PoC4: server only emits "bot.provision" now; the old
			// "agent.create" and "bot.add" actions are gone.
			if resp.PendingCommand.Action == "bot.provision" {
				go d.runHeartbeatBotProvision(ctx, resp.PendingCommand)
			} else {
				log.Printf("[WARN] unknown pending command action=%q id=%d — ignoring",
					resp.PendingCommand.Action, resp.PendingCommand.ID)
			}
		}
		// PR-B.2: accumulate managed_bots across runtimes. Whether the
		// server scopes managed_bots per-runtime or per-daemon, the union
		// (deduped) is the safe interpretation — never lose a bot just
		// because some sibling runtime reported it empty.
		for _, b := range resp.ManagedBots {
			if b.BotUID == "" || seenBots[b.BotUID] {
				continue
			}
			seenBots[b.BotUID] = true
			collectedBots = append(collectedBots, b)
		}
	}

	if anyHeartbeatOK {
		// Single assign after the loop — replaces the prior per-iteration
		// overwrite. matterPullLoop will see the union (possibly empty if
		// no bots managed). Skipping on all-fail keeps the last known set
		// rather than clobbering it on infrastructure outage.
		d.mu.Lock()
		d.managedBots = collectedBots
		d.mu.Unlock()
	}

	// Daemon-level heartbeat: maintain device online indicator ("green dot").
	// Sent unconditionally every tick — even when no runtimes are registered,
	// this is the liveness signal that keeps the device visible. best-effort:
	// log failure without interrupting the main loop.
	daemonHbErr := d.client.DaemonHeartbeat(ctx, DaemonHeartbeatRequest{
		DaemonID:          d.daemonID,
		DeviceUUID:        d.deviceID,
		HeartbeatIntervalMs: d.cfg.HeartbeatInterval.Milliseconds(),
	})
	if daemonHbErr != nil {
		if ctx.Err() != nil {
			return
		}
		log.Printf("[WARN] daemon heartbeat failed: %v", daemonHbErr)
	}

	if needReRegister {
		log.Printf("[INFO] heartbeat failure detected, fast re-register + async enrich...")
		gen, err := d.fastDetectAndRegister(ctx)
		if err != nil {
			log.Printf("[WARN] fast re-register failed: %v", err)
		} else {
			log.Printf("[INFO] fast re-registered (gen=%d), starting slow enrich...", gen)
			d.requestSlowDetect(ctx)
		}
	}
}

func (d *Daemon) deregister() {
	d.mu.Lock()
	ids := make([]int64, len(d.registeredRuntimes))
	for i, rt := range d.registeredRuntimes {
		ids[i] = rt.ID
	}
	d.mu.Unlock()

	if len(ids) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := d.client.Deregister(ctx, ids); err != nil {
		log.Printf("[WARN] deregister failed: %v", err)
		return
	}

	log.Printf("[INFO] deregistered %d runtime(s)", len(ids))
}

func runtimesChanged(old, current []RuntimeInfo) bool {
	if len(old) != len(current) {
		return true
	}
	oldMap := make(map[string]RuntimeInfo)
	for _, r := range old {
		oldMap[r.Provider] = r
	}
	for _, r := range current {
		prev, ok := oldMap[r.Provider]
		if !ok || prev.Version != r.Version || prev.Status != r.Status {
			return true
		}
		if agentsChanged(prev.Agents, r.Agents) {
			return true
		}
		if pluginsChanged(prev.Plugins, r.Plugins) {
			return true
		}
	}
	return false
}

// componentsChanged reports whether the installed device-component set differs
// from the last registered one — by count or by per-package version. Keyed on
// ComponentKey (the full npm package name). Absent (not-installed) packages are
// already filtered out by DetectDeviceComponents, so a changed count covers
// installs and uninstalls.
func componentsChanged(old, current []DeviceComponent) bool {
	if len(old) != len(current) {
		return true
	}
	oldMap := make(map[string]string, len(old))
	for _, c := range old {
		oldMap[c.ComponentKey] = c.Version
	}
	for _, c := range current {
		prev, ok := oldMap[c.ComponentKey]
		if !ok || prev != c.Version {
			return true
		}
	}
	return false
}

func agentsChanged(old, current []AgentEntry) bool {
	if len(old) != len(current) {
		return true
	}
	oldMap := make(map[string]AgentEntry, len(old))
	for _, a := range old {
		oldMap[a.ID] = a
	}
	for _, a := range current {
		prev, ok := oldMap[a.ID]
		if !ok || prev.Bindings != a.Bindings || prev.Default != a.Default {
			return true
		}
	}
	return false
}

func pluginsChanged(old, current []PluginInfo) bool {
	if len(old) != len(current) {
		return true
	}
	oldMap := make(map[string]string, len(old))
	for _, p := range old {
		oldMap[p.Name] = p.Version
	}
	for _, p := range current {
		prev, ok := oldMap[p.Name]
		if !ok || prev != p.Version {
			return true
		}
	}
	return false
}

func agentIDs(agents []AgentEntry) string {
	ids := make([]string, len(agents))
	for i, a := range agents {
		ids[i] = a.ID
	}
	return fmt.Sprintf("[%s]", joinStrings(ids, ", "))
}

func joinStrings(s []string, sep string) string {
	result := ""
	for i, v := range s {
		if i > 0 {
			result += sep
		}
		result += v
	}
	return result
}
