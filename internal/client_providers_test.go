package internal

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListProviders_ParsesActiveList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/providers" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing/wrong auth header: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"providers":[{"name":"claude","display_name":"Claude","binary_name":"claude","upgrade_timeout_sec":600},{"name":"openclaw","display_name":"OpenClaw","binary_name":"openclaw","upgrade_timeout_sec":720}]}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "0.0.0-test")
	got, err := c.ListProviders(context.Background())
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 providers, got %d: %v", len(got), got)
	}
	if got[0].Name == "" || got[0].BinaryName == "" {
		t.Errorf("unexpected first provider: %+v", got[0])
	}
}

func TestListProviders_OldFleet404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "0.0.0-test")
	_, err := c.ListProviders(context.Background())
	if err == nil {
		t.Fatal("expected error on 404 (old fleet without endpoint)")
	}
}

func TestListProviders_403ReturnsForbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("api key revoked"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "0.0.0-test")
	_, err := c.ListProviders(context.Background())
	var fe *ForbiddenError
	if !errors.As(err, &fe) {
		t.Fatalf("expected *ForbiddenError on 403, got %T: %v", err, err)
	}
}
