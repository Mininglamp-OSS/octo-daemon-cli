package internal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDaemonDeregister_SendsCorrectPayload(t *testing.T) {
	var receivedBody DaemonDeregisterRequest
	var gotAuth, gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		gotAuth = r.Header.Get("Authorization")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &receivedBody); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-api-key", "0.0.0-test")
	if err := c.DaemonDeregister(context.Background(), "daemon-abc123"); err != nil {
		t.Fatalf("DaemonDeregister: %v", err)
	}

	if gotPath != "/v1/daemons/_deregister" {
		t.Errorf("expected path /v1/daemons/_deregister, got %q", gotPath)
	}
	if gotAuth != "Bearer test-api-key" {
		t.Errorf("expected Bearer test-api-key, got %q", gotAuth)
	}
	if receivedBody.DaemonID != "daemon-abc123" {
		t.Errorf("expected daemon_id daemon-abc123, got %q", receivedBody.DaemonID)
	}
}

func TestDaemonDeregister_BestEffortOn5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal error"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-api-key", "0.0.0-test")
	err := c.DaemonDeregister(context.Background(), "daemon-abc123")
	if err == nil {
		t.Fatal("expected error on 5xx response")
	}
	// best-effort: returns error but does not panic; shutdown proceeds regardless
}
