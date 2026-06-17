package internal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Mininglamp-OSS/octo-daemon-cli/internal/adapter"
)

// testRegistry 返回一个只注册 openclaw+claude 的 registry(daemon 支持集)。
func testRegistry(t *testing.T) *adapter.Registry {
	t.Helper()
	reg := adapter.NewRegistry()
	for _, a := range []adapter.RuntimeAdapter{
		adapter.NewOpenclawAdapter(nil),
		adapter.NewClaudeAdapter(nil),
	} {
		if err := reg.Register(a); err != nil {
			t.Fatalf("register adapter: %v", err)
		}
	}
	return reg
}

// newTestDaemon 构造一个最小 Daemon:client 指向 srv,registry 为支持集。
func newTestDaemon(t *testing.T, srv *httptest.Server) *Daemon {
	t.Helper()
	return &Daemon{
		cfg:      Config{FleetURL: srv.URL, APIKey: "test-key"},
		client:   NewClient(srv.URL, "test-key", "0.0.0-test"),
		registry: testRegistry(t),
	}
}

func TestRefreshProviders_200ReplacesSnapshot(t *testing.T) {
	t.Cleanup(resetProvidersForTest)
	resetProvidersForTest()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"providers":[{"name":"claude","binary_name":"claude"}]}`))
	}))
	defer srv.Close()
	d := newTestDaemon(t, srv)

	if err := d.refreshProviders(context.Background()); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	got := currentProviders()
	if _, ok := got["claude"]; !ok || len(got) != 1 {
		t.Errorf("expected snapshot {claude}, got %v", got)
	}
}

func TestRefreshProviders_200EmptyClearsSnapshot(t *testing.T) {
	t.Cleanup(resetProvidersForTest)
	resetProvidersForTest() // 起点是 fallback {claude, openclaw}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"providers":[]}`))
	}))
	defer srv.Close()
	d := newTestDaemon(t, srv)

	if err := d.refreshProviders(context.Background()); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got := currentProviders(); len(got) != 0 {
		t.Errorf("200 empty must clear snapshot, got %v", got)
	}
}

func TestRefreshProviders_404KeepsSnapshot(t *testing.T) {
	t.Cleanup(resetProvidersForTest)
	resetProvidersForTest() // fallback {claude, openclaw}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	d := newTestDaemon(t, srv)

	if err := d.refreshProviders(context.Background()); err != nil {
		t.Fatalf("404 must not be terminal, got err: %v", err)
	}
	got := currentProviders()
	if _, ok := got["claude"]; !ok {
		t.Errorf("404 must keep last snapshot (fallback), got %v", got)
	}
	if _, ok := got["openclaw"]; !ok {
		t.Errorf("404 must keep last snapshot (fallback), got %v", got)
	}
}

func TestRefreshProviders_403Terminal(t *testing.T) {
	t.Cleanup(resetProvidersForTest)
	resetProvidersForTest()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	d := newTestDaemon(t, srv)
	// checkForbidden 需要 cancel,给一个可取消 ctx。
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	defer cancel()

	err := d.refreshProviders(ctx)
	if err == nil {
		t.Fatal("403 must return a terminal error")
	}
	if d.readExitErr() == nil {
		t.Error("403 must record an ExitError via checkForbidden")
	}
}

func TestRegister_403DoesNotCallRegisterEndpoint(t *testing.T) {
	t.Cleanup(resetProvidersForTest)
	resetProvidersForTest()
	var registerHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/daemon/runtime-providers":
			w.WriteHeader(http.StatusForbidden) // key 撤销
		case "/v1/daemon/register":
			registerHits.Add(1)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	d := newTestDaemon(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	defer cancel()

	if err := d.register(ctx); err == nil {
		t.Fatal("register must propagate 403 terminal error")
	}
	if registerHits.Load() != 0 {
		t.Errorf("register endpoint must NOT be called after 403 refresh, hits=%d", registerHits.Load())
	}
}
