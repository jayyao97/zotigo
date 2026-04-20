package tools

// ReadTracker lets mutating tools verify that the agent has actually
// opened a file with read_file before editing or overwriting it. The
// agent wraps its executor with an implementation that marks paths as
// read on successful ReadFile calls, so the model can't hallucinate a
// file's contents and then overwrite it.
//
// Tools check the tracker by type-asserting the executor passed to
// Execute:
//
//	if tracker, ok := exec.(tools.ReadTracker); ok {
//	    if !tracker.HasRead(path) { /* refuse */ }
//	}
//
// The type assertion is intentional: it keeps the Tool interface free
// of a hard dependency on session state, and lets third-party executors
// opt in by implementing this interface.
type ReadTracker interface {
	// HasRead reports whether the given path has been opened via
	// ReadFile during the current session. Implementations should
	// normalize the path (resolve relative paths against the working
	// directory, clean separators) before comparison.
	HasRead(path string) bool
	// MarkRead records that the given path was read. Primarily useful
	// for tools that want to credit themselves (e.g. a grep tool that
	// streamed the contents).
	MarkRead(path string)
}
