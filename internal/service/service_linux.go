//go:build linux

package service

// New returns the systemd --user backed Service on Linux.
func New() (Service, error) {
	return &systemdUserService{}, nil
}
