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
)

type Daemon struct {
	cfg      Config
	client   *Client
	daemonID string
	lockFile *os.File

	mu                 sync.Mutex
	registeredRuntimes []RegisteredRuntime
	lastRuntimes       []RuntimeInfo
	generation         uint64
	// exitErr is set-once by requestExit; readExitErr consumes under d.mu.
	// Populated when the daemon wants Run() to return a specific ExitError
	// (403 → 78, upgrade → 75). Nil means "plain graceful shutdown, exit 0".
	exitErr *ExitError

	slowDetectRunning atomic.Bool
	slowDetectPending atomic.Bool
	cancel            context.CancelFunc
}

func NewDaemon(cfg Config) (*Daemon, error) {
	cfg.withDefaults()

	daemonID, err := EnsureDaemonID()
	if err != nil {
		return nil, fmt.Errorf("ensure daemon id: %w", err)
	}

	// PR-A.2 URL routing:
	//   OCTO_FLEET_URL  — runtime/bot endpoints (defaults to APIURL, so
	//                     existing deployments still work pre-fleet-cutover)
	//   OCTO_SERVER_URL — auth + bot_token endpoints (defaults to APIURL)
	// When both env vars are set, the client uses JWT for fleet calls;
	// otherwise it stays on the legacy api_key path so daemon binaries
	// pointed at an old server still function.
	fleetURL := os.Getenv("OCTO_FLEET_URL")
	if fleetURL == "" {
		fleetURL = cfg.APIURL
	}
	serverURL := os.Getenv("OCTO_SERVER_URL")
	if serverURL == "" {
		serverURL = cfg.APIURL
	}
	matterURL := os.Getenv("OCTO_MATTER_URL")
	client := NewClient(fleetURL, cfg.APIKey, cfg.CLIVersion)
	client.SetServerURL(serverURL)
	if matterURL != "" {
		client.SetMatterURL(matterURL)
	}
	// Enable JWT only when we have a distinct fleet URL (i.e., a real
	// PR-A.2 cutover). Otherwise the same baseURL means we're talking to
	// the old in-server runtime endpoints which still expect api_key.
	if os.Getenv("OCTO_FLEET_URL") != "" {
		client.EnableJWT(daemonID)
		log.Printf("[INFO] daemon JWT mode enabled (fleet=%s server=%s matter=%s)", fleetURL, serverURL, matterURL)
	}

	return &Daemon{
		cfg:      cfg,
		client:   client,
		daemonID: daemonID,
	}, nil
}

func (d *Daemon) Run(ctx context.Context) error {
	lockFile, err := TryLock()
	if err != nil {
		// Lock conflict is a startup-level fatal (code 2). Under service
		// manager the wrapper/Go main will map 2 → 0 to avoid restart loops.
		return &ExitError{Code: 2, Message: fmt.Sprintf("acquire daemon lock: %v", err)}
	}
	d.lockFile = lockFile
	defer func() {
		RemovePID()
		d.lockFile.Close()
		os.Remove(LockFilePath())
	}()

	ctx, cancel := context.WithCancel(ctx)
	d.cancel = cancel

	log.Printf("[INFO] daemon starting (id=%s, device=%s)", d.daemonID, d.cfg.DeviceName)

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
	hbErr := d.heartbeatLoop(ctx)

	// Prefer the set-once ExitError (403 → 78, upgrade → 75) over the
	// heartbeat loop's return. heartbeatLoop only returns on ctx.Done()
	// today which yields nil, but keep this explicit for safety.
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
	runtimes := DetectRuntimesFast()
	d.addDeviceName(runtimes)

	resp, err := d.client.Register(ctx, d.buildRegisterRequest(runtimes))
	if err != nil {
		d.checkForbidden(err)
		return 0, err
	}

	d.mu.Lock()
	d.generation++
	d.lastRuntimes = runtimes
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
	runtimes := DetectRuntimesFast()
	d.addDeviceName(runtimes)
	runtimes = EnrichOpenclawRuntime(runtimes)

	resp, err := d.client.Register(ctx, d.buildRegisterRequest(runtimes))
	if err != nil {
		d.checkForbidden(err)
		return 0, err
	}

	d.mu.Lock()
	d.generation++
	d.lastRuntimes = runtimes
	d.registeredRuntimes = resp.Runtimes
	gen := d.generation
	d.mu.Unlock()

	return gen, nil
}

func (d *Daemon) register(ctx context.Context) error {
	runtimes := DetectRuntimesFast()
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

	resp, err := d.client.Register(ctx, d.buildRegisterRequest(runtimes))
	if err != nil {
		d.checkForbidden(err)
		return err
	}

	d.mu.Lock()
	d.generation++
	d.lastRuntimes = runtimes
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

	current := DetectRuntimesFast()
	d.addDeviceName(current)
	current = EnrichOpenclawRuntime(current)

	// Early exit if generation advanced during detection
	d.mu.Lock()
	if d.generation != baseGen {
		d.mu.Unlock()
		log.Printf("[DEBUG] slow detect discarded (gen %d → %d)", baseGen, d.generation)
		return
	}
	changed := runtimesChanged(d.lastRuntimes, current)
	d.mu.Unlock()

	if changed {
		log.Printf("[INFO] runtime changes detected, re-registering...")
		d.doRegister(ctx, current, baseGen)
	}
}

// doRegister sends runtimes to server. Only commits if generation still matches.
func (d *Daemon) doRegister(ctx context.Context, runtimes []RuntimeInfo, expectedGen uint64) {
	resp, err := d.client.Register(ctx, d.buildRegisterRequest(runtimes))
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
	d.registeredRuntimes = resp.Runtimes
	d.mu.Unlock()
	log.Printf("[INFO] registered %d runtime(s) (gen=%d)", len(resp.Runtimes), expectedGen+1)
}

func (d *Daemon) checkForbidden(err error) bool {
	var forbiddenErr *ForbiddenError
	if errors.As(err, &forbiddenErr) {
		log.Printf("[ERROR] API key rejected (403): user is no longer a member of this space. Stopping daemon.")
		// Record exit 78 (config-level fatal). Under service manager main
		// maps 78 → 0 to prevent an infinite restart loop on a permanently
		// bad api key.
		d.requestExit(&ExitError{Code: 78, Message: "API key rejected: user is no longer a member of this space"})
		return true
	}
	return false
}

func (d *Daemon) buildRegisterRequest(runtimes []RuntimeInfo) RegisterRequest {
	return RegisterRequest{
		DaemonID:            d.daemonID,
		DeviceName:          d.cfg.DeviceName,
		DeviceInfo:          GetDeviceInfo(),
		CLIVersion:          d.cfg.CLIVersion,
		HeartbeatIntervalMs: d.cfg.HeartbeatInterval.Milliseconds(),
		Runtimes:            runtimes,
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
		// Handle pending ping request from server
		if resp.PendingPing != nil {
			go func(pp *PendingPing) {
				if err := d.client.ReportPing(ctx, pp.PingID); err != nil {
					log.Printf("[WARN] ping report failed: %v", err)
				} else {
					log.Printf("[INFO] ping reported (id=%s)", pp.PingID)
				}
			}(resp.PendingPing)
		}
		// Handle pending upgrade task from server
		if resp.PendingUpgrade != nil {
			go d.handleUpgrade(ctx, resp.PendingUpgrade)
		}
		// Handle pending managed-agent provisioning command from server.
		// Off-loaded to a goroutine so the heartbeat loop is not blocked by
		// openclaw CLI subprocess time (can take ~5s for agents add).
		if resp.PendingCommand != nil {
			// PoC4: server only emits "bot.provision" now; the old
			// "agent.create" and "bot.add" actions are gone.
			if resp.PendingCommand.Action == "bot.provision" {
				go d.handleBotProvision(ctx, resp.PendingCommand)
			} else {
				log.Printf("[WARN] unknown pending command action=%q id=%d — ignoring",
					resp.PendingCommand.Action, resp.PendingCommand.ID)
			}
		}
		// Same off-loading pattern for matter-driven bot tasks. These can
		// take much longer (openclaw agent runs are minutes, not seconds).
		if resp.PendingTask != nil {
			go d.handleBotTask(ctx, resp.PendingTask)
		}
		// PR-B.2: matter ownership of bot_task. fleet emits managed_bots
		// in heartbeat response; daemon pulls each bot's queued tasks
		// from matter directly (skipping the fleet pending_task path).
		if len(resp.ManagedBots) > 0 {
			d.pollMatterTasksForManagedBots(ctx, resp.ManagedBots)
		}
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
