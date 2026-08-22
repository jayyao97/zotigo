package zotigod

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/codexapp"
)

type codexWorkerRPC struct {
	methods        []string
	resumeApproval string
	err            error
}

func TestCodexWorkerCloseInterruptsActiveTurnInDisplayLog(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "session-1", WorkingDirectory: t.TempDir(), CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	rpc := &codexWorkerRPC{}
	runtime := &codexWorkerRuntime{
		cfg:   codexWorkerConfig{workerClientConfig: workerClientConfig{SessionID: "session-1"}},
		store: store, app: rpc, threadID: "thread-1", activeTurnID: "turn-1", turnStarted: now,
		messages: map[string]string{"message-1": "partial"}, messageOrder: []string{"message-1"},
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[1].Type != zotigosession.DisplayItemTurnInterrupted || items[1].Turn == nil || items[1].Turn.ID != "turn-1" {
		t.Fatalf("display items = %#v", items)
	}
	if got := fmt.Sprint(rpc.methods); got != "[turn/interrupt]" {
		t.Fatalf("methods = %s", got)
	}
}

func (r *codexWorkerRPC) Call(_ context.Context, method string, params any, result any) error {
	r.methods = append(r.methods, method)
	request := params.(map[string]any)
	if method == "thread/resume" {
		r.resumeApproval, _ = request["approvalPolicy"].(string)
	}
	return r.err
}

func TestResumeCodexThreadClassifiesAnotherAppOwnership(t *testing.T) {
	rpc := &codexWorkerRPC{err: &codexapp.RPCError{Code: -32600, Message: "thread thread-1 already has an active writer"}}
	err := resumeCodexThread(context.Background(), rpc, codexWorkerConfig{
		ThreadID: "thread-1", WorkingDirectory: t.TempDir(), Model: "gpt-5.6-luna",
	})
	if !errors.Is(err, errRuntimeOccupied) {
		t.Fatalf("resume error = %v, want runtime occupied", err)
	}
}

func (*codexWorkerRPC) Notify(string, any) error { return nil }

func TestResumeCodexThreadUsesCWDWithoutUpdatingProject(t *testing.T) {
	rpc := &codexWorkerRPC{}
	err := resumeCodexThread(context.Background(), rpc, codexWorkerConfig{
		ThreadID: "thread-1", WorkingDirectory: t.TempDir(), Model: "gpt-5.6-luna",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(rpc.methods); got != "[thread/resume]" {
		t.Fatalf("methods = %s", got)
	}
	if rpc.resumeApproval != "never" {
		t.Fatalf("resume approval policy = %q", rpc.resumeApproval)
	}
}
