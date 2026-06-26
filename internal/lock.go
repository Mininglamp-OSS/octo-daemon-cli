package internal

import (
	"fmt"
	"os"
	"path/filepath"
)

func LockFilePath() string {
	return filepath.Join(DataDir(), "daemon.lock")
}

func pidFilePath() string {
	return filepath.Join(DataDir(), "daemon.pid")
}

// TryLock attempts to acquire an exclusive lock on the lock file.
// Returns the file handle (caller must keep it open) or an error if locked.
func TryLock() (*os.File, error) {
	lockPath := LockFilePath()
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	if err := lockFile(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another daemon is already running. Stop the existing 'octo-daemon run' process or the pm2 service first")
	}

	// Write PID to a separate file (readable even while lock is held)
	WritePID()

	return f, nil
}

// WritePID writes current PID to daemon.pid (separate from lock file).
func WritePID() {
	_ = os.WriteFile(pidFilePath(), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0600)
}

// RemovePID removes the PID file.
func RemovePID() {
	_ = os.Remove(pidFilePath())
}

// IsLocked checks if the daemon lock is held (non-destructive).
func IsLocked() bool {
	lockPath := LockFilePath()
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	if err := lockFile(f); err != nil {
		return true
	}
	unlockFile(f)
	return false
}

// ReadLockPID reads the PID from daemon.pid file.
func ReadLockPID() (int, error) {
	data, err := os.ReadFile(pidFilePath())
	if err != nil {
		return 0, err
	}
	var pid int
	_, err = fmt.Sscanf(string(data), "%d", &pid)
	return pid, err
}
