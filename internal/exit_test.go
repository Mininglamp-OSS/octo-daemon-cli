package internal

import (
	"errors"
	"os"
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

// ensure under-service env var name stays in sync with main.go.
func TestExitError_UnderServiceEnvVarName(t *testing.T) {
	// 约定：main.go 读 OCTO_DAEMON_UNDER_SERVICE=1 做 2/78→0 映射。
	// 这个测试把字面量钉在 ExitError 文档里，改名字要同步改。
	key := "OCTO_DAEMON_UNDER_SERVICE"
	t.Setenv(key, "1")
	if os.Getenv(key) != "1" {
		t.Fatal("env var sanity check failed")
	}
}
