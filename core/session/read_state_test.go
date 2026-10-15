package session

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/jayyao97/zotigo/core/catalogschema"
	"github.com/jayyao97/zotigo/internal/testutil/catalogtest"
)

func TestReadStateMigrationBaselinesExistingHistory(t *testing.T) {
	root := catalogtest.NewRoot(t)
	store, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := NewManagerWithStore(store).CreateNew(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AppendDisplayItem(t.Context(), sess.ID, DisplayItem{Type: DisplayItemTurnCompleted}); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "catalog.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []uint{13, catalogschema.Version} {
		if err = catalogschema.MigrateOffline(t.Context(), db, root, version); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.InvalidateDisplayIndex(t.Context(), sess.ID); err != nil {
		t.Fatal(err)
	}
	if err = store.RebuildDisplayIndex(t.Context()); err != nil {
		t.Fatal(err)
	}
	state, err := store.SessionReadState(t.Context(), sess.ID)
	if err != nil || state.Unread || state.Version != 0 {
		t.Fatalf("old history became unread: %+v %v", state, err)
	}
	if _, err = store.AppendDisplayItem(t.Context(), sess.ID, DisplayItem{Type: DisplayItemTurnCompleted}); err != nil {
		t.Fatal(err)
	}
	state, err = store.SessionReadState(t.Context(), sess.ID)
	if err != nil || !state.Unread || state.Version != 1 {
		t.Fatalf("new completion missing: %+v %v", state, err)
	}
}

func TestReadStatePersistsCompletionAndRejectsStaleAcknowledgement(t *testing.T) {
	root := catalogtest.NewRoot(t)
	store, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sess, err := NewManagerWithStore(store).CreateNew(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	appendItem := func(kind DisplayItemType) DisplayItem {
		t.Helper()
		item, err := store.AppendDisplayItem(ctx, sess.ID, DisplayItem{Type: kind})
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	appendItem(DisplayItemAssistantMessage)
	state, err := store.SessionReadState(ctx, sess.ID)
	if err != nil || state.Unread {
		t.Fatalf("unfinished turn: %+v %v", state, err)
	}
	first := appendItem(DisplayItemTurnCompleted)
	state, err = store.SessionReadState(ctx, sess.ID)
	if err != nil || !state.Unread || state.Version != 1 || state.CompletedSequence != int64(first.Sequence) {
		t.Fatalf("completed: %+v %v", state, err)
	}
	seenVersion := state.Version
	appendItem(DisplayItemTurnFailed)
	state, err = store.SetSessionReadState(ctx, sess.ID, &seenVersion)
	if err != nil || !state.Unread || state.Version != 2 {
		t.Fatalf("stale ack cleared newer completion: %+v %v", state, err)
	}
	seenVersion = state.Version
	state, err = store.SetSessionReadState(ctx, sess.ID, &seenVersion)
	if err != nil || state.Unread {
		t.Fatalf("read: %+v %v", state, err)
	}
	state, err = store.SetSessionReadState(ctx, sess.ID, nil)
	if err != nil || !state.Unread || state.Version != 3 {
		t.Fatalf("manual unread: %+v %v", state, err)
	}
	state, err = store.SetSessionReadState(ctx, sess.ID, &seenVersion)
	if err != nil || !state.Unread {
		t.Fatalf("stale ack cleared manual unread: %+v %v", state, err)
	}
	seenVersion = state.Version
	state, err = store.SetSessionReadState(ctx, sess.ID, nil)
	if err != nil || state.Version != seenVersion+1 {
		t.Fatalf("manual unread must invalidate in-flight reads: %+v %v", state, err)
	}
	state, err = store.SetSessionReadState(ctx, sess.ID, &seenVersion)
	if err != nil || !state.Unread {
		t.Fatalf("in-flight read erased explicit unread: %+v %v", state, err)
	}
	if err := store.InvalidateDisplayIndex(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RebuildDisplayIndex(ctx); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := store.SessionReadState(ctx, sess.ID)
	if err != nil || rebuilt != state {
		t.Fatalf("rebuild changed read state: %+v %v", rebuilt, err)
	}
	other, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	states, err := other.ListSessionReadStates(ctx)
	if err != nil || states[sess.ID] != state {
		t.Fatalf("other client: %+v %v", states, err)
	}
	seenVersion = state.Version
	if _, err := other.SetSessionReadState(ctx, sess.ID, &seenVersion); err != nil {
		t.Fatal(err)
	}
	state, err = store.SessionReadState(ctx, sess.ID)
	if err != nil || state.Unread {
		t.Fatalf("cross-client ack: %+v %v", state, err)
	}
	appendItem(DisplayItemTurnInterrupted)
	state, err = other.SessionReadState(ctx, sess.ID)
	if err != nil || !state.Unread {
		t.Fatalf("interrupted turn: %+v %v", state, err)
	}
	if err := store.Delete(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	states, err = other.ListSessionReadStates(ctx)
	if err != nil || len(states) != 0 {
		t.Fatalf("deleted session left read state: %+v %v", states, err)
	}
}
