package tools

import (
	"context"
	"path/filepath"
	"sync"
	"time"

	"github.com/jayyao97/zotigo/core/executor"
)

// ReadTracker lets mutating tools verify two things before touching a
// file: that the agent has actually opened it with read_file, and that
// the on-disk contents haven't changed since then. Both guards prevent
// the "hallucinate-or-stale contents, then overwrite" failure.
//
// Any Executor can opt in by implementing this interface. Callers
// typically compose it with the concrete Executor via WrapReadTracker.
// Tools check the tracker by type-asserting the Executor they are
// handed:
//
//	if tracker, ok := exec.(tools.ReadTracker); ok {
//	    snap, ok := tracker.ReadSnapshot(path)
//	    if !ok { /* never read */ }
//	    // compare snap against a fresh Stat to detect external edits.
//	}
type ReadTracker interface {
	// HasRead reports whether the given path has been opened via
	// ReadFile during the current session. Paths are normalized
	// (relative → working directory; cleaned) so "foo.go" and
	// "/workdir/foo.go" compare equal.
	HasRead(path string) bool
	// MarkRead records that the given path was read. Fetches a fresh
	// snapshot (size + mtime) from the underlying executor if possible.
	MarkRead(path string)
	// ReadSnapshot returns the recorded size+mtime at the last read, or
	// (zero, false) if the path was never read.
	ReadSnapshot(path string) (ReadSnapshot, bool)
}

// ReadSnapshot is the fingerprint captured at read time. Comparing it
// against a fresh Stat lets a mutating tool detect that the file was
// changed out-of-band (another process, the user's editor, a git
// checkout) between the model's read and its intended edit.
type ReadSnapshot struct {
	Size  int64
	Mtime time.Time
}

// WrapReadTracker wraps exec so mutating tools can observe read history
// and external changes. If exec already implements ReadTracker it is
// returned unchanged. The wrapper also exposes Unwrap() so tools that
// need optional capabilities on the underlying executor (e.g. shell's
// sandbox CheckCommand) can still reach them.
func WrapReadTracker(exec executor.Executor) executor.Executor {
	if _, ok := exec.(ReadTracker); ok {
		return exec
	}
	return &readTrackingExecutor{
		Executor:  exec,
		snapshots: make(map[string]ReadSnapshot),
	}
}

type readTrackingExecutor struct {
	executor.Executor
	mu        sync.Mutex
	snapshots map[string]ReadSnapshot
}

// ReadFile delegates to the wrapped executor and, on success, records a
// snapshot so Edit/Write can detect external changes later.
func (e *readTrackingExecutor) ReadFile(ctx context.Context, path string) ([]byte, error) {
	b, err := e.Executor.ReadFile(ctx, path)
	if err == nil {
		snap := ReadSnapshot{Size: int64(len(b))}
		if info, statErr := e.Executor.Stat(ctx, path); statErr == nil && info != nil {
			snap.Size = info.Size
			snap.Mtime = info.ModTime
		}
		e.setSnapshot(path, snap)
	}
	return b, err
}

// Unwrap exposes the underlying executor so callers that do optional
// capability probing (shell tool's sandbox CheckCommand, etc.) can type
// assert against the real implementation rather than the wrapper.
func (e *readTrackingExecutor) Unwrap() executor.Executor { return e.Executor }

// HasRead implements ReadTracker.
func (e *readTrackingExecutor) HasRead(path string) bool {
	_, ok := e.ReadSnapshot(path)
	return ok
}

// MarkRead implements ReadTracker. Captures a best-effort snapshot from
// the underlying executor so external-change detection works even when
// the caller credits itself via MarkRead rather than going through
// ReadFile.
func (e *readTrackingExecutor) MarkRead(path string) {
	snap := ReadSnapshot{}
	if info, err := e.Executor.Stat(context.Background(), path); err == nil && info != nil {
		snap.Size = info.Size
		snap.Mtime = info.ModTime
	}
	e.setSnapshot(path, snap)
}

// ReadSnapshot implements ReadTracker.
func (e *readTrackingExecutor) ReadSnapshot(path string) (ReadSnapshot, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.snapshots[e.key(path)]
	return s, ok
}

func (e *readTrackingExecutor) setSnapshot(path string, s ReadSnapshot) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.snapshots[e.key(path)] = s
}

func (e *readTrackingExecutor) key(path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(e.WorkDir(), path)
	}
	return filepath.Clean(path)
}
