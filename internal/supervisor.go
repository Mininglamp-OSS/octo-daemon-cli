package internal

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-daemon-cli/internal/adapter"
)

const (
	runnerRestartBaseDelay = 2 * time.Second
	runnerRestartMaxDelay  = 60 * time.Second
)

// Supervisor runs one backendRunner (Daemon) per profile inside a single
// process (goroutine fan-out). It owns the single-instance lock and the shared,
// stateless adapter registry.
//
// Fault isolation: a runner panic/error is contained and restarted with
// backoff; a per-space fatal (403 → 78) stops only that runner; a process-wide
// fatal (exit 75) tears down the whole process so the service manager / k8s
// respawns it. Daemon upgrade no longer drives 75 — it stops gracefully
// (exit 0) and lets the supervisor restart on the new binary; 75 remains a
// reserved respawn-request escape code with no current producer.
//
// NOTE: adapters are shared across all spaces and still write to per-machine
// host paths (~/.cc-channel-octo). When multiple spaces share one runtime these
// can collide — per-space namespacing is deferred (doc 16 step 8) pending the
// pod topology decision.
type Supervisor struct {
	profiles []Config
	registry *adapter.Registry
	// gwLock serializes cc-channel-octo lifecycle calls between the daemon's
	// provision/upgrade paths and the machine-level auto-start watchdog.
	gwLock *adapter.GatewayLock
}

// NewSupervisor builds the shared adapter registry once and binds the profiles.
func NewSupervisor(profiles []Config) (*Supervisor, error) {
	reg := adapter.NewRegistry()
	gwLock := adapter.NewGatewayLock()
	// Only openclaw + claude are supported runtimes (#52 dropped codex/hermes
	// detection and adapters; provider availability is driven by the
	// runtime-providers snapshot).
	for _, a := range []adapter.RuntimeAdapter{
		adapter.NewOpenclawAdapter(nil),
		adapter.NewClaudeAdapter(nil),
	} {
		if err := reg.Register(a); err != nil {
			return nil, fmt.Errorf("register runtime adapter: %w", err)
		}
	}
	return &Supervisor{profiles: profiles, registry: reg, gwLock: gwLock}, nil
}

// Run acquires the global lock, fans out one runner per profile, and blocks
// until ctx is cancelled, a process-wide fatal is escalated, or every profile
// has permanently stopped (all 403 → exit 78).
func (s *Supervisor) Run(ctx context.Context) error {
	if len(s.profiles) == 0 {
		return &ExitError{Code: 2, Message: "no profiles configured — run `octo-daemon config` first"}
	}

	lockFile, err := TryLock()
	if err != nil {
		// Lock conflict is a startup-level fatal (code 2). The npm-generated
		// pm2 ecosystem lists code 2 in stop_exit_codes to avoid restart loops.
		return &ExitError{Code: 2, Message: fmt.Sprintf("acquire daemon lock: %v", err)}
	}
	defer func() {
		RemovePID()
		_ = lockFile.Close()
		_ = os.Remove(LockFilePath())
	}()

	// Capture the parent before deriving the cancellable child, so we can later
	// distinguish a graceful signal shutdown (parent cancelled) from every
	// runner self-terminating on a permanent 403.
	parentCtx := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg        sync.WaitGroup
		fatalMu   sync.Mutex
		fatalExit *ExitError
	)
	setFatal := func(ee *ExitError) {
		fatalMu.Lock()
		if fatalExit == nil {
			fatalExit = ee
		}
		fatalMu.Unlock()
		cancel() // unwind all runners
	}

	// Machine-level device fingerprint — computed once here, serially, before the
	// per-profile goroutine fan-out (same pattern as EnsureDaemonID below). Doing
	// it per-goroutine would race on first install: concurrent profiles would all
	// miss the file and mint divergent ids. Non-fatal: an empty id degrades to
	// "" in the payload rather than taking the daemon down.
	deviceID, err := EnsureDeviceID()
	if err != nil {
		log.Printf("[WARN] device.id unavailable: %v", err)
	}

	// Machine-level cc-channel-octo auto-start watchdog. Single goroutine (the
	// global lock guarantees one per host), tied to ctx so it unwinds on
	// shutdown. Tracked in its own WaitGroup — it only returns on ctx.Done(), so
	// keeping it out of the per-profile wg lets wg.Wait() below still return when
	// every profile self-terminates on a 403 (which does not cancel ctx).
	var watchdogWg sync.WaitGroup
	watchdogWg.Add(1)
	go func() {
		defer watchdogWg.Done()
		s.runCcOctoWatchdog(ctx)
	}()

	started := 0
	for _, cfg := range s.profiles {
		cfg := cfg
		daemonID, err := EnsureDaemonID(cfg.SpaceID)
		if err != nil {
			// Bad space_id is a config error for this profile only; keep others.
			log.Printf("[ERROR] [%s] ensure daemon id: %v — skipping profile", cfg.SpaceID, err)
			continue
		}
		started++
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.runProfile(ctx, cfg, daemonID, deviceID, setFatal)
		}()
	}

	// No runner launched (every profile's space_id was invalid) means no daemon
	// is listening — that is a config error, not a successful run. Fail loudly so
	// it isn't mistaken for a clean exit.
	if started == 0 {
		return &ExitError{Code: 2, Message: "no valid profile to start — every configured space_id was invalid; run `octo-daemon config --space-id=... --server-url=... --fleet-url=... --api-key=...` to fix"}
	}

	wg.Wait()

	// Every profile runner has returned. Unwind the watchdog (a 403-driven stop
	// does not cancel ctx) and wait for it before computing the exit code.
	cancel()
	watchdogWg.Wait()

	fatalMu.Lock()
	defer fatalMu.Unlock()
	if fatalExit != nil {
		return fatalExit // process-wide fatal (exit 75 respawn request)
	}
	// All runners have stopped without a process-wide fatal. If this was not a
	// graceful signal shutdown, every profile self-terminated on a permanent
	// 403 — surface exit 78 so pm2's stop_exit_codes halts the service instead
	// of restarting a daemon whose every key is dead.
	if parentCtx.Err() == nil {
		return &ExitError{Code: 78, Message: "all profiles stopped: API key permanently rejected (403)"}
	}
	return nil // graceful shutdown
}

// runProfile runs one backendRunner with panic recovery and backoff restart,
// returning when ctx is cancelled. A per-space 403 (78) stops this runner; a
// reserved respawn-request exit code (75) is escalated to a process-wide fatal
// via setFatal. Daemon upgrade itself uses graceful stop (exit 0), not 75.
func (s *Supervisor) runProfile(ctx context.Context, cfg Config, daemonID, deviceID string, setFatal func(*ExitError)) {
	delay := runnerRestartBaseDelay
	for {
		if ctx.Err() != nil {
			return
		}
		err := s.runOnce(ctx, cfg, daemonID, deviceID)
		if ctx.Err() != nil {
			return
		}

		var ee *ExitError
		if errors.As(err, &ee) {
			switch ee.Code {
			case 75:
				log.Printf("[INFO] [%s] respawn requested (exit 75) — escalating to process exit", cfg.SpaceID)
				setFatal(ee)
				return
			case 78:
				log.Printf("[ERROR] [%s] API key rejected (403) — stopping this profile: %v", cfg.SpaceID, ee.Message)
				return
			}
		}

		if err != nil {
			log.Printf("[WARN] [%s] backend runner exited (%v) — restarting in %v", cfg.SpaceID, err, delay)
		} else {
			log.Printf("[INFO] [%s] backend runner exited — restarting in %v", cfg.SpaceID, delay)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay *= 2
		if delay > runnerRestartMaxDelay {
			delay = runnerRestartMaxDelay
		}
	}
}

// runOnce builds and runs a single backendRunner, converting a panic into an
// error so one space's crash never takes down the process.
func (s *Supervisor) runOnce(ctx context.Context, cfg Config, daemonID, deviceID string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in backend runner: %v", r)
		}
	}()
	d := newBackendRunner(cfg, s.registry, daemonID, deviceID, s.gwLock)
	return d.Run(ctx)
}
