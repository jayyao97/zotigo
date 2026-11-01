package update

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Manifest struct {
	Version  string `json:"version"`
	Protocol int    `json:"update_protocol"`
	API      int    `json:"api_epoch"`
	Data     int    `json:"data_epoch"`
}

func readManifest(name string) (Manifest, error) {
	var m Manifest
	data, err := os.ReadFile(name)
	if err != nil {
		return m, err
	}
	if err = json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	if _, err = parseVersion(m.Version); err != nil {
		return m, err
	}
	if m.Protocol != 1 || m.API < 1 || m.Data < 1 {
		return m, fmt.Errorf("unsupported release manifest")
	}
	return m, nil
}

// Start moves the worker out of the daemon service's control group. A detached
// child alone is insufficient: systemd would kill it when stopping the daemon.
func (m *Manager) Start(action string) (Status, error) {
	var s Status
	err := m.locked(func() error {
		var err error
		s, err = m.Status()
		if err != nil {
			return err
		}
		if active(s.Phase) {
			return fmt.Errorf("an update is already running")
		}
		if action != "prepare" && action != "apply" {
			return fmt.Errorf("invalid update action")
		}
		if action == "prepare" && (s.Target == nil || s.Phase == "ready") {
			return fmt.Errorf("check for an update first")
		}
		if action == "apply" && s.Phase != "ready" {
			return fmt.Errorf("prepare the update first")
		}
		if _, err := readManifest(m.current("UPDATE.json")); err != nil {
			return fmt.Errorf("install an update-capable release first: %w", err)
		}
		executable := m.current("zotigo-update")
		data, err := os.ReadFile(executable)
		if err != nil {
			return err
		}
		runner := filepath.Join(m.directory(), "runner")
		// The preceding worker may have released its lock but still be exiting.
		// Replace the directory entry rather than overwrite a live executable.
		if err := os.WriteFile(runner+".new", data, 0700); err != nil {
			return err
		}
		if err := os.Rename(runner+".new", runner); err != nil {
			return err
		}
		s.Phase, s.Error = "preparing", ""
		if err := os.Remove(filepath.Join(m.directory(), "progress")); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Remove(filepath.Join(m.directory(), "cancel")); err != nil && !os.IsNotExist(err) {
			return err
		}
		if action == "apply" {
			s.Phase = "waiting"
		}
		if err := m.save(s); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(m.directory(), "starting"), []byte("starting\n"), 0600); err != nil {
			return err
		}
		args := []string{runner, "worker", "--prefix", m.Prefix, "--component", m.Component, "--action", action}
		if err = launchWorker(args, m.directory()); err != nil {
			s.Phase, s.Error = "failed", err.Error()
			_ = m.save(s)
			return err
		}
		return nil
	})
	return s, err
}

func launchWorker(args []string, directory string) error {
	unit := "zotigo-update-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if runtime.GOOS == "linux" {
		command := append([]string{"--user", "--collect", "--unit=" + unit, "--property=Type=exec"}, args...)
		if out, err := exec.Command("systemd-run", command...).CombinedOutput(); err != nil {
			return fmt.Errorf("start update service: %s: %w", out, err)
		}
		return nil
	}
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("unsupported update platform")
	}
	var body strings.Builder
	body.WriteString(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>Label</key><string>` + unit + `</string><key>ProgramArguments</key><array>`)
	for _, arg := range args {
		body.WriteString("<string>")
		_ = xml.EscapeText(&body, []byte(arg))
		body.WriteString("</string>")
	}
	body.WriteString("</array><key>RunAtLoad</key><true/></dict></plist>")
	name := filepath.Join(directory, "worker.plist")
	if err := os.WriteFile(name, []byte(body.String()), 0600); err != nil {
		return err
	}
	if out, err := exec.Command("launchctl", "bootstrap", "gui/"+strconv.Itoa(os.Getuid()), name).CombinedOutput(); err != nil {
		return fmt.Errorf("start update service: %s: %w", out, err)
	}
	return nil
}

func (m *Manager) Worker(action string) error {
	return m.withLock(true, func() error {
		_ = os.Remove(filepath.Join(m.directory(), "starting"))
		s, err := m.Status()
		if err != nil {
			return err
		}
		log, err := os.OpenFile(filepath.Join(m.directory(), "update.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer log.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		if action == "prepare" {
			err = m.prepare(ctx, &s, log)
		} else if action == "apply" {
			err = m.apply(ctx, &s, log)
		} else {
			err = fmt.Errorf("invalid worker action")
		}
		if err != nil {
			s.Phase, s.Error = "failed", err.Error()
			_ = m.save(s)
			if m.Component == "daemon" {
				_ = os.Remove(filepath.Join(m.directory(), "draining"))
			}
		}
		return err
	})
}

func run(ctx context.Context, log io.Writer, dir, command string, args ...string) error {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w (see update.log)", command, err)
	}
	return nil
}

func (m *Manager) prepare(ctx context.Context, s *Status, log io.Writer) error {
	if s.Target == nil {
		return fmt.Errorf("no selected release")
	}
	checkout, err := os.MkdirTemp(m.directory(), "source-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(checkout)
	if err = run(ctx, log, checkout, "git", "init", "-q"); err != nil {
		return err
	}
	if err = run(ctx, log, checkout, "git", "fetch", "--depth=1", m.repository(), s.Target.Commit); err != nil {
		return err
	}
	if err = run(ctx, log, checkout, "git", "checkout", "--detach", "FETCH_HEAD"); err != nil {
		return err
	}
	target, err := readManifest(filepath.Join(checkout, "update.json"))
	if err != nil {
		return fmt.Errorf("release does not support application updates: %w", err)
	}
	current, err := readManifest(m.current("UPDATE.json"))
	if err != nil {
		return err
	}
	if target.Version != s.Target.Version || target.API != current.API || target.Data != current.Data {
		return fmt.Errorf("release requires a manual compatibility or data migration")
	}
	// Only generated, inactive build output is replaced on a retry.
	prepared := filepath.Join(m.Prefix, m.Component, "prepared")
	if err = os.RemoveAll(prepared); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "sh", filepath.Join(checkout, "install.sh"), "--source", checkout, "--component", m.Component, "--prefix", m.Prefix, "--no-service")
	cmd.Env = append(os.Environ(), "ZOTIGO_PREPARE_UPDATE=1")
	cmd.Stdout, cmd.Stderr = log, log
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("prepare release: %w (see update.log)", err)
	}
	built, err := readManifest(filepath.Join(prepared, "UPDATE.json"))
	if err != nil {
		return err
	}
	if built != target {
		return fmt.Errorf("prepared manifest differs from the selected release")
	}
	s.Phase = "ready"
	return m.save(*s)
}

func (m *Manager) apply(ctx context.Context, s *Status, log io.Writer) error {
	// Use the installed updater contract rather than executing an unvalidated
	// command supplied by an HTTP caller.
	prepared, err := readManifest(filepath.Join(m.Prefix, m.Component, "prepared", "UPDATE.json"))
	if err != nil {
		return err
	}
	current, err := readManifest(m.current("UPDATE.json"))
	if err != nil {
		return err
	}
	if s.Target == nil || prepared.Version != s.Target.Version || prepared.API != current.API || prepared.Data != current.Data {
		return fmt.Errorf("prepared release is incompatible")
	}
	// assert_stopped scans executable paths and argv. Run outside the component
	// directory so the updater cannot mistake its own shell for a live app.
	script, err := os.ReadFile(m.current("update-support/apply-update.sh"))
	if err != nil {
		return err
	}
	scriptPath := filepath.Join(m.directory(), "apply.sh")
	if err := os.WriteFile(scriptPath, script, 0600); err != nil {
		return err
	}
	helpers, err := os.ReadFile(m.current("update-support/install-common.sh"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(m.directory(), "install-common.sh"), helpers, 0600); err != nil {
		return err
	}
	if err := run(ctx, log, m.directory(), "sh", scriptPath, m.Prefix, m.Component); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 10 {
			s.Phase, s.Error = "ready", ""
			return m.save(*s)
		}
		return err
	}
	s.Phase, s.Error, s.Target = "completed", "", nil
	return m.save(*s)
}
