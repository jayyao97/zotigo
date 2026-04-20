package executor

// unwrapper is implemented by executor wrappers (read trackers, metric
// decorators, caches, ...) that sit in front of another Executor and
// want to let callers peel them off when probing for a capability the
// inner executor provides.
type unwrapper interface {
	Unwrap() Executor
}

// Unwrap peels all layered wrappers from e and returns the innermost
// Executor. Wrappers opt in by implementing the Unwrap() Executor
// method — the convention mirrors errors.Unwrap. Safe to call on an
// unwrapped executor (returns it unchanged).
func Unwrap(e Executor) Executor {
	for {
		u, ok := e.(unwrapper)
		if !ok {
			return e
		}
		e = u.Unwrap()
	}
}

// Probe looks up an optional capability T on the Executor, walking
// through any Unwrap() layers along the way. It's the standard way for
// a tool to ask "does this executor support feature X, no matter how
// many wrappers sit in front of it?".
//
// Example:
//
//	if tr, ok := executor.Probe[tools.ReadTracker](exec); ok {
//	    if !tr.HasRead(path) { ... }
//	}
//
// Returns the zero value of T and false when no layer implements T.
func Probe[T any](e Executor) (T, bool) {
	for {
		if t, ok := any(e).(T); ok {
			return t, true
		}
		u, ok := e.(unwrapper)
		if !ok {
			var zero T
			return zero, false
		}
		e = u.Unwrap()
	}
}
