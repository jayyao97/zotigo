package zotigod

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
)

type codexWorkerRPC struct {
	methods        []string
	resumeApproval string
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
	projectID := "old-project"
	if method == "thread/metadata/update" {
		projectID, _ = request["projectId"].(string)
	}
	payload, _ := json.Marshal(map[string]any{"thread": map[string]any{"projectId": projectID}})
	return json.Unmarshal(payload, result)
}

func (*codexWorkerRPC) Notify(string, any) error { return nil }

func TestResumeCodexThreadReassignsAndConfirmsProject(t *testing.T) {
	rpc := &codexWorkerRPC{}
	err := resumeCodexThread(context.Background(), rpc, codexWorkerConfig{
		ThreadID: "thread-1", ProjectID: "project-1", WorkingDirectory: t.TempDir(), Model: "gpt-5.6-luna",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(rpc.methods); got != "[thread/resume thread/metadata/update]" {
		t.Fatalf("methods = %s", got)
	}
	if rpc.resumeApproval != "never" {
		t.Fatalf("resume approval policy = %q", rpc.resumeApproval)
	}
}
