package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Recover is also called by the fixed service launcher before starting a
// daemon. An active worker owns the OS lock, so its activation is never undone.
// restore is reserved for the launcher, before the target service is running.
func (m *Manager) Recover(restore bool) error {
	var recoveryErr error
	err := m.locked(func() error {
		data, err := os.ReadFile(filepath.Join(m.directory(), "status.json"))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		var s Status
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		_, journalErr := os.Stat(filepath.Join(m.directory(), "journal"))
		if !active(s.Phase) && errors.Is(journalErr, os.ErrNotExist) {
			return nil
		}
		// Start publishes its intent before the service manager launches Worker.
		// A short grace period prevents status polling from cancelling that handoff.
		if info, err := os.Stat(filepath.Join(m.directory(), "starting")); err == nil && time.Since(info.ModTime()) < 30*time.Second {
			return nil
		}
		journal, err := os.ReadFile(filepath.Join(m.directory(), "journal"))
		if err == nil {
			stage := strings.TrimSpace(string(journal))
			if filepath.Dir(stage) != m.directory() || !strings.HasPrefix(filepath.Base(stage), "apply.") {
				return fmt.Errorf("invalid update recovery journal")
			}
			old := filepath.Join(stage, "old-current")
			_, currentErr := os.Lstat(m.current(""))
			if _, oldErr := os.Lstat(old); oldErr == nil && (restore || errors.Is(currentErr, os.ErrNotExist)) {
				if currentErr == nil {
					if err := os.Rename(m.current(""), filepath.Join(stage, "interrupted-current")); err != nil {
						return err
					}
				}
				if err := os.Rename(old, m.current("")); err != nil {
					return err
				}
			}
			lock := filepath.Join(m.Prefix, m.Component, "install.lock")
			if owner, err := os.ReadFile(filepath.Join(lock, "update-owner")); err == nil && strings.TrimSpace(string(owner)) == stage {
				if err := os.Remove(filepath.Join(lock, "update-owner")); err != nil {
					return err
				}
				if err := os.Remove(lock); err != nil {
					return err
				}
			}
			if err := os.Remove(filepath.Join(m.directory(), "journal")); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		s.Phase, s.Error = "failed", "Update was interrupted. The installed version is shown above; prepare the update again to retry."
		lock := filepath.Join(m.Prefix, m.Component, "install.lock")
		if owner, err := os.ReadFile(filepath.Join(lock, "update-owner")); err == nil && strings.TrimSpace(string(owner)) == "prepare" {
			if err := os.Remove(filepath.Join(lock, "update-owner")); err != nil {
				return err
			}
			if err := os.Remove(lock); err != nil {
				return err
			}
		}
		if err := m.save(s); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(m.directory(), "draining")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
	// Lock contention means the independent worker is still responsible for it.
	if err != nil && !errors.Is(err, errUpdateBusy) {
		recoveryErr = err
	}
	return recoveryErr
}
