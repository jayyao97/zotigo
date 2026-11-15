package workerbase

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/testutil/catalogtest"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

func TestLoadWorkerCommandCursorDoesNotSkipPendingMessage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	defer store.Close()
	sessionID := "sess-corrupt-cursor"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        sessionID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	item, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemUserMessage,
		Command: &zotigosession.DisplayCommand{
			Type: wire.CommandMessage,
			Text: "already applied",
		},
	})
	if err != nil {
		t.Fatalf("append command item: %v", err)
	}
	_ = item
	if err := os.MkdirAll(filepath.Dir(commandCursorPath(sessionID)), 0755); err != nil {
		t.Fatalf("create cursor dir: %v", err)
	}
	if err := os.WriteFile(commandCursorPath(sessionID), []byte("{"), 0644); err != nil {
		t.Fatalf("write corrupt cursor: %v", err)
	}

	cursor, err := LoadCommandCursor(context.Background(), store, sessionID)
	if err != nil {
		t.Fatalf("load worker command cursor: %v", err)
	}
	if cursor.Sequence != 0 || cursor.Offset != 0 {
		t.Fatalf("expected corrupt cursor recovery not to skip pending message, got %#v", cursor)
	}
}

func TestLoadWorkerCommandCursorRecoversAppliedMessage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	defer store.Close()
	sessionID := "sess-applied-cursor"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        sessionID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	item, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemUserMessage,
		Command: &zotigosession.DisplayCommand{
			Type: wire.CommandMessage,
			Text: "already applied",
		},
	})
	if err != nil {
		t.Fatalf("append command item: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
	}); err != nil {
		t.Fatalf("append turn started: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(commandCursorPath(sessionID)), 0755); err != nil {
		t.Fatalf("create cursor dir: %v", err)
	}
	if err := os.WriteFile(commandCursorPath(sessionID), []byte("{"), 0644); err != nil {
		t.Fatalf("write corrupt cursor: %v", err)
	}

	cursor, err := LoadCommandCursor(context.Background(), store, sessionID)
	if err != nil {
		t.Fatalf("load worker command cursor: %v", err)
	}
	if cursor.Sequence != item.Sequence || cursor.Offset != 0 {
		t.Fatalf("expected recovered applied message sequence %d, got %#v", item.Sequence, cursor)
	}
}

func TestLoadWorkerCommandCursorIgnoresPersistedSteering(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	defer store.Close()
	sessionID := "sess-pending-steering-cursor"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        sessionID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
	}); err != nil {
		t.Fatalf("append turn started: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemSteeringMessage,
		Role: string(protocol.RoleUser),
		Content: []zotigosession.DisplayContentPart{{
			Type: string(protocol.ContentTypeText),
			Text: "still pending",
		}},
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
		Command: &zotigosession.DisplayCommand{
			Type:   wire.CommandSteering,
			Text:   "still pending",
			TurnID: "turn-1",
		},
	}); err != nil {
		t.Fatalf("append steering command: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(commandCursorPath(sessionID)), 0755); err != nil {
		t.Fatalf("create cursor dir: %v", err)
	}
	if err := os.WriteFile(commandCursorPath(sessionID), []byte("{"), 0644); err != nil {
		t.Fatalf("write corrupt cursor: %v", err)
	}

	cursor, err := LoadCommandCursor(context.Background(), store, sessionID)
	if err != nil {
		t.Fatalf("load worker command cursor: %v", err)
	}
	if cursor.Sequence != 0 || cursor.Offset != 0 {
		t.Fatalf("expected steering display item to be ignored, got %#v", cursor)
	}
}

func TestLoadWorkerCommandCursorDoesNotTreatAppliedImageSteeringAsCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	defer store.Close()
	sessionID := "sess-applied-image-steering-cursor"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        sessionID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
	}); err != nil {
		t.Fatalf("append turn started: %v", err)
	}
	_, err = store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemSteeringMessage,
		Role: string(protocol.RoleUser),
		Content: []zotigosession.DisplayContentPart{{
			Type: string(protocol.ContentTypeImage),
			Image: &zotigosession.DisplayMediaPart{
				MediaType: "image/png",
				SizeBytes: 1,
				Width:     1,
				Height:    1,
			},
		}},
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
		Command: &zotigosession.DisplayCommand{
			Type: wire.CommandSteering,
			Images: []zotigosession.DisplayCommandImage{{
				MimeType:  "image/png",
				SizeBytes: 1,
				Width:     1,
				Height:    1,
			}},
			TurnID: "turn-1",
		},
	})
	if err != nil {
		t.Fatalf("append steering command: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnCompleted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
	}); err != nil {
		t.Fatalf("append turn completed: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(commandCursorPath(sessionID)), 0755); err != nil {
		t.Fatalf("create cursor dir: %v", err)
	}
	if err := os.WriteFile(commandCursorPath(sessionID), []byte("{"), 0644); err != nil {
		t.Fatalf("write corrupt cursor: %v", err)
	}

	cursor, err := LoadCommandCursor(context.Background(), store, sessionID)
	if err != nil {
		t.Fatalf("load worker command cursor: %v", err)
	}
	if cursor.Sequence != 0 || cursor.Offset != 0 {
		t.Fatalf("expected applied image steering to stay outside the command cursor, got %#v", cursor)
	}
}

func TestLoadWorkerCommandCursorDowngradesUnsafeSequence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	defer store.Close()
	sessionID := "sess-unsafe-sequence"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        sessionID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemUserMessage,
		Command: &zotigosession.DisplayCommand{
			Type: wire.CommandMessage,
			Text: "pending",
		},
	}); err != nil {
		t.Fatalf("append pending command: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(commandCursorPath(sessionID)), 0755); err != nil {
		t.Fatalf("create cursor dir: %v", err)
	}
	if err := os.WriteFile(commandCursorPath(sessionID), []byte(`{"offset":0,"sequence":999}`), 0644); err != nil {
		t.Fatalf("write corrupt cursor: %v", err)
	}

	cursor, err := LoadCommandCursor(context.Background(), store, sessionID)
	if err != nil {
		t.Fatalf("load worker command cursor: %v", err)
	}
	if cursor.Sequence != 0 || cursor.Offset != 0 {
		t.Fatalf("expected unsafe sequence to recover to pending boundary, got %#v", cursor)
	}
}

func TestLoadWorkerCommandCursorDowngradesInvalidOffset(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	defer store.Close()
	sessionID := "sess-invalid-offset"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        sessionID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	item, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemUserMessage,
		Command: &zotigosession.DisplayCommand{
			Type: wire.CommandMessage,
			Text: "applied",
		},
	})
	if err != nil {
		t.Fatalf("append command: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
	}); err != nil {
		t.Fatalf("append turn started: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(commandCursorPath(sessionID)), 0755); err != nil {
		t.Fatalf("create cursor dir: %v", err)
	}
	if err := os.WriteFile(commandCursorPath(sessionID), []byte(`{"offset":999999,"sequence":1}`), 0644); err != nil {
		t.Fatalf("write corrupt cursor: %v", err)
	}

	cursor, err := LoadCommandCursor(context.Background(), store, sessionID)
	if err != nil {
		t.Fatalf("load worker command cursor: %v", err)
	}
	if cursor.Sequence != item.Sequence || cursor.Offset != 0 {
		t.Fatalf("expected invalid offset to recover to safe sequence %d, got %#v", item.Sequence, cursor)
	}
}

func TestLoadWorkerCommandCursorDowngradesOffsetBeforePendingCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	defer store.Close()
	sessionID := "sess-pending-offset"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        sessionID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemUserMessage,
		Command: &zotigosession.DisplayCommand{
			Type: wire.CommandMessage,
			Text: "pending",
		},
	}); err != nil {
		t.Fatalf("append pending command: %v", err)
	}
	_, _, eofOffset, err := store.ListDisplayItemsFromOffset(context.Background(), sessionID, 0, 10)
	if err != nil {
		t.Fatalf("read display log offset: %v", err)
	}
	if eofOffset <= 0 {
		t.Fatalf("expected positive display log offset, got %d", eofOffset)
	}
	if err := os.MkdirAll(filepath.Dir(commandCursorPath(sessionID)), 0755); err != nil {
		t.Fatalf("create cursor dir: %v", err)
	}
	cursorData := fmt.Sprintf(`{"offset":%d,"sequence":0}`, eofOffset)
	if err := os.WriteFile(commandCursorPath(sessionID), []byte(cursorData), 0644); err != nil {
		t.Fatalf("write cursor: %v", err)
	}

	cursor, err := LoadCommandCursor(context.Background(), store, sessionID)
	if err != nil {
		t.Fatalf("load worker command cursor: %v", err)
	}
	if cursor.Sequence != 0 || cursor.Offset != 0 {
		t.Fatalf("expected offset downgrade before pending command, got %#v", cursor)
	}
}

func TestRecoverWorkerCommandCursorTracksDurableSteeringApplication(t *testing.T) {
	items := []zotigosession.DisplayItem{
		{Sequence: 1, Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
		{ID: "steer-1", Sequence: 2, Type: zotigosession.DisplayItemSessionCommand, Command: &zotigosession.DisplayCommand{Type: wire.CommandSteering, Text: "change", TurnID: "turn-1"}},
	}
	if got := RecoverAppliedCommandSequence(items); got != 0 {
		t.Fatalf("pending steering advanced cursor to %d", got)
	}
	items = append(items, zotigosession.DisplayItem{
		ID: "steer-1", Sequence: 3, Type: zotigosession.DisplayItemSteeringMessage,
		Command: &zotigosession.DisplayCommand{Type: wire.CommandSteering, Text: "change", TurnID: "turn-1"},
	})
	if got := RecoverAppliedCommandSequence(items); got != 2 {
		t.Fatalf("applied steering cursor = %d, want 2", got)
	}
}

func TestRecoverAppliedCommandSequenceWaitsForProfileResult(t *testing.T) {
	command := zotigosession.DisplayItem{
		ID:       "item-profile",
		Sequence: 1,
		Type:     zotigosession.DisplayItemSessionCommand,
		Command:  &zotigosession.DisplayCommand{Type: wire.CommandProfile, Profile: "new"},
	}
	if cursor := RecoverAppliedCommandSequence([]zotigosession.DisplayItem{command}); cursor != 0 {
		t.Fatalf("expected pending profile command cursor 0, got %d", cursor)
	}
	failed := zotigosession.DisplayItem{
		Sequence: 2,
		Type:     zotigosession.DisplayItemProfileFailed,
		Profile:  &zotigosession.DisplayProfileChange{CommandID: command.ID, To: "new"},
	}
	if cursor := RecoverAppliedCommandSequence([]zotigosession.DisplayItem{command, failed}); cursor != command.Sequence {
		t.Fatalf("expected failed profile command cursor %d, got %d", command.Sequence, cursor)
	}
}

func newTestStoreRoot(t *testing.T) string {
	t.Helper()
	return catalogtest.NewRoot(t)
}
