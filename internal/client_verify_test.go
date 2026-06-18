package internal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerify_ReturnsSpaceFromEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/runtimes/verify" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer uk_test" {
			t.Errorf("missing/wrong auth header: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"space_id":"sp_42","owner_uid":"u_7"}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "uk_test", "0.0.0-test")
	got, err := c.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.SpaceID != "sp_42" || got.OwnerUID != "u_7" {
		t.Fatalf("unexpected verify resp: %+v", got)
	}
}

func TestVerify_AuthFailureErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"AUTH_REQUIRED","message":"invalid api key"}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "uk_bad", "0.0.0-test")
	if _, err := c.Verify(context.Background()); err == nil {
		t.Fatal("expected error on 401")
	}
}
