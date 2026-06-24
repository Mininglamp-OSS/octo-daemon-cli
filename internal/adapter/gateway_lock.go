package adapter

import "context"

// GatewayLock serializes every cc-channel-octo lifecycle subprocess call
// (start / restart / upgrade) across the process. Daemon-initiated provision /
// upgrade paths take it with the context-aware Acquire so a wedged holder
// degrades to a bounded, logged error instead of hanging forever; the
// machine-level auto-start watchdog probes it with TryLock and skips its tick
// when the lock is held, so it never races a daemon-initiated restart (the
// exact conflict that rules out pm2 supervising cc-channel-octo).
//
// Backed by a capacity-1 channel rather than a sync.Mutex so Acquire can race
// the caller's context. All methods are nil-safe: a nil *GatewayLock behaves as
// "no coordination" (Acquire/Unlock no-op, TryLock always succeeds), so tests
// and adapters that don't need coordination can pass nil.
type GatewayLock struct {
	ch chan struct{}
}

// NewGatewayLock returns a ready-to-use lock.
func NewGatewayLock() *GatewayLock { return &GatewayLock{ch: make(chan struct{}, 1)} }

// Acquire blocks until the lock is acquired or ctx is done, returning ctx.Err()
// in the latter case. Used by daemon-initiated lifecycle calls so a hung holder
// (e.g. a stuck watchdog start) caps out at the caller's timeout instead of
// blocking the whole host's provision/upgrade path indefinitely.
func (g *GatewayLock) Acquire(ctx context.Context) error {
	if g == nil {
		return nil
	}
	select {
	case g.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Unlock releases the lock. The non-blocking drain makes a stray Unlock (one not
// paired with a successful Acquire/TryLock) a no-op rather than a deadlock.
func (g *GatewayLock) Unlock() {
	if g == nil {
		return
	}
	select {
	case <-g.ch:
	default:
	}
}

// TryLock acquires the lock without blocking, returning false if it is already
// held. Used by the watchdog to skip a tick when a daemon op is in progress. A
// nil lock reports success (nothing to coordinate with).
func (g *GatewayLock) TryLock() bool {
	if g == nil {
		return true
	}
	select {
	case g.ch <- struct{}{}:
		return true
	default:
		return false
	}
}
