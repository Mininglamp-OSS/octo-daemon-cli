//go:build !darwin && !linux

package service

import (
	"fmt"
	"runtime"
)

// New returns an error on unsupported platforms (Windows in v1).
func New() (Service, error) {
	return nil, fmt.Errorf("service install not supported on %s", runtime.GOOS)
}
