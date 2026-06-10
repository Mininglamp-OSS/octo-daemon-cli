package adapter

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// CLIRunner abstracts spawning a CLI subprocess so adapters can be unit-tested
// without a real binary on PATH. Run executes one command and returns its
// combined stdout+stderr. stdin, when non-nil, is fed to the process.
type CLIRunner interface {
	Run(ctx context.Context, name string, args []string, stdin []byte) ([]byte, error)
}

// ExecRunner is the production CLIRunner backed by os/exec. It mirrors the
// original direct-exec behaviour: CombinedOutput, stdin via a byte reader.
type ExecRunner struct{}

// Run implements CLIRunner using exec.CommandContext + CombinedOutput.
func (ExecRunner) Run(ctx context.Context, name string, args []string, stdin []byte) ([]byte, error) {
	c := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	}
	out, err := c.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}
