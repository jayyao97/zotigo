package zotigod

import "sync"

type sessionOperationLock struct {
	mu   sync.Mutex
	refs int
}

type sessionOperationLocks struct {
	mu      sync.Mutex
	entries map[string]*sessionOperationLock
}

func newSessionOperationLocks() *sessionOperationLocks {
	return &sessionOperationLocks{entries: make(map[string]*sessionOperationLock)}
}

func (l *sessionOperationLocks) lock(sessionID string) func() {
	l.mu.Lock()
	entry := l.entries[sessionID]
	if entry == nil {
		entry = &sessionOperationLock{}
		l.entries[sessionID] = entry
	}
	entry.refs++
	l.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		l.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(l.entries, sessionID)
		}
		l.mu.Unlock()
	}
}
