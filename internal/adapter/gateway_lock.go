package adapter

import "sync"

// GatewayLock serializes every cc-channel-octo lifecycle subprocess call
// (start / restart / upgrade) across the process. The daemon's own provision /
// upgrade paths hold it with the blocking Lock while they run; the machine-level
// auto-start watchdog probes it with TryLock and skips its tick when the lock is
// held, so it never races a daemon-initiated restart (the exact conflict that
// rules out pm2 supervising cc-channel-octo).
//
// All methods are nil-safe: a nil *GatewayLock behaves as "no coordination"
// (Lock/Unlock are no-ops, TryLock always succeeds), so tests and adapters that
// don't need coordination can pass nil.
type GatewayLock struct {
	mu sync.Mutex
}

// NewGatewayLock returns a ready-to-use lock.
func NewGatewayLock() *GatewayLock { return &GatewayLock{} }

// Lock blocks until the lock is acquired. Used by daemon-initiated lifecycle
// calls that must run to completion.
func (g *GatewayLock) Lock() {
	if g == nil {
		return
	}
	g.mu.Lock()
}

// Unlock releases the lock.
func (g *GatewayLock) Unlock() {
	if g == nil {
		return
	}
	g.mu.Unlock()
}

// TryLock acquires the lock without blocking, returning false if it is already
// held. Used by the watchdog to skip a tick when a daemon op is in progress. A
// nil lock reports success (nothing to coordinate with).
func (g *GatewayLock) TryLock() bool {
	if g == nil {
		return true
	}
	return g.mu.TryLock()
}
