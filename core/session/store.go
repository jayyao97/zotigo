package session

import (
	"context"
)

// Store defines the interface for session storage backends.
// Implementations can be file-based (local), Redis, database, etc.
type Store interface {
	// Get retrieves a session by ID.
	// Returns nil if not found.
	Get(ctx context.Context, id string) (*Session, error)

	// Put stores a session.
	Put(ctx context.Context, sess *Session) error

	// Delete removes a session by ID.
	Delete(ctx context.Context, id string) error

	// List returns all sessions matching the filter.
	List(ctx context.Context, filter ListFilter) ([]Metadata, error)

	// Lock acquires an exclusive lock on a session.
	// Returns error if already locked.
	Lock(ctx context.Context, id string) error

	// Unlock releases the lock on a session.
	Unlock(ctx context.Context, id string) error

	// IsLocked checks if a session is currently locked.
	IsLocked(ctx context.Context, id string) (bool, error)

	// Close releases any resources held by the store.
	Close() error
}

// ListFilter defines filtering criteria for listing sessions.
type ListFilter struct {
	// WorkingDirectory filters by project path.
	// Empty string means no filter.
	WorkingDirectory string

	// Limit limits the number of results.
	// 0 means no limit.
	Limit int

	// OrderBy specifies the sort order.
	OrderBy OrderBy
}

// OrderBy defines sort order options.
type OrderBy int

const (
	// OrderByUpdatedDesc sorts by UpdatedAt descending (newest first).
	OrderByUpdatedDesc OrderBy = iota
	// OrderByUpdatedAsc sorts by UpdatedAt ascending (oldest first).
	OrderByUpdatedAsc
	// OrderByCreatedDesc sorts by CreatedAt descending.
	OrderByCreatedDesc
	// OrderByCreatedAsc sorts by CreatedAt ascending.
	OrderByCreatedAsc
)
