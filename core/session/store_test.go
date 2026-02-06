package session

import (
	"context"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
)

func TestFileStore_PutGet(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a session
	sess := &Session{
		Metadata: Metadata{
			ID:               "test_session_1",
			WorkingDirectory: "/tmp/test",
			LastPrompt:       "Hello world",
			CreatedAt:        time.Now(),
			UpdatedAt:        time.Now(),
		},
		AgentSnapshot: agent.Snapshot{
			State:     agent.StateIdle,
			CreatedAt: time.Now(),
		},
	}

	// Put
	err = store.Put(ctx, sess)
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// Get
	loaded, err := store.Get(ctx, "test_session_1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if loaded == nil {
		t.Fatal("Get returned nil")
	}
	if loaded.ID != sess.ID {
		t.Errorf("ID mismatch: got %s, want %s", loaded.ID, sess.ID)
	}
	if loaded.LastPrompt != sess.LastPrompt {
		t.Errorf("LastPrompt mismatch: got %s, want %s", loaded.LastPrompt, sess.LastPrompt)
	}
}

func TestFileStore_GetNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Get non-existent session
	sess, err := store.Get(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if sess != nil {
		t.Error("Expected nil for non-existent session")
	}
}

func TestFileStore_Delete(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a session
	sess := &Session{
		Metadata: Metadata{
			ID:               "test_delete",
			WorkingDirectory: "/tmp/test",
			CreatedAt:        time.Now(),
			UpdatedAt:        time.Now(),
		},
	}

	// Put
	err = store.Put(ctx, sess)
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// Delete
	err = store.Delete(ctx, "test_delete")
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Get should return nil
	loaded, err := store.Get(ctx, "test_delete")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if loaded != nil {
		t.Error("Session should be deleted")
	}
}

func TestFileStore_List(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create multiple sessions
	sessions := []*Session{
		{
			Metadata: Metadata{
				ID:               "sess_1",
				WorkingDirectory: "/project/a",
				CreatedAt:        time.Now().Add(-2 * time.Hour),
				UpdatedAt:        time.Now().Add(-2 * time.Hour),
			},
		},
		{
			Metadata: Metadata{
				ID:               "sess_2",
				WorkingDirectory: "/project/a",
				CreatedAt:        time.Now().Add(-1 * time.Hour),
				UpdatedAt:        time.Now().Add(-1 * time.Hour),
			},
		},
		{
			Metadata: Metadata{
				ID:               "sess_3",
				WorkingDirectory: "/project/b",
				CreatedAt:        time.Now(),
				UpdatedAt:        time.Now(),
			},
		},
	}

	for _, s := range sessions {
		if err := store.Put(ctx, s); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
	}

	// List all
	all, err := store.List(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("Expected 3 sessions, got %d", len(all))
	}

	// List by working directory
	projectA, err := store.List(ctx, ListFilter{WorkingDirectory: "/project/a"})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(projectA) != 2 {
		t.Errorf("Expected 2 sessions for /project/a, got %d", len(projectA))
	}

	// List with limit
	limited, err := store.List(ctx, ListFilter{Limit: 1})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("Expected 1 session, got %d", len(limited))
	}
}

func TestFileStore_Lock(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Lock
	err = store.Lock(ctx, "test_lock")
	if err != nil {
		t.Fatalf("Lock failed: %v", err)
	}

	// IsLocked should return true
	locked, err := store.IsLocked(ctx, "test_lock")
	if err != nil {
		t.Fatalf("IsLocked failed: %v", err)
	}
	if !locked {
		t.Error("Expected session to be locked")
	}

	// Lock again should fail
	err = store.Lock(ctx, "test_lock")
	if err == nil {
		t.Error("Expected error when locking already locked session")
	}

	// Unlock
	err = store.Unlock(ctx, "test_lock")
	if err != nil {
		t.Fatalf("Unlock failed: %v", err)
	}

	// IsLocked should return false
	locked, err = store.IsLocked(ctx, "test_lock")
	if err != nil {
		t.Fatalf("IsLocked failed: %v", err)
	}
	if locked {
		t.Error("Expected session to be unlocked")
	}
}

func TestManager_CreateAndLoad(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	mgr := NewManagerWithStore(store)
	defer mgr.Close()

	// Create new session
	sess, err := mgr.CreateNew("/test/project")
	if err != nil {
		t.Fatalf("CreateNew failed: %v", err)
	}

	if sess.ID == "" {
		t.Error("Session ID should not be empty")
	}
	if sess.WorkingDirectory != "/test/project" {
		t.Errorf("WorkingDirectory mismatch: got %s", sess.WorkingDirectory)
	}

	// Load the session
	loaded, err := mgr.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if loaded.ID != sess.ID {
		t.Errorf("ID mismatch: got %s, want %s", loaded.ID, sess.ID)
	}
}

func TestManager_ListByDir(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	mgr := NewManagerWithStore(store)
	defer mgr.Close()

	// Create sessions in different directories
	_, err = mgr.CreateNew("/project/a")
	if err != nil {
		t.Fatalf("CreateNew failed: %v", err)
	}
	_, err = mgr.CreateNew("/project/a")
	if err != nil {
		t.Fatalf("CreateNew failed: %v", err)
	}
	_, err = mgr.CreateNew("/project/b")
	if err != nil {
		t.Fatalf("CreateNew failed: %v", err)
	}

	// List for /project/a
	sessions, err := mgr.ListByDir("/project/a")
	if err != nil {
		t.Fatalf("ListByDir failed: %v", err)
	}

	if len(sessions) != 2 {
		t.Errorf("Expected 2 sessions, got %d", len(sessions))
	}
}
