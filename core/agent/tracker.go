package agent

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/tools"
)

// readTrackingExecutor wraps an Executor and remembers which file paths
// have been read during the session. Mutating tools (Edit, Write) check
// the tracker to refuse edits against files the model has not actually
// opened — prevents the "hallucinate contents, then overwrite" failure.
type readTrackingExecutor struct {
	executor.Executor
	mu      sync.Mutex
	readSet map[string]bool
}

// wrapReadTracker wraps exec with a ReadTracker. If exec already
// implements tools.ReadTracker, it is returned as-is to avoid
// double-wrapping.
func wrapReadTracker(exec executor.Executor) executor.Executor {
	if _, ok := exec.(tools.ReadTracker); ok {
		return exec
	}
	return &readTrackingExecutor{
		Executor: exec,
		readSet:  make(map[string]bool),
	}
}

func (e *readTrackingExecutor) ReadFile(ctx context.Context, path string) ([]byte, error) {
	b, err := e.Executor.ReadFile(ctx, path)
	if err == nil {
		e.MarkRead(path)
	}
	return b, err
}

// Unwrap exposes the underlying executor so callers that do optional
// capability probing (shell tool's sandbox CheckCommand, etc.) can type
// assert against the real implementation rather than the wrapper.
func (e *readTrackingExecutor) Unwrap() executor.Executor { return e.Executor }

// HasRead implements tools.ReadTracker.
func (e *readTrackingExecutor) HasRead(path string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.readSet[e.key(path)]
}

// MarkRead implements tools.ReadTracker.
func (e *readTrackingExecutor) MarkRead(path string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.readSet[e.key(path)] = true
}

// key normalizes a path so reads and edits compare apples to apples
// regardless of whether the caller used an absolute or a workdir-relative
// form.
func (e *readTrackingExecutor) key(path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(e.WorkDir(), path)
	}
	return filepath.Clean(path)
}
