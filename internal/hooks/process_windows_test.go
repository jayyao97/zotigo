//go:build windows

package hooks

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const windowsHookHelperEnv = "ZOTIGO_WINDOWS_HOOK_HELPER"

func TestExecuteCommandTimeoutKillsWindowsDescendants(t *testing.T) {
	if mode := os.Getenv(windowsHookHelperEnv); mode != "" {
		runWindowsHookHelper(mode)
		return
	}

	directory := t.TempDir()
	pidPath := filepath.Join(directory, "child.pid")
	t.Setenv(windowsHookHelperEnv, "parent")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := executeCommand(ctx, Handler{
		Command: os.Args[0],
		Args:    []string{"-test.run=^TestExecuteCommandTimeoutKillsWindowsDescendants$", "--", pidPath},
	}, Event{EventName: PostToolUse, SessionID: "sess-test", Agent: "zotigo", CWD: directory})
	if !result.timedOut {
		t.Fatalf("expected timeout: %#v", result)
	}

	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read descendant pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse descendant pid: %v", err)
	}
	assertWindowsProcessExited(t, uint32(pid))
}

func runWindowsHookHelper(mode string) {
	if mode == "parent" {
		_ = os.Setenv(windowsHookHelperEnv, "child")
		command := exec.Command(os.Args[0], os.Args[1:]...)
		if err := command.Start(); err != nil {
			os.Exit(3)
		}
		separator := -1
		for index, argument := range os.Args {
			if argument == "--" {
				separator = index
				break
			}
		}
		if separator < 0 || separator+1 >= len(os.Args) {
			os.Exit(4)
		}
		if err := os.WriteFile(os.Args[separator+1], []byte(strconv.Itoa(command.Process.Pid)), 0o600); err != nil {
			os.Exit(5)
		}
	}
	for {
		time.Sleep(time.Hour)
	}
}

func assertWindowsProcessExited(t *testing.T, pid uint32) {
	t.Helper()
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return
	}
	if err != nil {
		t.Fatalf("open descendant process: %v", err)
	}
	defer windows.CloseHandle(handle)
	status, err := windows.WaitForSingleObject(handle, 1000)
	if err != nil {
		t.Fatalf("wait for descendant process: %v", err)
	}
	if status != windows.WAIT_OBJECT_0 {
		t.Fatalf("hook descendant %d survived timeout", pid)
	}
}
