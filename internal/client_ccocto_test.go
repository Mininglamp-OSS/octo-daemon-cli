package internal

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchCcOctoConfig_404IsNilNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"NOT_FOUND"}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k1", "dev")
	cfg, err := c.FetchCcOctoConfig(context.Background(), 7, "task_x")
	if err != nil || cfg != nil {
		t.Fatalf("404 should map to (nil,nil); got cfg=%+v err=%v", cfg, err)
	}
}

// 409 = install task but secret missing/expired (or terminal task). MUST be an
// error so the install reports failed — never silently fall back to a no-key
// plain upgrade. Only 404 (plain upgrade, no secret expected) maps to (nil,nil).
func TestFetchCcOctoConfig_ConflictIsMissingError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k1", "dev")
	cfg, err := c.FetchCcOctoConfig(context.Background(), 7, "task_x")
	if err == nil || cfg != nil {
		t.Fatalf("409 must be an error (install secret gone), got cfg=%+v err=%v", cfg, err)
	}
	if !errors.Is(err, ErrCcOctoConfigMissing) {
		t.Fatalf("409 should map to ErrCcOctoConfigMissing, got %v", err)
	}
}

// 500 = transient server error. Should return an error that is NOT the sentinel.
func TestFetchCcOctoConfig_500IsTransientError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k1", "dev")
	_, err := c.FetchCcOctoConfig(context.Background(), 7, "task_x")
	if err == nil {
		t.Fatal("500 should be an error")
	}
	if errors.Is(err, ErrCcOctoConfigMissing) || errors.Is(err, ErrCcOctoConfigStale) || errors.Is(err, ErrCcOctoConfigPermanent) {
		t.Fatalf("500 should NOT map to any sentinel, got %v", err)
	}
}

// 410 Gone = task already terminal (stale replay). Must map to ErrCcOctoConfigStale.
func TestFetchCcOctoConfig_StaleIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"error":{"code":"TASK_TERMINAL"}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k1", "dev")
	cfg, err := c.FetchCcOctoConfig(context.Background(), 7, "task_x")
	if err == nil || cfg != nil {
		t.Fatalf("410 must be an error (stale replay), got cfg=%+v err=%v", cfg, err)
	}
	if !errors.Is(err, ErrCcOctoConfigStale) {
		t.Fatalf("410 should map to ErrCcOctoConfigStale, got %v", err)
	}
}

// 403 Forbidden = permanent failure (ownership mismatch). Must map to ErrCcOctoConfigPermanent.
func TestFetchCcOctoConfig_ForbiddenIsPermanentError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"FORBIDDEN"}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k1", "dev")
	cfg, err := c.FetchCcOctoConfig(context.Background(), 7, "task_x")
	if err == nil || cfg != nil {
		t.Fatalf("403 must be an error (permanent), got cfg=%+v err=%v", cfg, err)
	}
	if !errors.Is(err, ErrCcOctoConfigPermanent) {
		t.Fatalf("403 should map to ErrCcOctoConfigPermanent, got %v", err)
	}
	// Also verify it's NOT Stale or Missing
	if errors.Is(err, ErrCcOctoConfigStale) || errors.Is(err, ErrCcOctoConfigMissing) {
		t.Fatalf("403 should NOT map to Stale or Missing, got %v", err)
	}
}

// 200 OK with valid payload returns config.
func TestFetchCcOctoConfig_ValidPayloadReturnsConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/upgrades/task_1/cc-octo-config" || r.URL.Query().Get("runtime_id") != "7" {
			t.Errorf("unexpected request: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "Bearer k1" {
			t.Errorf("missing bearer auth: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"gateway_url":"https://gw.example.com","api_key":"sk-test-123"}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k1", "dev")
	cfg, err := c.FetchCcOctoConfig(context.Background(), 7, "task_1")
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil || cfg.GatewayURL != "https://gw.example.com" || cfg.APIKey != "sk-test-123" {
		t.Fatalf("got %+v; want gw+key", cfg)
	}
}
