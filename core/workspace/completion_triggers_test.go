package workspace

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/catalogschema"
	"github.com/jayyao97/zotigo/internal/testutil/catalogtest"
)

func TestCompletionTriggerPersistenceAndSingleDelivery(t *testing.T) {
	ctx := context.Background()
	root := catalogtest.NewRoot(t)
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	value := CompletionTrigger{ID: "cb_one", SessionID: "s", WorkspaceID: "w", Title: "Build", OnFire: "Read the build result", TTL: 86400, ExpiresAt: time.Now().Add(time.Hour)}
	created, token, err := store.CreateCompletionTrigger(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	value.ExpiresAt = time.Now().Add(2 * time.Hour)
	retry, token2, err := store.CreateCompletionTrigger(ctx, value)
	if err != nil || token != token2 || !created.ExpiresAt.Equal(retry.ExpiresAt) {
		t.Fatalf("retry changed receipt: %+v %v", retry, err)
	}
	if _, err := store.AuthenticateCompletionTrigger(ctx, value.ID, "wrong"); !errors.Is(err, ErrTriggerUnavailable) {
		t.Fatalf("bad token: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.AuthenticateCompletionTrigger(ctx, value.ID, token); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			if _, err := store.FireCompletionTrigger(ctx, value.ID, "done", []byte(`{"text":"done"}`)); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	if _, err := store.FireCompletionTrigger(ctx, value.ID, "different", []byte(`{}`)); !errors.Is(err, ErrTriggerContentConflict) {
		t.Fatalf("changed retry: %v", err)
	}
	if value, err := store.CompletionTrigger(ctx, value.ID); err != nil || value.CallbackText != "done" {
		t.Fatalf("lost callback text: %+v %v", value, err)
	}
	rows, err := store.QueuedInputs(ctx, "s")
	if err != nil || len(rows) != 1 || rows[0].ID != "completion:cb_one" {
		t.Fatalf("delivery: %+v %v", rows, err)
	}
	if err := store.RecoverQueuedInputs(ctx); err != nil {
		t.Fatal(err)
	}
	if rows, err := store.ClaimQueuedInputs(ctx, "s", "new", false); err != nil || len(rows) != 0 {
		t.Fatalf("automatic restart delivery: %v %v", rows, err)
	}
	rows, err = store.ClaimQueuedInputs(ctx, "s", "new", true)
	if err != nil || len(rows) != 1 || rows[0].BatchID != rows[0].ID {
		t.Fatalf("resume: %v %v", rows, err)
	}
}

func TestCompletionTriggerExpiryCancelQuotaAndRollback(t *testing.T) {
	ctx := context.Background()
	store, err := Open(catalogtest.NewRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i := range 16 {
		value := CompletionTrigger{ID: fmt.Sprint(i), SessionID: "s", ExpiresAt: time.Now().Add(time.Hour)}
		if _, _, err := store.CreateCompletionTrigger(ctx, value); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := store.CreateCompletionTrigger(ctx, CompletionTrigger{ID: "excess", SessionID: "s", ExpiresAt: time.Now().Add(time.Hour)}); err == nil {
		t.Fatal("quota ignored")
	}
	if err := store.CancelCompletionTrigger(ctx, "other", "0"); !errors.Is(err, ErrTriggerUnavailable) {
		t.Fatalf("cross-session cancellation: %v", err)
	}
	if err := store.CancelCompletionTrigger(ctx, "s", "0"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FireCompletionTrigger(ctx, "0", "", nil); !errors.Is(err, ErrTriggerUnavailable) {
		t.Fatalf("cancelled: %v", err)
	}
	if _, _, err := store.CreateCompletionTrigger(ctx, CompletionTrigger{ID: "expired", SessionID: "s", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FireCompletionTrigger(ctx, "expired", "", nil); !errors.Is(err, ErrTriggerUnavailable) {
		t.Fatalf("expired: %v", err)
	}
	if err := store.QueueInput(ctx, "s", "completion:1", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FireCompletionTrigger(ctx, "1", "", []byte(`{}`)); err == nil {
		t.Fatal("expected queue conflict")
	}
	value, err := store.CompletionTrigger(ctx, "1")
	if err != nil || value.State != "pending" {
		t.Fatalf("failed insert consumed callback: %v %v", value, err)
	}
	if err := catalogschema.Migrate(ctx, store.db, 15); err != nil {
		t.Fatal(err)
	}
	if err := catalogschema.Migrate(ctx, store.db, catalogschema.Version); err != nil {
		t.Fatal(err)
	}
	values, err := store.ListCompletionTriggers(ctx, "s")
	if err != nil || len(values) != 0 {
		t.Fatalf("migration: %v %v", values, err)
	}
}

func TestCompletionQueuePreservesOrderAndAuthority(t *testing.T) {
	ctx := context.Background()
	store, err := Open(catalogtest.NewRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, id := range []string{"Completion:user", "user1", "user2", "completion:one", "completion:two", "user3"} {
		if err := store.QueueInput(ctx, "s", id, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range [][]string{{"Completion:user", "user1", "user2"}, {"completion:one"}, {"completion:two"}, {"user3"}} {
		rows, err := store.ClaimQueuedInputs(ctx, "s", fmt.Sprint(i), false)
		if err != nil || len(rows) != len(want) {
			t.Fatalf("batch: %v %v", rows, err)
		}
		for j, row := range rows {
			if row.ID != want[j] {
				t.Fatalf("order: %v", rows)
			}
		}
		if err := store.FinishQueuedBatch(ctx, "s", rows[0].BatchID, true); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCompletionCleanupRetainsFiredAuthority(t *testing.T) {
	ctx := context.Background()
	store, err := Open(catalogtest.NewRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, id := range []string{"paused", "sent"} {
		_, _, err := store.CreateCompletionTrigger(ctx, CompletionTrigger{ID: id, SessionID: id, ExpiresAt: time.Now().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.FireCompletionTrigger(ctx, id, "", []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec(`UPDATE session_input_queue SET state=CASE WHEN session_id='paused' THEN 'paused' ELSE 'sent' END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE completion_triggers SET expires_at=?`, time.Now().Add(-8*24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateCompletionTrigger(ctx, CompletionTrigger{ID: "new", SessionID: "new", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"paused", "sent"} {
		if value, err := store.CompletionTrigger(ctx, id); err != nil || value.State != "fired" {
			t.Fatalf("lost replay authority for %s: %+v %v", id, value, err)
		}
	}
}
