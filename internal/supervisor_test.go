package internal

import (
	"context"
	"errors"
	"testing"
)

func TestSupervisorRun_AllInvalidProfilesFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// The only profile has an invalid space_id, so EnsureDaemonID skips it and
	// no runner starts. Run must return a non-nil ExitError (not a clean nil),
	// otherwise a zero-runner start looks successful.
	sup, err := NewSupervisor([]Config{
		{SpaceID: "../bad", APIKey: "k", FleetURL: "http://f", ServerURL: "http://s"},
	})
	if err != nil {
		t.Fatal(err)
	}

	runErr := sup.Run(context.Background())
	if runErr == nil {
		t.Fatal("Run must fail loudly when no runner started, got nil")
	}
	var ee *ExitError
	if !errors.As(runErr, &ee) {
		t.Fatalf("expected *ExitError, got %T: %v", runErr, runErr)
	}
	if ee.Code != 2 {
		t.Fatalf("expected exit code 2, got %d", ee.Code)
	}
}
