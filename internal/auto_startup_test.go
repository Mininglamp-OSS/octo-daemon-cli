package internal

import (
	"sync"
	"testing"

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
// while a daemon lifecycle op holds the lock (blocking Lock), the watchdog's
// TryLock fails (skip tick); once released, TryLock succeeds.
func TestGatewayLockTryLockExcludesWatchdog(t *testing.T) {
	g := adapter.NewGatewayLock()

	g.Lock() // simulate daemon restart/upgrade in progress
	if g.TryLock() {
		t.Fatal("TryLock should fail while the lock is held by a daemon op")
	}
	g.Unlock()

	if !g.TryLock() {
		t.Fatal("TryLock should succeed once the lock is free")
	}
	g.Unlock()
}

// TestGatewayLockNilSafe asserts a nil lock degrades to no-coordination so tests
// and lock-less adapters can pass nil.
func TestGatewayLockNilSafe(t *testing.T) {
	var g *adapter.GatewayLock
	g.Lock()   // no panic
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
			g.Lock()
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
