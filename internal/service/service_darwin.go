//go:build darwin

package service

// New returns the launchd-backed Service on macOS.
func New() (Service, error) {
	return &launchdService{}, nil
}
