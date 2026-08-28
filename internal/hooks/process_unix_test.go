//go:build !windows

package hooks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCloseKillsAsyncHookDescendants(t *testing.T) {
	directory := t.TempDir()
	pidPath := filepath.Join(directory, "child.pid")
	dispatcher := New(Config{Version: ConfigVersion, Hooks: map[EventName][]Handler{
		PostToolUse: {{
			Command: "/bin/sh",
			Args:    []string{"-c", `sleep 30 & child=$!; printf '%s' "$child" > "$1"; wait "$child"`, "hook", pidPath},
			Async:   true,
		}},
	}})
	event := testToolEvent(PostToolUse, "shell")
	event.CWD = directory
	dispatcher.Dispatch(context.Background(), event)

	childPID := waitForPIDFile(t, pidPath)
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := dispatcher.Close(closeCtx); err != nil {
		t.Fatalf("close dispatcher: %v", err)
	}
	waitForProcessExit(t, childPID)
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr != nil {
				t.Fatalf("parse child pid: %v", parseErr)
			}
			return pid
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read child pid: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("hook child did not start")
	return 0
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil {
			t.Fatalf("inspect child process: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("hook descendant %d survived dispatcher close", pid)
}
