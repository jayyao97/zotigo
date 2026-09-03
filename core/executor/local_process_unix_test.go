//go:build !windows

package executor

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

func TestLocalExecutorCancellationKillsCommandProcessGroup(t *testing.T) {
	root := t.TempDir()
	executor, err := NewLocalExecutor(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := executor.Exec(ctx, "sleep 30 & echo $! > child.pid; wait", ExecOptions{})
		done <- err
	}()
	pidPath := filepath.Join(root, "child.pid")
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, readErr := os.ReadFile(pidPath)
		if readErr == nil && strings.TrimSpace(string(data)) != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child process did not start: %v", readErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("executor remained blocked by a descendant process after cancellation")
	}
	deadline = time.Now().Add(time.Second)
	for {
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant process %d survived cancellation: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
