package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// StateDirEnv overrides where Zotigo keeps runtime state: sessions, the
// Workspace catalog and Project/Workspace directories, scratch directories,
// Channels and logs. Configuration (config.yaml, hooks) and user skills are
// not state and are always read from the home directory.
//
// zotigo --no-daemon --state-dir sets it so the in-process service and its
// session workers (child processes) agree on one isolated root.
const StateDirEnv = "ZOTIGO_STATE_DIR"

// DefaultStateDir returns ~/.zotigo, the state root shared by zotigod and
// every client that talks to it.
func DefaultStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ConfigDirName), nil
}

// StateDir returns the state root for this process: $ZOTIGO_STATE_DIR when
// set, otherwise ~/.zotigo.
func StateDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv(StateDirEnv)); dir != "" {
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("%s must be an absolute path: %q", StateDirEnv, dir)
		}
		return filepath.Clean(dir), nil
	}
	return DefaultStateDir()
}
