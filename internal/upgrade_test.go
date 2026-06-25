package internal

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestShouldSkipDaemonUpgrade(t *testing.T) {
	tests := []struct {
		name      string
		installed string
		target    string
		want      bool
	}{
		{"already at target", "0.0.5", "0.0.5", true},
		{"empty target with version installed", "0.0.5", "", true},
		{"target newer than installed", "0.0.4", "0.0.5", false},
		{"nothing installed, empty target", "", "", false},
		{"nothing installed, target set", "", "0.0.5", false},
		{"v-prefixed target matches installed", "0.0.5", "v0.0.5", true},
		{"installed newer than target (no downgrade)", "0.0.6", "0.0.5", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldSkipDaemonUpgrade(tt.installed, tt.target); got != tt.want {
				t.Errorf("shouldSkipDaemonUpgrade(%q, %q) = %v, want %v", tt.installed, tt.target, got, tt.want)
			}
		})
	}
}

// TestHandleDaemonUpgrade_NpmProbeFailureReportsFailed locks the regression where
// a pre-install probe failure (npm missing / `npm ls` error) silently returned
// nil instead of reporting a terminal "failed". With no npm on PATH the probe
// fails; the handler must POST status=failed so the fleet task closes instead of
// lingering until sweeper timeout (covers k8s/image deployments without npm).
func TestHandleDaemonUpgrade_NpmProbeFailureReportsFailed(t *testing.T) {
	t.Setenv("PATH", "") // npm unfindable → installedDaemonVersion() errors

	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	d := &Daemon{client: NewClient(srv.URL, "tok", "0.0.4")}
	_ = d.handleDaemonUpgrade(context.Background(), &PendingUpgrade{TaskID: "t1", TargetVersion: "0.0.5"})

	if !strings.Contains(gotBody, `"failed"`) {
		t.Fatalf("npm-probe-failure daemon upgrade must report status=failed, got request body %q", gotBody)
	}
}
