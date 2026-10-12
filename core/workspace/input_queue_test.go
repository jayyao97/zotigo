package workspace

import (
	"context"
	"errors"
	"testing"

	"github.com/jayyao97/zotigo/internal/testutil/catalogtest"
)

func TestInputQueuePersistenceAndClaims(t *testing.T) {
	root := catalogtest.NewRoot(t)
	ctx := context.Background()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if err := store.QueueInput(ctx, "s", id, []byte(`{"text":"`+id+`"}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.QueueInput(ctx, "s", "a", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateQueuedInput(ctx, "s", "a", 1, []byte(`{"text":"edited"}`), false); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateQueuedInput(ctx, "s", "a", 1, []byte(`{}`), false); !errors.Is(err, ErrQueueConflict) {
		t.Fatalf("stale edit: %v", err)
	}
	if err := store.UpdateQueuedInput(ctx, "s", "c", 1, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.RecoverQueuedInputs(ctx); err != nil {
		t.Fatal(err)
	}
	if rows, err := store.ClaimQueuedInputs(ctx, "s", "auto", false); err != nil || len(rows) != 0 {
		t.Fatalf("restart auto dispatch: %v %v", rows, err)
	}
	rows, err := store.ClaimQueuedInputs(ctx, "s", "batch", true)
	if err != nil || len(rows) != 2 || rows[0].ID != "a" || rows[1].ID != "b" {
		t.Fatalf("claim: %v %v", rows, err)
	}
	if string(rows[0].Payload) != `{"text":"edited"}` {
		t.Fatal(string(rows[0].Payload))
	}
	if err := store.UpdateQueuedInput(ctx, "s", "a", rows[0].Revision, nil, true); !errors.Is(err, ErrQueueConflict) {
		t.Fatalf("delete claimed: %v", err)
	}
	if err := store.QueueInput(ctx, "s", "d", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishQueuedBatch(ctx, "s", "batch", false); err != nil {
		t.Fatal(err)
	}
	if err := store.RecoverQueuedInputs(ctx); err != nil {
		t.Fatal(err)
	}
	retry, err := store.ClaimQueuedInputs(ctx, "s", "new-batch", true)
	if err != nil || len(retry) != 2 || retry[0].BatchID != "batch" {
		t.Fatalf("retry changed batch: %v %v", retry, err)
	}
	if err := store.FinishQueuedBatch(ctx, "s", "batch", true); err != nil {
		t.Fatal(err)
	}
	rows, err = store.ClaimQueuedInputs(ctx, "s", "next", false)
	if err != nil || len(rows) != 1 || rows[0].ID != "d" {
		t.Fatalf("next: %v %v", rows, err)
	}
}
