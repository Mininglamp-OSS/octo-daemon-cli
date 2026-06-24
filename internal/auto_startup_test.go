package internal

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-daemon-cli/internal/adapter"
)

func TestCcOctoStatusRunning(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want bool
	}{
		{"running", "cc-channel-octo: running (pid 1306), logs at /tmp/gateway.log", true},
		{"stopped", "cc-channel-octo: stopped", false},
		{"not running", "cc-channel-octo: not running", false},
		{"empty", "", false},
		{"running with ansi", "\033[32mcc-channel-octo: running\033[0m (pid 1)", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ccOctoStatusRunning(tt.out); got != tt.want {
				t.Errorf("ccOctoStatusRunning(%q) = %v, want %v", tt.out, got, tt.want)
			}
		})
	}
}

// TestGatewayLockTryLockExcludesWatchdog asserts the core coordination contract:
// while a daemon lifecycle op holds the lock (via Acquire), the watchdog's
// TryLock fails (skip tick); once released, TryLock succeeds.
func TestGatewayLockTryLockExcludesWatchdog(t *testing.T) {
	g := adapter.NewGatewayLock()

	if err := g.Acquire(context.Background()); err != nil { // simulate daemon op in progress
		t.Fatalf("Acquire on free lock: %v", err)
	}
	if g.TryLock() {
		t.Fatal("TryLock should fail while the lock is held by a daemon op")
	}
	g.Unlock()

	if !g.TryLock() {
		t.Fatal("TryLock should succeed once the lock is free")
	}
	g.Unlock()
}

// TestGatewayLockAcquireTimeout asserts the deadlock fix: when the lock is held,
// a context-aware Acquire returns the ctx error instead of blocking forever.
func TestGatewayLockAcquireTimeout(t *testing.T) {
	g := adapter.NewGatewayLock()
	if !g.TryLock() { // hold it (stand-in for a wedged watchdog start)
		t.Fatal("TryLock on free lock should succeed")
	}
	defer g.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := g.Acquire(ctx); err == nil {
		t.Fatal("Acquire should fail while the lock is held and ctx expires")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Acquire blocked %v, expected to bail near ctx deadline", elapsed)
	}
}

// TestGatewayLockNilSafe asserts a nil lock degrades to no-coordination so tests
// and lock-less adapters can pass nil.
func TestGatewayLockNilSafe(t *testing.T) {
	var g *adapter.GatewayLock
	if err := g.Acquire(context.Background()); err != nil {
		t.Fatalf("nil GatewayLock.Acquire should be a no-op, got %v", err)
	}
	g.Unlock() // no panic
	if !g.TryLock() {
		t.Fatal("nil GatewayLock.TryLock should report success")
	}
	g.Unlock()
}

// TestGatewayLockSerializes asserts mutual exclusion under contention.
func TestGatewayLockSerializes(t *testing.T) {
	g := adapter.NewGatewayLock()
	var mu sync.Mutex
	inside := 0
	max := 0
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := g.Acquire(context.Background()); err != nil {
				return
			}
			defer g.Unlock()
			mu.Lock()
			inside++
			if inside > max {
				max = inside
			}
			mu.Unlock()
			mu.Lock()
			inside--
			mu.Unlock()
		}()
	}
	wg.Wait()
	if max != 1 {
		t.Fatalf("GatewayLock allowed %d concurrent holders, want 1", max)
	}
}
