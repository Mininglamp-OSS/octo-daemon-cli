package internal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestSendHeartbeats_OfflineRuntimeStillHeartbeats is the #59 regression guard.
//
// A daemon whose runtime is detected offline (e.g. cc-channel-octo / openclaw
// gateway not running) must still send that runtime's heartbeat: daemon
// liveness is not the same thing as runtime readiness. Before the fix, offline
// providers were skipped entirely, so a machine with no ready runtime stopped
// heartbeating and fleet's markStaleOffline flapped the whole device offline
// after 3× interval. The detected status must instead ride along in the
// runtime_status field so fleet can report readiness without losing liveness.
func TestSendHeartbeats_OfflineRuntimeStillHeartbeats(t *testing.T) {
	type hit struct {
		runtimeID string
		status    string
	}
	var (
		mu   sync.Mutex
		hits []hit
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// path: /v1/runtimes/{id}/heartbeat
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 4 || parts[0] != "v1" || parts[1] != "runtimes" || parts[3] != "heartbeat" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req HeartbeatRequest
		if len(body) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("bad heartbeat body %q: %v", body, err)
			}
		}
		mu.Lock()
		hits = append(hits, hit{runtimeID: parts[2], status: req.RuntimeStatus})
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	d := &Daemon{
		cfg:    Config{FleetURL: srv.URL, APIKey: "test-key"},
		client: NewClient(srv.URL, "test-key", "0.0.0-test"),
		lastRuntimes: []RuntimeInfo{
			{Provider: "openclaw", Status: "online"},
			{Provider: "claude", Status: "offline"},
		},
		registeredRuntimes: []RegisteredRuntime{
			{ID: 1, Provider: "openclaw"},
			{ID: 2, Provider: "claude"},
		},
	}

	d.sendHeartbeats(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 2 {
		t.Fatalf("expected heartbeats for both runtimes (incl. the offline one), got %d: %+v", len(hits), hits)
	}
	gotStatus := map[string]string{}
	for _, h := range hits {
		gotStatus[h.runtimeID] = h.status
	}
	if gotStatus["1"] != "online" {
		t.Errorf("runtime 1 (openclaw): want runtime_status=online, got %q", gotStatus["1"])
	}
	if gotStatus["2"] != "offline" {
		t.Errorf("runtime 2 (claude, offline): want a heartbeat with runtime_status=offline, got %q", gotStatus["2"])
	}
}

// TestHeartbeatRequest_OmitsEmptyStatus pins the backward-compat contract: when
// no status is known the field is omitted, so the payload stays the empty
// object older fleet builds expect.
func TestHeartbeatRequest_OmitsEmptyStatus(t *testing.T) {
	data, err := json.Marshal(HeartbeatRequest{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(data) != "{}" {
		t.Errorf("empty HeartbeatRequest should marshal to {}, got %s", data)
	}
}
