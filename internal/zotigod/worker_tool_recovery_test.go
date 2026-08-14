package zotigod

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

func TestWorkerToolExecutionWaitsForDurableCallBeforeInvokingTool(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog("sess-tool-order", source)
	ctx := context.Background()
	if _, err := display.StartTurn(ctx); err != nil {
		t.Fatalf("start turn: %v", err)
	}

	executed := make(chan struct{})
	completed := make(chan error, 1)
	recorder := &workerDurabilityRecorder{display: display}
	invoke := func(ctx context.Context, call *agent.ToolCall) (any, error) {
		if err := recorder.RecordToolExecutionStarted(ctx, call); err != nil {
			return nil, err
		}
		items, _, err := source.LoadItems(ctx, "sess-tool-order")
		if err != nil {
			return nil, err
		}
		if len(items) != 3 || items[1].Content[0].ToolCall == nil || items[2].Type != zotigosession.DisplayItemToolExecutionStarted {
			return nil, errors.New("tool invoked before durable call and execution marker")
		}
		close(executed)
		return "ok", nil
	}
	go func() {
		_, err := invoke(ctx, &agent.ToolCall{ToolCallID: "call-1", Name: "shell"})
		completed <- err
	}()

	waitForToolCallWaiter(t, display, "call-1")
	select {
	case <-executed:
		t.Fatal("tool executed before tool_call was persisted")
	default:
	}
	if err := display.HandleEvent(ctx, protocol.Event{
		Type:     protocol.EventTypeToolCallEnd,
		ToolCall: &protocol.ToolCall{ID: "call-1", Name: "shell", Arguments: `{"command":"touch marker"}`},
	}); err != nil {
		t.Fatalf("persist tool call: %v", err)
	}
	select {
	case err := <-completed:
		if err != nil {
			t.Fatalf("invoke tool: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("tool did not run after durable tool_call")
	}
}

func TestWorkerToolExecutionDoesNotInvokeWhenStartedMarkerFails(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog("sess-tool-marker-failure", source)
	ctx := context.Background()
	if _, err := display.StartTurn(ctx); err != nil {
		t.Fatalf("start turn: %v", err)
	}
	if err := display.HandleEvent(ctx, protocol.Event{
		Type:     protocol.EventTypeToolCallEnd,
		ToolCall: &protocol.ToolCall{ID: "call-1", Name: "shell", Arguments: `{}`},
	}); err != nil {
		t.Fatalf("persist tool call: %v", err)
	}
	display.items = failingDisplayItemSource{err: errors.New("disk full")}
	executed := false
	recorder := &workerDurabilityRecorder{display: display}
	invoke := func(ctx context.Context, call *agent.ToolCall) (any, error) {
		if err := recorder.RecordToolExecutionStarted(ctx, call); err != nil {
			return nil, err
		}
		executed = true
		return nil, nil
	}
	if _, err := invoke(ctx, &agent.ToolCall{ToolCallID: "call-1", Name: "shell"}); err == nil {
		t.Fatal("expected durable marker failure")
	}
	if executed {
		t.Fatal("tool executed despite durable marker failure")
	}
}

func TestRecoverInterruptedToolExecutionInjectsOutcomeUnknownHintOnce(t *testing.T) {
	store, sessionID := newToolRecoveryStore(t)
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
	})
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemAssistantMessage,
		Content: []zotigosession.DisplayContentPart{{
			Type:     string(protocol.ContentTypeToolCall),
			ToolCall: &zotigosession.DisplayToolCall{ID: "call-1", Name: "shell", Arguments: `{"command":"touch marker"}`},
		}},
	})
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{
		Type:          zotigosession.DisplayItemToolExecutionStarted,
		ToolExecution: &zotigosession.DisplayToolExecution{TurnID: "turn-1", ToolCallID: "call-1", ToolName: "shell"},
	})
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnInterrupted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1", Status: "interrupted", Reason: controlChannelClosedReason},
	})

	sess, err := store.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if err := recoverInterruptedToolExecutions(context.Background(), store, sess); err != nil {
		t.Fatalf("recover interrupted tool: %v", err)
	}
	stored, err := store.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	if len(stored.AgentSnapshot.History) != 1 {
		t.Fatalf("recovery history = %#v", stored.AgentSnapshot.History)
	}
	hint := stored.AgentSnapshot.History[0].String()
	if !historyHasToolRecovery(stored.AgentSnapshot.History, "turn-1", "call-1") || !containsAll(hint, "outcomes are unknown", "Do not repeat", "touch marker") {
		t.Fatalf("recovery hint = %q", hint)
	}
	if err := recoverInterruptedToolExecutions(context.Background(), store, stored); err != nil {
		t.Fatalf("repeat recovery: %v", err)
	}
	stored, err = store.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("reload repeated recovery: %v", err)
	}
	if len(stored.AgentSnapshot.History) != 1 {
		t.Fatalf("recovery hint duplicated: %#v", stored.AgentSnapshot.History)
	}
}

func TestRecoverInterruptedToolExecutionIgnoresDurableResult(t *testing.T) {
	store, sessionID := newToolRecoveryStore(t)
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}})
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{
		Type:          zotigosession.DisplayItemToolExecutionStarted,
		ToolExecution: &zotigosession.DisplayToolExecution{TurnID: "turn-1", ToolCallID: "call-1", ToolName: "read_file"},
	})
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemAssistantMessage,
		Content: []zotigosession.DisplayContentPart{{
			Type:       string(protocol.ContentTypeToolResult),
			ToolResult: &zotigosession.DisplayToolResult{ToolCallID: "call-1", ToolName: "read_file", Text: "done"},
		}},
	})
	sess, err := store.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if err := recoverInterruptedToolExecutions(context.Background(), store, sess); err != nil {
		t.Fatalf("recover completed tool: %v", err)
	}
	if len(sess.AgentSnapshot.History) != 0 {
		t.Fatalf("completed tool received recovery hint: %#v", sess.AgentSnapshot.History)
	}
}

func TestRecoverUnansweredToolCallUsesDurableDisplayResult(t *testing.T) {
	store, sessionID := newToolRecoveryStore(t)
	assistant := protocol.NewAssistantMessage("")
	assistant.AddToolCall(protocol.ToolCall{ID: "call-1", Name: "read_file", Arguments: `{}`})
	sess, err := store.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	sess.AgentSnapshot.History = []protocol.Message{protocol.NewUserMessage("read"), assistant}
	if err := store.Put(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}})
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{
		Type:          zotigosession.DisplayItemToolExecutionStarted,
		ToolExecution: &zotigosession.DisplayToolExecution{TurnID: "turn-1", ToolCallID: "call-1", ToolName: "read_file"},
	})
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemAssistantMessage,
		Content: []zotigosession.DisplayContentPart{{
			Type:       string(protocol.ContentTypeToolResult),
			ToolResult: &zotigosession.DisplayToolResult{ToolCallID: "call-1", ToolName: "read_file", Text: "durable result"},
		}},
	})
	if err := recoverUnansweredToolCalls(context.Background(), store, sess); err != nil {
		t.Fatal(err)
	}
	last := sess.AgentSnapshot.History[len(sess.AgentSnapshot.History)-1]
	if last.Content[0].ToolResult == nil || last.Content[0].ToolResult.Text != "durable result" {
		t.Fatalf("display result was not restored: %#v", last)
	}
}

func TestRecoverInterruptedApprovedToolUsesMatchingToolResults(t *testing.T) {
	store, sessionID := newToolRecoveryStore(t)
	sess, err := store.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	assistant := protocol.NewAssistantMessage("")
	assistant.AddToolCall(protocol.ToolCall{ID: "call-unknown", Name: "shell", Arguments: `{"command":"touch marker"}`})
	assistant.AddToolCall(protocol.ToolCall{ID: "call-not-started", Name: "read_file", Arguments: `{}`})
	sess.AgentSnapshot.State = agent.StatePaused
	sess.AgentSnapshot.History = []protocol.Message{protocol.NewUserMessage("do it"), assistant}
	sess.AgentSnapshot.PendingActions = []*agent.PendingAction{{ToolCallID: "call-unknown", Name: "shell"}}
	if err := store.Put(context.Background(), sess); err != nil {
		t.Fatalf("save paused snapshot: %v", err)
	}
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}})
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{
		Type:          zotigosession.DisplayItemToolExecutionStarted,
		ToolExecution: &zotigosession.DisplayToolExecution{TurnID: "turn-1", ToolCallID: "call-unknown", ToolName: "shell"},
	})

	sess, err = store.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("reload paused snapshot: %v", err)
	}
	if err := recoverInterruptedToolExecutions(context.Background(), store, sess); err != nil {
		t.Fatalf("recover approved tool: %v", err)
	}
	if sess.AgentSnapshot.State != agent.StateIdle || len(sess.AgentSnapshot.PendingActions) != 0 || len(sess.AgentSnapshot.DeferredActions) != 0 {
		t.Fatalf("recovered snapshot remained executable: %#v", sess.AgentSnapshot)
	}
	if len(sess.AgentSnapshot.History) != 3 || sess.AgentSnapshot.History[2].Role != protocol.RoleTool {
		t.Fatalf("recovery did not append a tool message: %#v", sess.AgentSnapshot.History)
	}
	results := make([]protocol.ToolResult, 0, 2)
	for _, part := range sess.AgentSnapshot.History[2].Content {
		if part.ToolResult != nil {
			results = append(results, *part.ToolResult)
		}
	}
	if len(results) != 2 || results[0].ToolCallID != "call-unknown" || results[1].ToolCallID != "call-not-started" {
		t.Fatalf("recovery results do not match assistant calls: %#v", results)
	}
	if !strings.Contains(results[0].Text, "outcome is unknown") || !strings.Contains(results[1].Text, "was not executed") {
		t.Fatalf("recovery result semantics = %#v", results)
	}
}

func TestRecoverInterruptedToolDoesNotReuseResultFromEarlierTurn(t *testing.T) {
	store, sessionID := newToolRecoveryStore(t)
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}})
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemAssistantMessage,
		Content: []zotigosession.DisplayContentPart{{
			Type:     string(protocol.ContentTypeToolCall),
			ToolCall: &zotigosession.DisplayToolCall{ID: "call-reused", Name: "shell", Arguments: `{"command":"first"}`},
		}},
	})
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemAssistantMessage,
		Content: []zotigosession.DisplayContentPart{{
			Type:       string(protocol.ContentTypeToolResult),
			ToolResult: &zotigosession.DisplayToolResult{ToolCallID: "call-reused", ToolName: "shell", ResultType: string(protocol.ToolResultTypeText), Text: "first completed"},
		}},
	})
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{Type: zotigosession.DisplayItemTurnCompleted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}})
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-2"}})
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemAssistantMessage,
		Content: []zotigosession.DisplayContentPart{{
			Type:     string(protocol.ContentTypeToolCall),
			ToolCall: &zotigosession.DisplayToolCall{ID: "call-reused", Name: "shell", Arguments: `{"command":"second"}`},
		}},
	})
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{
		Type:          zotigosession.DisplayItemToolExecutionStarted,
		ToolExecution: &zotigosession.DisplayToolExecution{TurnID: "turn-2", ToolCallID: "call-reused", ToolName: "shell"},
	})
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{Type: zotigosession.DisplayItemTurnInterrupted, Turn: &zotigosession.DisplayTurn{ID: "turn-2"}})

	sess, err := store.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if err := recoverInterruptedToolExecutions(context.Background(), store, sess); err != nil {
		t.Fatalf("recover reused call id: %v", err)
	}
	hint := sess.AgentSnapshot.History[len(sess.AgentSnapshot.History)-1]
	if hint.Role != protocol.RoleUser || !strings.Contains(hint.String(), "outcomes are unknown") || strings.Contains(hint.String(), "first completed") {
		t.Fatalf("earlier result was reused for interrupted turn: %#v", hint)
	}
	if !historyHasToolRecovery(sess.AgentSnapshot.History, "turn-2", "call-reused") {
		t.Fatalf("recovery marker was not scoped to turn 2: %#v", sess.AgentSnapshot.History)
	}
}

func TestRecoverInterruptedToolDoesNotAppendNonAdjacentToolResult(t *testing.T) {
	store, sessionID := newToolRecoveryStore(t)
	sess, err := store.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	oldAssistant := protocol.NewAssistantMessage("")
	oldAssistant.AddToolCall(protocol.ToolCall{ID: "call-reused", Name: "shell", Arguments: `{}`})
	oldResult := protocol.NewTextToolResult("call-reused", "old result", false)
	sess.AgentSnapshot.State = agent.StatePaused
	sess.AgentSnapshot.History = []protocol.Message{oldAssistant, protocol.NewToolMessage([]protocol.ToolResult{oldResult})}
	if err := store.Put(context.Background(), sess); err != nil {
		t.Fatalf("save existing tool result: %v", err)
	}
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-2"}})
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{
		Type:          zotigosession.DisplayItemToolExecutionStarted,
		ToolExecution: &zotigosession.DisplayToolExecution{TurnID: "turn-2", ToolCallID: "call-reused", ToolName: "shell"},
	})

	sess, err = store.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	if err := recoverInterruptedToolExecutions(context.Background(), store, sess); err != nil {
		t.Fatalf("recover after existing tool result: %v", err)
	}
	if len(sess.AgentSnapshot.History) != 3 || sess.AgentSnapshot.History[2].Role != protocol.RoleUser {
		t.Fatalf("recovery appended a duplicate or non-adjacent tool message: %#v", sess.AgentSnapshot.History)
	}
}

func TestRecoverInterruptedToolIgnoresOlderUnknownAfterNewerCompletedTurn(t *testing.T) {
	store, sessionID := newToolRecoveryStore(t)
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-unknown"}})
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemAssistantMessage,
		Content: []zotigosession.DisplayContentPart{{
			Type:     string(protocol.ContentTypeToolCall),
			ToolCall: &zotigosession.DisplayToolCall{ID: "call-old", Name: "shell", Arguments: `{}`},
		}},
	})
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{
		Type:          zotigosession.DisplayItemToolExecutionStarted,
		ToolExecution: &zotigosession.DisplayToolExecution{TurnID: "turn-unknown", ToolCallID: "call-old", ToolName: "shell"},
	})
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{Type: zotigosession.DisplayItemTurnInterrupted, Turn: &zotigosession.DisplayTurn{ID: "turn-unknown"}})
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-new"}})
	appendToolRecoveryItem(t, store, sessionID, zotigosession.DisplayItem{Type: zotigosession.DisplayItemTurnCompleted, Turn: &zotigosession.DisplayTurn{ID: "turn-new"}})

	sess, err := store.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	// An empty history models compaction having removed the exact textual
	// recovery marker after the user successfully continued the session.
	sess.AgentSnapshot.History = nil
	if err := recoverInterruptedToolExecutions(context.Background(), store, sess); err != nil {
		t.Fatalf("recover after newer completed turn: %v", err)
	}
	if len(sess.AgentSnapshot.History) != 0 {
		t.Fatalf("older unknown execution was injected again: %#v", sess.AgentSnapshot.History)
	}
}

func waitForToolCallWaiter(t *testing.T, display *workerDisplayLog, toolCallID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		display.mu.Lock()
		_, ok := display.toolCalls[toolCallID]
		display.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("tool execution did not wait for durable tool_call")
		}
		time.Sleep(time.Millisecond)
	}
}

func newToolRecoveryStore(t *testing.T) (*zotigosession.FileStore, string) {
	t.Helper()
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const sessionID = "sess-tool-recovery"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata:      zotigosession.Metadata{ID: sessionID, CreatedAt: now, UpdatedAt: now},
		AgentSnapshot: agent.Snapshot{State: agent.StateIdle, CreatedAt: now},
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return store, sessionID
}

func appendToolRecoveryItem(t *testing.T, store *zotigosession.FileStore, sessionID string, item zotigosession.DisplayItem) {
	t.Helper()
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, item); err != nil {
		t.Fatalf("append display item: %v", err)
	}
}

func containsAll(text string, values ...string) bool {
	for _, value := range values {
		if !strings.Contains(text, value) {
			return false
		}
	}
	return true
}
