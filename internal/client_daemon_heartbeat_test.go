package internal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDaemonHeartbeat_SendsCorrectPayload(t *testing.T) {
	var receivedBody DaemonHeartbeatRequest
	var gotAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/daemons/heartbeat" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &receivedBody); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-api-key", "0.0.0-test")
	err := c.DaemonHeartbeat(context.Background(), DaemonHeartbeatRequest{
		DaemonID:          "daemon-abc123",
		DeviceUUID:        "device-xyz789",
		HeartbeatIntervalMs: 5000,
	})
	if err != nil {
		t.Fatalf("DaemonHeartbeat: %v", err)
	}

	if gotAuth != "Bearer test-api-key" {
		t.Errorf("expected Bearer test-api-key, got %q", gotAuth)
	}
	if receivedBody.DaemonID != "daemon-abc123" {
		t.Errorf("expected daemon_id daemon-abc123, got %q", receivedBody.DaemonID)
	}
	if receivedBody.DeviceUUID != "device-xyz789" {
		t.Errorf("expected device_uuid device-xyz789, got %q", receivedBody.DeviceUUID)
	}
	if receivedBody.HeartbeatIntervalMs != 5000 {
		t.Errorf("expected heartbeat_interval_ms 5000, got %d", receivedBody.HeartbeatIntervalMs)
	}
}

func TestDaemonHeartbeat_BestEffortOn5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal error"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-api-key", "0.0.0-test")
	err := c.DaemonHeartbeat(context.Background(), DaemonHeartbeatRequest{
		DaemonID:          "daemon-abc123",
		DeviceUUID:        "device-xyz789",
		HeartbeatIntervalMs: 5000,
	})
	if err == nil {
		t.Fatal("expected error on 5xx response")
	}
	// best-effort: returns error but does not panic
}

func TestDaemonHeartbeat_NetworkError(t *testing.T) {
	// Point to a non-routable address to trigger a transport-level error
	c := NewClient("http://127.0.0.1:1", "test-api-key", "0.0.0-test")
	c.httpClient.Timeout = 0 // use default timeout for faster failure
	err := c.DaemonHeartbeat(context.Background(), DaemonHeartbeatRequest{
		DaemonID:          "daemon-abc123",
		DeviceUUID:        "device-xyz789",
		HeartbeatIntervalMs: 5000,
	})
	if err == nil {
		t.Fatal("expected error on network failure")
	}
	// best-effort: returns error but does not panic
}
