package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/Mininglamp-OSS/octo-daemon-cli/cmd"
	"github.com/Mininglamp-OSS/octo-daemon-cli/internal"
)

var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

func main() {
	cmd.SetVersionInfo(Version, Commit, BuildDate)
	err := cmd.Execute()
	if err == nil {
		return
	}

	// 错误带 exit code 语义时走专用映射；否则默认 exit 1。
	var ee *internal.ExitError
	if errors.As(err, &ee) {
		if ee.Message != "" {
			fmt.Fprintln(os.Stderr, ee.Message)
		}
		os.Exit(ee.Code)
	}

	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
