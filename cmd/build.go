package cmd

// Build metadata. Set via -ldflags at release time.
//
//	go build -ldflags "-X github.com/Mininglamp-OSS/octo-daemon-cli/main.Version=v0.2.0 \
//	                   -X github.com/Mininglamp-OSS/octo-daemon-cli/main.Commit=<sha> \
//	                   -X github.com/Mininglamp-OSS/octo-daemon-cli/main.BuildDate=<date>"
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

// SetVersionInfo injects build metadata from main at startup.
func SetVersionInfo(v, c, d string) {
	version = v
	commit = c
	buildDate = d
}
