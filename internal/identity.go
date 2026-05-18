package internal

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

func DataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".octo-daemon")
}

func daemonIDPath() string {
	return filepath.Join(DataDir(), "daemon.id")
}

func EnsureDaemonID() (string, error) {
	idPath := daemonIDPath()

	data, err := os.ReadFile(idPath)
	if err == nil {
		id := strings.TrimSpace(string(data))
		if id != "" {
			return id, nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(idPath), 0700); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}

	id := uuid.Must(uuid.NewV7()).String()
	if err := os.WriteFile(idPath, []byte(id+"\n"), 0600); err != nil {
		return "", fmt.Errorf("write daemon id: %w", err)
	}

	return id, nil
}

func LoadDaemonID() (string, error) {
	data, err := os.ReadFile(daemonIDPath())
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
