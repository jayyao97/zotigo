package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
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
		Turns: []Turn{
			{
				ID:                "turn_1",
				CreatedAt:         time.Now(),
				UpdatedAt:         time.Now(),
				UserPromptSummary: "hello",
				SafetyEvents: []SafetyEvent{
					{
						Timestamp:      time.Now(),
						TurnID:         "turn_1",
						ToolName:       "shell",
						DecisionSource: SafetyDecisionSourceClassifier,
						Decision:       SafetyDecisionAskUser,
					},
				},
			},
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
	if len(loaded.Turns) != 1 {
		t.Fatalf("Expected 1 turn, got %d", len(loaded.Turns))
	}
	if len(loaded.Turns[0].SafetyEvents) != 1 {
		t.Fatalf("Expected 1 safety event, got %d", len(loaded.Turns[0].SafetyEvents))
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
	imageDir := filepath.Join(tmpDir, "sessions", "test_delete.images")
	if err := os.MkdirAll(imageDir, 0700); err != nil {
		t.Fatalf("Create image dir failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(imageDir, "image.png"), []byte("image"), 0600); err != nil {
		t.Fatalf("Write image blob failed: %v", err)
	}
	if err := store.PutImageRefs(ctx, []ImageRef{{
		SessionID: sess.ID,
		Name:      "image.png",
		BlobPath:  filepath.Join("sessions", sess.ID+".images", "image.png"),
		MimeType:  "image/png",
	}}); err != nil {
		t.Fatalf("Put image ref failed: %v", err)
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
	listed, err := store.List(ctx, ListFilter{WorkingDirectory: "/tmp/test"})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("Deleted session should be removed from index, got %#v", listed)
	}
	if _, err := os.Stat(imageDir); !os.IsNotExist(err) {
		t.Fatalf("Image blob directory should be deleted, got err=%v", err)
	}
	if _, ok, err := store.GetImageRef(ctx, sess.ID, "image.png"); err != nil {
		t.Fatalf("Get image ref failed: %v", err)
	} else if ok {
		t.Fatalf("Image ref should be deleted with session")
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

func TestFileStore_ListUsesSQLiteIndex(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{
		Metadata: Metadata{
			ID:               "sqlite_indexed",
			WorkingDirectory: "/project/sqlite",
			LastPrompt:       "hello sqlite",
			CreatedAt:        time.Now().Add(-time.Hour),
			UpdatedAt:        time.Now(),
		},
	}
	if err := store.Put(ctx, sess); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	if err := os.WriteFile(store.registryPath, []byte(`{"sessions":[]}`), 0644); err != nil {
		t.Fatalf("overwrite legacy registry: %v", err)
	}

	listed, err := store.List(ctx, ListFilter{WorkingDirectory: "/project/sqlite"})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("Expected 1 indexed session, got %d", len(listed))
	}
	if listed[0].ID != "sqlite_indexed" {
		t.Fatalf("Expected sqlite_indexed, got %s", listed[0].ID)
	}
	if listed[0].LastPrompt != "hello sqlite" {
		t.Fatalf("Expected LastPrompt to round trip, got %q", listed[0].LastPrompt)
	}
}

func TestFileStore_SQLiteIndexOrdersAndLimits(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	base := time.Now().Add(-3 * time.Hour)
	for idx, id := range []string{"oldest", "middle", "newest"} {
		if err := store.Put(ctx, &Session{Metadata: Metadata{
			ID:               id,
			WorkingDirectory: "/project/order",
			CreatedAt:        base.Add(time.Duration(idx) * time.Hour),
			UpdatedAt:        base.Add(time.Duration(idx) * time.Hour),
		}}); err != nil {
			t.Fatalf("Put %s failed: %v", id, err)
		}
	}

	listed, err := store.List(ctx, ListFilter{
		WorkingDirectory: "/project/order",
		OrderBy:          OrderByUpdatedDesc,
		Limit:            2,
	})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("Expected 2 sessions, got %d", len(listed))
	}
	if listed[0].ID != "newest" || listed[1].ID != "middle" {
		t.Fatalf("Unexpected order: %s, %s", listed[0].ID, listed[1].ID)
	}
}

func TestFileStore_SQLiteIndexOrdersZeroAndNonZeroNanosecondTimes(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	earlier := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	later := time.Date(2026, 1, 1, 0, 0, 0, 1, time.UTC)
	for _, meta := range []Metadata{
		{ID: "earlier_zero_nano", WorkingDirectory: "/project/time", CreatedAt: earlier, UpdatedAt: earlier},
		{ID: "later_one_nano", WorkingDirectory: "/project/time", CreatedAt: later, UpdatedAt: later},
	} {
		if err := store.Put(ctx, &Session{Metadata: meta}); err != nil {
			t.Fatalf("Put %s failed: %v", meta.ID, err)
		}
	}

	desc, err := store.List(ctx, ListFilter{WorkingDirectory: "/project/time", OrderBy: OrderByUpdatedDesc})
	if err != nil {
		t.Fatalf("List desc failed: %v", err)
	}
	if desc[0].ID != "later_one_nano" || desc[1].ID != "earlier_zero_nano" {
		t.Fatalf("Unexpected desc order: %s, %s", desc[0].ID, desc[1].ID)
	}
	asc, err := store.List(ctx, ListFilter{WorkingDirectory: "/project/time", OrderBy: OrderByCreatedAsc})
	if err != nil {
		t.Fatalf("List asc failed: %v", err)
	}
	if asc[0].ID != "earlier_zero_nano" || asc[1].ID != "later_one_nano" {
		t.Fatalf("Unexpected asc order: %s, %s", asc[0].ID, asc[1].ID)
	}
}

func TestFileStore_SQLiteIndexOrdersEqualTimestampsByID(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"sess_a", "sess_c", "sess_b"} {
		if err := store.Put(ctx, &Session{Metadata: Metadata{
			ID:               id,
			WorkingDirectory: "/project/tie",
			CreatedAt:        ts,
			UpdatedAt:        ts,
		}}); err != nil {
			t.Fatalf("Put %s failed: %v", id, err)
		}
	}

	desc, err := store.List(ctx, ListFilter{WorkingDirectory: "/project/tie", OrderBy: OrderByUpdatedDesc})
	if err != nil {
		t.Fatalf("List desc failed: %v", err)
	}
	if desc[0].ID != "sess_c" || desc[1].ID != "sess_b" || desc[2].ID != "sess_a" {
		t.Fatalf("Unexpected desc tiebreak order: %#v", desc)
	}
	asc, err := store.List(ctx, ListFilter{WorkingDirectory: "/project/tie", OrderBy: OrderByCreatedAsc})
	if err != nil {
		t.Fatalf("List asc failed: %v", err)
	}
	if asc[0].ID != "sess_a" || asc[1].ID != "sess_b" || asc[2].ID != "sess_c" {
		t.Fatalf("Unexpected asc tiebreak order: %#v", asc)
	}
}

func TestFileStore_SQLiteImageRefs(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	ref := ImageRef{
		SessionID: "sess_images",
		Name:      "image.png",
		BlobPath:  filepath.Join("sessions", "sess_images.images", "image.png"),
		MimeType:  "image/png",
		SizeBytes: 10,
		Width:     1,
		Height:    2,
		CreatedAt: time.Now(),
	}
	if err := store.PutImageRefs(ctx, []ImageRef{ref}); err != nil {
		t.Fatalf("PutImageRefs failed: %v", err)
	}
	got, ok, err := store.GetImageRef(ctx, "sess_images", "image.png")
	if err != nil {
		t.Fatalf("GetImageRef failed: %v", err)
	}
	if !ok {
		t.Fatalf("Expected image ref")
	}
	if got.BlobPath != ref.BlobPath || got.MimeType != ref.MimeType || got.SizeBytes != ref.SizeBytes || got.Width != ref.Width || got.Height != ref.Height {
		t.Fatalf("Unexpected image ref: %#v", got)
	}
	if err := store.DeleteImageRefs(ctx, "sess_images", []string{"image.png"}); err != nil {
		t.Fatalf("DeleteImageRefs failed: %v", err)
	}
	if _, ok, err := store.GetImageRef(ctx, "sess_images", "image.png"); err != nil {
		t.Fatalf("GetImageRef after delete failed: %v", err)
	} else if ok {
		t.Fatalf("Expected image ref to be deleted")
	}
}

func TestFileStore_SQLiteIndexRepairsExistingSessionFiles(t *testing.T) {
	tmpDir := t.TempDir()
	sessionsDir := filepath.Join(tmpDir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		t.Fatalf("Create sessions dir failed: %v", err)
	}
	sess := &Session{Metadata: Metadata{
		ID:               "legacy_file",
		WorkingDirectory: "/project/legacy",
		CreatedAt:        time.Now().Add(-time.Hour),
		UpdatedAt:        time.Now(),
	}}
	data, err := json.Marshal(sess)
	if err != nil {
		t.Fatalf("Marshal session failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionsDir, "legacy_file.json"), data, 0644); err != nil {
		t.Fatalf("Write legacy session failed: %v", err)
	}

	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	listed, err := store.List(context.Background(), ListFilter{WorkingDirectory: "/project/legacy"})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != "legacy_file" {
		t.Fatalf("Expected repaired legacy_file index row, got %#v", listed)
	}
}

func TestFileStore_SQLiteIndexBootstrapSkipsCorruptSessionFiles(t *testing.T) {
	tmpDir := t.TempDir()
	sessionsDir := filepath.Join(tmpDir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		t.Fatalf("Create sessions dir failed: %v", err)
	}
	valid := &Session{Metadata: Metadata{
		ID:               "valid_file",
		WorkingDirectory: "/project/bootstrap",
		CreatedAt:        time.Now().Add(-time.Hour),
		UpdatedAt:        time.Now(),
	}}
	data, err := json.Marshal(valid)
	if err != nil {
		t.Fatalf("Marshal session failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionsDir, "valid_file.json"), data, 0644); err != nil {
		t.Fatalf("Write valid session failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionsDir, "corrupt.json"), []byte(`{not-json`), 0644); err != nil {
		t.Fatalf("Write corrupt session failed: %v", err)
	}

	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create store with corrupt bootstrap file: %v", err)
	}
	defer store.Close()

	listed, err := store.List(context.Background(), ListFilter{WorkingDirectory: "/project/bootstrap"})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != "valid_file" {
		t.Fatalf("Expected valid_file index row, got %#v", listed)
	}
}

func TestFileStore_SQLiteIndexSyncsLegacyRegistryAfterBootstrap(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	ctx := context.Background()
	existing := Metadata{
		ID:               "existing",
		WorkingDirectory: "/project/existing",
		CreatedAt:        time.Now().Add(-2 * time.Hour),
		UpdatedAt:        time.Now().Add(-2 * time.Hour),
	}
	if err := store.Put(ctx, &Session{Metadata: existing}); err != nil {
		t.Fatalf("Put existing failed: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	legacy := Metadata{
		ID:               "legacy_after_rollback",
		WorkingDirectory: "/project/legacy-after-rollback",
		LastPrompt:       "created by old version",
		CreatedAt:        time.Now().Add(-time.Hour),
		UpdatedAt:        time.Now(),
	}
	sessData, err := json.Marshal(&Session{Metadata: legacy})
	if err != nil {
		t.Fatalf("Marshal legacy session failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "sessions", "legacy_after_rollback.json"), sessData, 0644); err != nil {
		t.Fatalf("Write legacy session failed: %v", err)
	}
	regData, err := json.Marshal(registry{Sessions: []Metadata{existing, legacy}})
	if err != nil {
		t.Fatalf("Marshal registry failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "registry.json"), regData, 0644); err != nil {
		t.Fatalf("Write legacy registry failed: %v", err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(tmpDir, "registry.json"), future, future); err != nil {
		t.Fatalf("Chtimes registry failed: %v", err)
	}

	reopened, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("Reopen store failed: %v", err)
	}
	defer reopened.Close()

	listed, err := reopened.List(ctx, ListFilter{WorkingDirectory: "/project/legacy-after-rollback"})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != "legacy_after_rollback" {
		t.Fatalf("Expected legacy registry session in SQLite index, got %#v", listed)
	}
	if listed[0].LastPrompt != "created by old version" {
		t.Fatalf("Expected LastPrompt from legacy registry, got %q", listed[0].LastPrompt)
	}
}

func TestFileStore_SQLiteIndexDoesNotResyncOwnLegacyRegistryWrite(t *testing.T) {
	tmpDir := t.TempDir()
	sessionsDir := filepath.Join(tmpDir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		t.Fatalf("Create sessions dir failed: %v", err)
	}
	existing := &Session{Metadata: Metadata{
		ID:               "existing_from_files",
		WorkingDirectory: "/project/bootstrap-existing",
		CreatedAt:        time.Now().Add(-2 * time.Hour),
		UpdatedAt:        time.Now().Add(-2 * time.Hour),
	}}
	data, err := json.Marshal(existing)
	if err != nil {
		t.Fatalf("Marshal existing session failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionsDir, "existing_from_files.json"), data, 0644); err != nil {
		t.Fatalf("Write existing session failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "registry.json"), []byte(`{not-json`), 0644); err != nil {
		t.Fatalf("Write invalid registry failed: %v", err)
	}

	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("Create store failed: %v", err)
	}
	ctx := context.Background()
	createdByNewVersion := &Session{Metadata: Metadata{
		ID:               "created_by_new_version",
		WorkingDirectory: "/project/bootstrap-existing",
		LastPrompt:       "new write",
		CreatedAt:        time.Now().Add(-time.Hour),
		UpdatedAt:        time.Now(),
	}}
	if err := store.Put(ctx, createdByNewVersion); err != nil {
		t.Fatalf("Put new session failed: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	reopened, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("Reopen store failed: %v", err)
	}
	defer reopened.Close()

	listed, err := reopened.List(ctx, ListFilter{WorkingDirectory: "/project/bootstrap-existing"})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	ids := make(map[string]bool)
	for _, meta := range listed {
		ids[meta.ID] = true
	}
	if !ids["existing_from_files"] || !ids["created_by_new_version"] {
		t.Fatalf("Expected bootstrap and new sessions to remain indexed, got %#v", listed)
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

func TestFileStore_LockCleansStalePID(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if err := os.WriteFile(store.lockPath("stale_lock"), []byte("99999999"), 0644); err != nil {
		t.Fatalf("write stale lock: %v", err)
	}

	if err := store.Lock(ctx, "stale_lock"); err != nil {
		t.Fatalf("expected stale lock to be cleaned up: %v", err)
	}
	defer func() { _ = store.Unlock(ctx, "stale_lock") }()

	data, err := os.ReadFile(store.lockPath("stale_lock"))
	if err != nil {
		t.Fatalf("read refreshed lock: %v", err)
	}
	if string(data) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("expected refreshed lock pid %d, got %q", os.Getpid(), string(data))
	}
}

func TestFileStore_LockIsExclusive(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	var success atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.Lock(ctx, "exclusive_lock"); err == nil {
				success.Add(1)
			}
		}()
	}
	wg.Wait()
	defer func() { _ = store.Unlock(ctx, "exclusive_lock") }()

	if got := success.Load(); got != 1 {
		t.Fatalf("expected exactly one lock acquisition, got %d", got)
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
	if sess.Turns == nil {
		t.Error("Expected turns slice to be initialized")
	}

	// Load the session
	loaded, err := mgr.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if loaded.ID != sess.ID {
		t.Errorf("ID mismatch: got %s, want %s", loaded.ID, sess.ID)
	}
	if loaded.Turns == nil {
		t.Error("Expected turns slice to be initialized on load")
	}
}

func TestFileStore_Get_BackfillsMissingTurns(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewFileStore(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	raw := `{
  "id": "legacy_session",
  "working_directory": "/tmp/test",
  "created_at": "2026-01-01T00:00:00Z",
  "updated_at": "2026-01-01T00:00:00Z",
  "agent_snapshot": {
    "state": "idle",
    "created_at": "2026-01-01T00:00:00Z"
  }
}`
	sessionPath := store.sessionPath("legacy_session")
	if err := os.WriteFile(sessionPath, []byte(raw), 0644); err != nil {
		t.Fatalf("Failed to write legacy session: %v", err)
	}

	loaded, err := store.Get(context.Background(), "legacy_session")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if loaded == nil {
		t.Fatal("Expected loaded session")
	}
	if loaded.Turns == nil {
		t.Fatal("Expected missing turns to be backfilled")
	}
	if len(loaded.Turns) != 0 {
		t.Fatalf("Expected no turns, got %d", len(loaded.Turns))
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
