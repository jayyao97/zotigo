package tools

import (
	"path/filepath"
	"sync"
	"time"
)

// ReadTracker is a session-scoped bag of "which files the model has
// read and what they looked like at that moment". It is consumed by
// tool-call middleware (see core/tools/middleware/tracker.go) which
// calls Mark on successful reads and HasRead + Snapshot on attempted
// mutations, and is otherwise invisible to tool code.
//
// ReadTracker is not an Executor wrapper. Paths are normalized against
// a working directory so relative and absolute forms compare equal;
// callers pass the working directory once at construction.
type ReadTracker struct {
	workDir string

	mu        sync.Mutex
	snapshots map[string]ReadSnapshot
}

// ReadSnapshot records the size and mtime captured at read time so
// mutators can detect that the file changed on disk since then.
type ReadSnapshot struct {
	Size  int64
	Mtime time.Time
}

// NewReadTracker returns an empty tracker that normalizes paths against
// workDir. A zero workDir treats all paths as absolute.
func NewReadTracker(workDir string) *ReadTracker {
	return &ReadTracker{
		workDir:   filepath.Clean(workDir),
		snapshots: make(map[string]ReadSnapshot),
	}
}

// HasRead reports whether the path has been marked during this session.
func (t *ReadTracker) HasRead(path string) bool {
	_, ok := t.Snapshot(path)
	return ok
}

// Mark records a snapshot for the given path. Subsequent mutations
// against the same path are accepted until the file changes on disk.
func (t *ReadTracker) Mark(path string, snap ReadSnapshot) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.snapshots[t.key(path)] = snap
}

// Snapshot returns the stored snapshot for path (or the zero value and
// false when not tracked).
func (t *ReadTracker) Snapshot(path string) (ReadSnapshot, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.snapshots[t.key(path)]
	return s, ok
}

func (t *ReadTracker) key(path string) string {
	if !filepath.IsAbs(path) && t.workDir != "" {
		path = filepath.Join(t.workDir, path)
	}
	return filepath.Clean(path)
}
