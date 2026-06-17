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
// fatal (upgrade → 75) tears down the whole process so the service manager / k8s
// respawns it on the new binary.
//
// NOTE: adapters are shared across all spaces and still write to per-machine
// host paths (~/.cc-channel-octo, ~/.hermes/.env). When multiple spaces share
// one runtime these can collide — per-space namespacing is deferred (doc 16
// step 8) pending the pod topology decision.
type Supervisor struct {
	profiles []Config
	registry *adapter.Registry
}

// NewSupervisor builds the shared adapter registry once and binds the profiles.
func NewSupervisor(profiles []Config) (*Supervisor, error) {
	reg := adapter.NewRegistry()
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
	return &Supervisor{profiles: profiles, registry: reg}, nil
}

// Run acquires the global lock, fans out one runner per profile, and blocks
// until ctx is cancelled or a process-wide fatal is escalated.
func (s *Supervisor) Run(ctx context.Context) error {
	if len(s.profiles) == 0 {
		return &ExitError{Code: 2, Message: "no profiles configured — run `octo-daemon config` first"}
	}

	lockFile, err := TryLock()
	if err != nil {
		// Lock conflict is a startup-level fatal (code 2). Under service
		// manager the wrapper/Go main maps 2 → 0 to avoid restart loops.
		return &ExitError{Code: 2, Message: fmt.Sprintf("acquire daemon lock: %v", err)}
	}
	defer func() {
		RemovePID()
		_ = lockFile.Close()
		_ = os.Remove(LockFilePath())
	}()

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
			s.runProfile(ctx, cfg, daemonID, setFatal)
		}()
	}

	// No runner launched (every profile's space_id was invalid) means no daemon
	// is listening — that is a config error, not a successful run. Fail loudly so
	// it isn't mistaken for a clean exit.
	if started == 0 {
		return &ExitError{Code: 2, Message: "no valid profile to start — every configured space_id was invalid; run `octo-daemon config --space-id=... --server-url=... --fleet-url=... --api-key=...` to fix"}
	}

	wg.Wait()

	fatalMu.Lock()
	defer fatalMu.Unlock()
	return fatalExit // nil = graceful shutdown
}

// runProfile runs one backendRunner with panic recovery and backoff restart,
// returning when ctx is cancelled. A per-space 403 (78) stops this runner; an
// upgrade (75) is escalated to a process-wide fatal via setFatal.
func (s *Supervisor) runProfile(ctx context.Context, cfg Config, daemonID string, setFatal func(*ExitError)) {
	delay := runnerRestartBaseDelay
	for {
		if ctx.Err() != nil {
			return
		}
		err := s.runOnce(ctx, cfg, daemonID)
		if ctx.Err() != nil {
			return
		}

		var ee *ExitError
		if errors.As(err, &ee) {
			switch ee.Code {
			case 75:
				log.Printf("[INFO] [%s] upgrade requested — escalating to process exit", cfg.SpaceID)
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
func (s *Supervisor) runOnce(ctx context.Context, cfg Config, daemonID string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in backend runner: %v", r)
		}
	}()
	d := newBackendRunner(cfg, s.registry, daemonID)
	return d.Run(ctx)
}
