package service

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-daemon-cli/internal"
)

func TestPaths_AllUnderDataDir(t *testing.T) {
	dataDir := internal.DataDir()
	for _, p := range []string{
		ServiceEnvDir(),
		LogDir(),
		WrapperScriptPath(),
		EnvFilePath(),
		StdoutLogPath(),
		StderrLogPath(),
	} {
		if !strings.HasPrefix(p, dataDir+string(filepath.Separator)) {
			t.Errorf("path %q not under DataDir %q", p, dataDir)
		}
	}
}

func TestServiceLabel_Stable(t *testing.T) {
	if ServiceLabel != "ai.octo.daemon" {
		t.Errorf("ServiceLabel drifted: %q", ServiceLabel)
	}
}
