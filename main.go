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
		// under-service 模式下把"别重启"语义的 code 压成 0，避免 launchd/systemd
		// 循环拉起。详见 plan §二.2.3 / §二.2.4。
		if os.Getenv("OCTO_DAEMON_UNDER_SERVICE") == "1" && (ee.Code == 2 || ee.Code == 78) {
			os.Exit(0)
		}
		os.Exit(ee.Code)
	}

	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
