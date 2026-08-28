package hooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

const maxHookOutputBytes = 64 * 1024

type processResult struct {
	stdout          string
	stderr          string
	exitCode        int
	started         bool
	timedOut        bool
	cancelled       bool
	stdoutTruncated bool
	stderrTruncated bool
	err             error
}

type limitedBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	remaining int
	truncated bool
}

func newLimitedBuffer(limit int) *limitedBuffer {
	return &limitedBuffer{remaining: limit}
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	written := len(data)
	if len(data) > b.remaining {
		data = data[:b.remaining]
		b.truncated = true
	}
	if len(data) > 0 {
		_, _ = b.buffer.Write(data)
		b.remaining -= len(data)
	}
	return written, nil
}

func (b *limitedBuffer) snapshot() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String(), b.truncated
}

func executeCommand(ctx context.Context, handler Handler, event Event) processResult {
	if err := ctx.Err(); err != nil {
		return processResult{timedOut: errors.Is(err, context.DeadlineExceeded), cancelled: errors.Is(err, context.Canceled), err: err}
	}
	payload, err := marshalEvent(event)
	if err != nil {
		return processResult{err: err}
	}
	command := exec.Command(handler.Command, handler.Args...)
	command.Dir = event.CWD
	command.Env = hookEnvironment(os.Environ(), event)
	command.Stdin = bytes.NewReader(payload)
	stdout := newLimitedBuffer(maxHookOutputBytes)
	stderr := newLimitedBuffer(maxHookOutputBytes)
	command.Stdout = stdout
	command.Stderr = stderr
	processGroup, err := newProcessGroup(command)
	if err != nil {
		return processResult{err: fmt.Errorf("create process group: %w", err)}
	}
	defer func() { _ = processGroup.close() }()
	if err := command.Start(); err != nil {
		return processResult{err: err}
	}
	if err := processGroup.attach(command); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return processResult{started: true, exitCode: -1, err: fmt.Errorf("attach process group: %w", err)}
	}
	if err := processGroup.resume(command); err != nil {
		_ = processGroup.terminate(command)
		_ = command.Process.Kill()
		_ = command.Wait()
		return processResult{started: true, exitCode: -1, err: fmt.Errorf("resume process: %w", err)}
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- command.Wait() }()
	result := processResult{started: true}
	select {
	case err = <-waitCh:
		result.err = err
	case <-ctx.Done():
		result.timedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
		result.cancelled = errors.Is(ctx.Err(), context.Canceled)
		_ = processGroup.terminate(command)
		result.err = errors.Join(ctx.Err(), <-waitCh)
	}
	result.stdout, result.stdoutTruncated = stdout.snapshot()
	result.stderr, result.stderrTruncated = stderr.snapshot()
	if result.err == nil {
		result.exitCode = 0
		return result
	}
	var exitErr *exec.ExitError
	if errors.As(result.err, &exitErr) {
		result.exitCode = exitErr.ExitCode()
	} else {
		result.exitCode = -1
	}
	return result
}

func hookEnvironment(environment []string, event Event) []string {
	result := make([]string, 0, len(environment)+2)
	for _, value := range environment {
		if strings.HasPrefix(value, "ZOTIGO_SESSION_ID=") || strings.HasPrefix(value, "ZOTIGO_HOOK_EVENT=") {
			continue
		}
		result = append(result, value)
	}
	return append(result, "ZOTIGO_SESSION_ID="+event.SessionID, "ZOTIGO_HOOK_EVENT="+string(event.EventName))
}
