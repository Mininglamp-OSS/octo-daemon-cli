package internal

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

func DataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".octo-daemon")
}

// spaceIDPattern restricts space_id to a safe charset so it can be used as a
// directory name without path-traversal risk.
var spaceIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidateSpaceID rejects empty, oversized, or path-traversal-prone values.
func ValidateSpaceID(spaceID string) error {
	if spaceID == "" {
		return fmt.Errorf("space_id is required")
	}
	if len(spaceID) > 128 {
		return fmt.Errorf("space_id too long (max 128)")
	}
	if spaceID == "." || spaceID == ".." || strings.Contains(spaceID, "/") || strings.Contains(spaceID, `\`) {
		return fmt.Errorf("space_id %q must not contain path separators", spaceID)
	}
	if !spaceIDPattern.MatchString(spaceID) {
		return fmt.Errorf("space_id %q has invalid characters (allowed: letters, digits, . _ -)", spaceID)
	}
	return nil
}

// SpaceDir returns the per-space state directory ~/.octo-daemon/<space_id>.
func SpaceDir(spaceID string) string {
	return filepath.Join(DataDir(), spaceID)
}

func daemonIDPath(spaceID string) string {
	return filepath.Join(SpaceDir(spaceID), "daemon.id")
}

// EnsureDaemonID returns the stable daemon identity for a space, generating and
// persisting a UUIDv7 on first use.
func EnsureDaemonID(spaceID string) (string, error) {
	if err := ValidateSpaceID(spaceID); err != nil {
		return "", err
	}
	idPath := daemonIDPath(spaceID)

	if data, err := os.ReadFile(idPath); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id, nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(idPath), 0700); err != nil {
		return "", fmt.Errorf("create space dir: %w", err)
	}

	id := uuid.Must(uuid.NewV7()).String()
	if err := os.WriteFile(idPath, []byte(id+"\n"), 0600); err != nil {
		return "", fmt.Errorf("write daemon id: %w", err)
	}
	return id, nil
}

// LoadDaemonID reads a space's persisted daemon id without creating it.
func LoadDaemonID(spaceID string) (string, error) {
	data, err := os.ReadFile(daemonIDPath(spaceID))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func deviceIDPath() string {
	return filepath.Join(DataDir(), "device.id")
}

// EnsureDeviceID returns the stable machine-level device fingerprint, generating
// and persisting a 32-char hex id (UUIDv7 with dashes stripped) on first use.
// Unlike daemon.id this is per-machine, not per-space — multiple daemons on the
// same device share one device.id.
func EnsureDeviceID() (string, error) {
	idPath := deviceIDPath()

	if data, err := os.ReadFile(idPath); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id, nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(idPath), 0700); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}

	id := strings.ReplaceAll(uuid.Must(uuid.NewV7()).String(), "-", "")
	if err := os.WriteFile(idPath, []byte(id+"\n"), 0600); err != nil {
		return "", fmt.Errorf("write device id: %w", err)
	}
	return id, nil
}
