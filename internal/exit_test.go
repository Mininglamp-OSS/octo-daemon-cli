package internal

import (
	"errors"
	"testing"
)

func TestExitError_Error(t *testing.T) {
	if got := (&ExitError{Code: 75, Message: "upgrade respawn"}).Error(); got != "upgrade respawn" {
		t.Errorf("Error() = %q, want upgrade respawn", got)
	}
	if got := (&ExitError{Code: 2}).Error(); got != "exit code 2" {
		t.Errorf("Error() without message = %q, want exit code 2", got)
	}
}

func TestExitError_ErrorsAs(t *testing.T) {
	var ee *ExitError
	err := error(&ExitError{Code: 78, Message: "forbidden"})
	if !errors.As(err, &ee) {
		t.Fatal("errors.As should match ExitError")
	}
	if ee.Code != 78 {
		t.Errorf("ee.Code = %d, want 78", ee.Code)
	}
}
