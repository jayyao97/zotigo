package zotigod

import "sync"

type sessionOperationLock struct {
	mu         sync.Mutex
	cond       *sync.Cond
	held       bool
	admissions int
	refs       int
}

type sessionOperationLocks struct {
	mu      sync.Mutex
	entries map[string]*sessionOperationLock
}

func newSessionOperationLocks() *sessionOperationLocks {
	return &sessionOperationLocks{entries: make(map[string]*sessionOperationLock)}
}

func (l *sessionOperationLocks) lock(sessionID string) func() {
	return l.acquire(sessionID, false)
}

// lockWorkerCallback serializes a worker callback with session mutations while
// allowing callbacks required to finish an in-flight input admission.
func (l *sessionOperationLocks) lockWorkerCallback(sessionID string) func() {
	return l.acquire(sessionID, true)
}

func (l *sessionOperationLocks) acquire(sessionID string, allowAdmission bool) func() {
	entry := l.retain(sessionID)
	entry.mu.Lock()
	for entry.held || (!allowAdmission && entry.admissions > 0) {
		entry.cond.Wait()
	}
	entry.held = true
	entry.mu.Unlock()

	return func() {
		entry.mu.Lock()
		entry.held = false
		entry.cond.Broadcast()
		entry.mu.Unlock()
		l.release(sessionID, entry)
	}
}

// reserveInput keeps ordinary session mutations serialized behind an input
// that has been handed to a worker without holding the session lock across the
// worker RPC. Worker callbacks use lockWorkerCallback to complete the input.
func (l *sessionOperationLocks) reserveInput(sessionID string) func() {
	entry := l.retain(sessionID)
	entry.mu.Lock()
	entry.admissions++
	entry.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			entry.mu.Lock()
			entry.admissions--
			entry.cond.Broadcast()
			entry.mu.Unlock()
			l.release(sessionID, entry)
		})
	}
}

func (l *sessionOperationLocks) retain(sessionID string) *sessionOperationLock {
	l.mu.Lock()
	entry := l.entries[sessionID]
	if entry == nil {
		entry = &sessionOperationLock{}
		entry.cond = sync.NewCond(&entry.mu)
		l.entries[sessionID] = entry
	}
	entry.refs++
	l.mu.Unlock()
	return entry
}

func (l *sessionOperationLocks) release(sessionID string, entry *sessionOperationLock) {
	l.mu.Lock()
	entry.refs--
	if entry.refs == 0 {
		delete(l.entries, sessionID)
	}
	l.mu.Unlock()
}
