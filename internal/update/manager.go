package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Release struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

type Status struct {
	Component string   `json:"component"`
	Version   string   `json:"version"`
	Commit    string   `json:"commit"`
	Custom    bool     `json:"custom"`
	Channel   string   `json:"channel"`
	Target    *Release `json:"target,omitempty"`
	CheckedAt string   `json:"checked_at,omitempty"`
	Phase     string   `json:"phase"`
	Error     string   `json:"error,omitempty"`
}

type Manager struct{ Prefix, Component string }

var errUpdateBusy = errors.New("another update operation is running")

func ForExecutable(executable, component string) (*Manager, error) {
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, err
	}
	for directory := filepath.Dir(resolved); directory != filepath.Dir(directory); directory = filepath.Dir(directory) {
		if filepath.Base(directory) == component {
			return New(filepath.Dir(directory), component)
		}
	}
	return nil, fmt.Errorf("not an installer-managed executable")
}

func (m *Manager) Draining() bool {
	_, err := os.Stat(filepath.Join(m.directory(), "draining"))
	return err == nil || !errors.Is(err, os.ErrNotExist)
}
func (m *Manager) BeginDrain() error {
	if err := os.MkdirAll(m.directory(), 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(m.directory(), "draining"), []byte("update\n"), 0600)
}

func (m *Manager) CancelWaiting() (Status, error) {
	s, err := m.Status()
	if err != nil {
		return s, err
	}
	if s.Phase != "waiting" {
		return s, fmt.Errorf("only an update waiting for idle can be cancelled")
	}
	return s, os.WriteFile(filepath.Join(m.directory(), "cancel"), []byte("cancel\n"), 0600)
}

func New(prefix, component string) (*Manager, error) {
	if component != "daemon" && component != "desktop" && component != "web" {
		return nil, fmt.Errorf("invalid update component")
	}
	if !filepath.IsAbs(prefix) {
		return nil, fmt.Errorf("installation prefix must be absolute")
	}
	m := &Manager{Prefix: filepath.Clean(prefix), Component: component}
	if err := m.Recover(false); err != nil {
		return nil, err
	}
	if _, err := os.Stat(m.current("INSTALLATION")); err != nil {
		return nil, fmt.Errorf("application updates require an installer-managed installation: %w", err)
	}
	return m, nil
}

func (m *Manager) current(name string) string {
	return filepath.Join(m.Prefix, m.Component, "current", name)
}
func (m *Manager) directory() string { return filepath.Join(m.Prefix, "updates", m.Component) }
func (m *Manager) repository() string {
	if m.Component == "daemon" {
		return "https://github.com/jayyao97/zotigo.git"
	}
	return "https://github.com/jayyao97/zotigo-ui.git"
}

func (m *Manager) locked(fn func() error) error {
	return m.withLock(false, fn)
}

func (m *Manager) withLock(wait bool, fn func() error) error {
	if err := os.MkdirAll(m.directory(), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(m.directory(), "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	flags := syscall.LOCK_EX
	if !wait {
		flags |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), flags); err != nil {
		return errUpdateBusy
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func atomicJSON(name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(name), ".update-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, name)
}

func (m *Manager) save(status Status) error {
	return atomicJSON(filepath.Join(m.directory(), "status.json"), status)
}

func (m *Manager) Status() (Status, error) {
	if err := m.Recover(false); err != nil {
		return Status{}, err
	}
	s := Status{Component: m.Component, Phase: "idle", Channel: "stable"}
	data, err := os.ReadFile(m.current("INSTALLATION"))
	if err != nil {
		return s, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, _ := strings.Cut(line, "=")
		switch key {
		case "version":
			s.Version = value
		case "commit":
			s.Commit = value
		case "dirty":
			s.Custom = value != "false"
		}
	}
	if v, err := parseVersion(s.Version); err == nil && v.dev {
		s.Channel = "development"
	}
	data, err = os.ReadFile(filepath.Join(m.directory(), "status.json"))
	if err == nil {
		var saved Status
		if err := json.Unmarshal(data, &saved); err != nil {
			return s, fmt.Errorf("read update state: %w", err)
		}
		if !validChannel(saved.Channel) {
			return s, fmt.Errorf("invalid stored channel")
		}
		s.Channel, s.Target, s.CheckedAt, s.Phase, s.Error = saved.Channel, saved.Target, saved.CheckedAt, saved.Phase, saved.Error
	} else if !errors.Is(err, os.ErrNotExist) {
		return s, err
	}
	if s.Phase == "waiting" {
		if progress, err := os.ReadFile(filepath.Join(m.directory(), "progress")); err == nil {
			phase := strings.TrimSpace(string(progress))
			if phase == "applying" || phase == "verifying" {
				s.Phase = phase
			}
		}
	}
	return s, nil
}

func (m *Manager) SetChannel(channel string) (Status, error) {
	var s Status
	err := m.locked(func() error {
		if !validChannel(channel) {
			return fmt.Errorf("invalid update channel")
		}
		var err error
		s, err = m.Status()
		if err != nil {
			return err
		}
		if active(s.Phase) {
			return fmt.Errorf("finish the current update before changing channel")
		}
		s.Channel, s.Target, s.CheckedAt, s.Phase, s.Error = channel, nil, "", "idle", ""
		return m.save(s)
	})
	return s, err
}

func active(phase string) bool {
	return phase == "preparing" || phase == "waiting" || phase == "applying" || phase == "verifying"
}

func (m *Manager) Check(ctx context.Context) (Status, error) {
	var s Status
	err := m.locked(func() error {
		var err error
		s, err = m.Status()
		if err != nil {
			return err
		}
		if active(s.Phase) || s.Phase == "ready" {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "git", "ls-remote", "--tags", m.repository()).Output()
		if err != nil {
			return fmt.Errorf("check published releases: %w", err)
		}
		tag, commit, err := SelectRelease(string(out), s.Channel)
		if err != nil {
			return err
		}
		current, err := parseVersion(s.Version)
		if err != nil {
			s.Custom = true
		}
		target, _ := parseVersion(tag)
		s.Target = nil
		if s.Custom || compare(target, current) > 0 || (s.Channel == "stable" && current.dev) {
			s.Target = &Release{Version: strings.TrimPrefix(tag, "v"), Commit: commit}
		}
		s.CheckedAt, s.Error = time.Now().UTC().Format(time.RFC3339), ""
		s.Phase = "idle"
		return m.save(s)
	})
	return s, err
}
