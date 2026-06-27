package internal

import (
	"regexp"
	"strings"
	"testing"
)

// TestDeregisterUsesSeparateContexts locks in the fix that daemon-level
// deregister and runtime deregister must NOT share one shutdown context.
//
// Why source-grep: Daemon.client is a concrete struct (not an interface), so a
// unit test can't make DaemonDeregister hang to prove the runtime deregister
// still runs. The risk: a single 5s ctx shared by both means a slow/stuck
// DaemonDeregister burns the whole window, leaving the runtime Deregister with
// an already-expired ctx — i.e. a best-effort daemon-down starving the runtime
// down. This asserts they use distinct contexts.
func TestDeregisterUsesSeparateContexts(t *testing.T) {
	src := readSource(t, "daemon.go")
	re := regexp.MustCompile(`func \(d \*Daemon\) deregister\(\) \{`)
	loc := re.FindStringIndex(src)
	if loc == nil {
		t.Fatal("deregister() not found in daemon.go")
	}
	// Take the function body slice (from match to the next top-level closing
	// brace at column 0) — good enough for substring assertions here.
	body := src[loc[0]:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}

	// DaemonDeregister must run on its own context, cancelled before the runtime
	// deregister's context is created — i.e. two distinct WithTimeout calls.
	timeouts := strings.Count(body, "context.WithTimeout(")
	if timeouts < 2 {
		t.Errorf("deregister() must use TWO independent contexts (daemon-down vs runtime-down), found %d context.WithTimeout calls", timeouts)
	}

	// The daemon-down context must be created (and ideally cancelled) before the
	// runtime Deregister call, so a stuck DaemonDeregister can't expire the
	// runtime context.
	idxDaemonDown := strings.Index(body, "DaemonDeregister(")
	idxRuntimeDown := strings.Index(body, "Deregister(ctx, ids)")
	if idxDaemonDown < 0 {
		t.Fatal("deregister() must call DaemonDeregister")
	}
	if idxRuntimeDown < 0 {
		t.Fatal("deregister() must call Deregister(ctx, ids) for runtimes")
	}
	if idxDaemonDown >= idxRuntimeDown {
		t.Error("DaemonDeregister must be invoked before the runtime Deregister")
	}
}
