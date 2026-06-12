package service

import "github.com/Mininglamp-OSS/octo-daemon-cli/internal"

// internalDataDir is a thin wrapper to keep Platform files independent of the
// internal package's exact API surface. Currently just delegates.
//
//nolint:unused // called from darwin.go; CI lints on linux, which build-excludes that file.
func internalDataDir() string { return internal.DataDir() }
