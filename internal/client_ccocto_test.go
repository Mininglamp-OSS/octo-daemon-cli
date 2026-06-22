package internal

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchCcOctoConfig_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/upgrades/task_1/cc-octo-config" || r.URL.Query().Get("runtime_id") != "7" {
			t.Errorf("unexpected request: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "Bearer k1" {
			t.Errorf("missing bearer auth: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"gateway_url":"https://gw","api_key":"sk-1"}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k1", "dev")
	cfg, err := c.FetchCcOctoConfig(context.Background(), 7, "task_1")
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil || cfg.GatewayURL != "https://gw" || cfg.APIKey != "sk-1" {
		t.Fatalf("got %+v; want gw+key", cfg)
	}
}

func TestFetchCcOctoConfig_404IsNilNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"code":"NOT_FOUND"}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k1", "dev")
	cfg, err := c.FetchCcOctoConfig(context.Background(), 7, "task_x")
	if err != nil || cfg != nil {
		t.Fatalf("404 should map to (nil,nil); got cfg=%+v err=%v", cfg, err)
	}
}

func TestFetchCcOctoConfig_ForbiddenIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k1", "dev")
	if _, err := c.FetchCcOctoConfig(context.Background(), 7, "task_x"); err == nil {
		t.Fatal("403 should be an error, not silently nil")
	}
}

// 409 = install task but secret missing/expired (or terminal task). MUST be an
// error so the install reports failed — never silently fall back to a no-key
// plain upgrade. Only 404 (plain upgrade, no secret expected) maps to (nil,nil).
func TestFetchCcOctoConfig_ConflictIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k1", "dev")
	cfg, err := c.FetchCcOctoConfig(context.Background(), 7, "task_x")
	if err == nil || cfg != nil {
		t.Fatalf("409 must be an error (install secret gone), got cfg=%+v err=%v", cfg, err)
	}
	if !errors.Is(err, ErrCcOctoConfigUnavailable) {
		t.Fatalf("409 should map to ErrCcOctoConfigUnavailable, got %v", err)
	}
}

// 500 = transient server error. Should return an error that is NOT the sentinel.
func TestFetchCcOctoConfig_500IsTransientError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"internal"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k1", "dev")
	_, err := c.FetchCcOctoConfig(context.Background(), 7, "task_x")
	if err == nil {
		t.Fatal("500 should be an error")
	}
	if errors.Is(err, ErrCcOctoConfigUnavailable) {
		t.Fatalf("500 should NOT map to ErrCcOctoConfigUnavailable, got %v", err)
	}
}
