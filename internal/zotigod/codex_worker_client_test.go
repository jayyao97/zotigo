package zotigod

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/codexapp"
	"github.com/jayyao97/zotigo/internal/hooks"
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

func TestCodexWorkerCloseCompletesPendingToolAsInterrupted(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "session-pending", CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), "session-pending", zotigosession.DisplayItem{
		ID: "tool-1", Type: zotigosession.DisplayItemAssistantMessage, Role: string(protocol.RoleAssistant),
		Content: []zotigosession.DisplayContentPart{{Type: "tool_call", ToolCall: &zotigosession.DisplayToolCall{ID: "tool-1", Name: "shell"}}},
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &codexWorkerRuntime{
		cfg: codexWorkerConfig{workerClientConfig: workerClientConfig{SessionID: "session-pending"}}, store: store, app: &codexWorkerRPC{},
		threadID: "thread-1", activeTurnID: "turn-1", turnStarted: now, messages: make(map[string]string),
		toolNames: map[string]string{"tool-1": "shell"}, toolOrder: []string{"tool-1"},
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-pending")
	if err != nil || len(items) != 3 {
		t.Fatalf("display items = %#v, err=%v", items, err)
	}
	result := items[1].Content[0].ToolResult
	if result == nil || result.ToolCallID != "tool-1" || !result.IsError || result.Reason != "codex worker disconnected" {
		t.Fatalf("interrupted tool result = %#v", items[1])
	}
	if items[2].Type != zotigosession.DisplayItemTurnInterrupted {
		t.Fatalf("terminal item = %#v", items[2])
	}
}

func TestCodexWorkerPersistsContextUsageNotification(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{ID: "session-usage", CreatedAt: now, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	runtime := &codexWorkerRuntime{cfg: codexWorkerConfig{workerClientConfig: workerClientConfig{SessionID: "session-usage"}}, store: store, threadID: "thread-1"}
	if err := runtime.handleNotification(context.Background(), codexapp.Message{
		Method: "thread/tokenUsage/updated",
		Params: []byte(`{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"total":{"totalTokens":8000,"inputTokens":6500,"cachedInputTokens":4000,"cacheWriteInputTokens":500,"outputTokens":1500},"last":{"totalTokens":2400},"modelContextWindow":128000}}`),
	}); err != nil {
		t.Fatal(err)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-usage")
	if err != nil || len(items) != 1 || items[0].ContextUsage == nil || items[0].ContextUsage.Tokens != 2400 || items[0].ContextUsage.Window != 128000 {
		t.Fatalf("context usage items = %#v, err=%v", items, err)
	}
	if got, want := runtime.totalUsage, (protocol.Usage{
		InputTokens: 2000, OutputTokens: 1500, TotalTokens: 8000, CacheCreationInputTokens: 500, CacheReadInputTokens: 4000,
	}); got != want {
		t.Fatalf("total usage = %#v, want %#v", got, want)
	}
}

func TestCodexWorkerDoesNotAttributeUsageFromAnotherTurn(t *testing.T) {
	runtime := &codexWorkerRuntime{threadID: "thread-1", activeTurnID: "turn-active"}
	if err := runtime.handleNotification(context.Background(), codexapp.Message{
		Method: "thread/tokenUsage/updated",
		Params: []byte(`{"threadId":"thread-1","turnId":"turn-stale","tokenUsage":{"total":{"totalTokens":20,"inputTokens":15,"outputTokens":5},"last":{"totalTokens":20,"inputTokens":15,"outputTokens":5}}}`),
	}); err != nil {
		t.Fatal(err)
	}
	if runtime.turnUsage != (protocol.Usage{}) {
		t.Fatalf("turn usage = %#v", runtime.turnUsage)
	}
	if runtime.totalUsage.TotalTokens != 20 || !runtime.hasTotalUsage {
		t.Fatalf("cumulative usage baseline = %#v, known=%v", runtime.totalUsage, runtime.hasTotalUsage)
	}
}

func TestCodexWorkerDispatchesPromptHooksForInitialMessageAndSteering(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "session-prompts", Model: "gpt-5.6-luna", CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	hookDispatcher := &capturingHookDispatcher{}
	runtime := &codexWorkerRuntime{
		cfg:   codexWorkerConfig{workerClientConfig: workerClientConfig{SessionID: "session-prompts"}, WorkingDirectory: "/workspace"},
		store: store, app: &codexWorkerRPC{}, threadID: "thread-1", messages: make(map[string]string), hooks: hookDispatcher,
	}
	if err := runtime.handleCommand(context.Background(), commandResponse{
		ID: "message-1", Type: sessionCommandMessage, Message: &messageCommandPayload{Text: "first"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := runtime.handleCommand(context.Background(), commandResponse{
		ID: "steering-1", Type: sessionCommandSteering, Steering: &steeringCommandPayload{Text: "correction"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if len(hookDispatcher.events) != 3 {
		t.Fatalf("hook events = %#v", hookDispatcher.events)
	}
	if hookDispatcher.events[0].EventName != hooks.TurnStart || hookDispatcher.events[0].TurnID != "turn-1" || hookDispatcher.events[0].Turn == nil || hookDispatcher.events[0].Turn.Model != "gpt-5.6-luna" {
		t.Fatalf("TurnStart event = %#v", hookDispatcher.events[0])
	}
	for index, text := range []string{"first", "correction"} {
		event := hookDispatcher.events[index+1]
		if event.EventName != hooks.UserPromptSubmit || event.TurnID != "turn-1" || event.Prompt == nil || event.Prompt.Text != text {
			t.Fatalf("prompt event %d = %#v", index, event)
		}
	}
}

func TestCodexWorkerPersistsAppliedSteering(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "session-steering", CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	rpc := &codexWorkerRPC{}
	runtime := &codexWorkerRuntime{
		cfg:   codexWorkerConfig{workerClientConfig: workerClientConfig{SessionID: "session-steering"}},
		store: store, app: rpc, threadID: "thread-1", activeTurnID: "turn-1",
	}
	command := commandResponse{
		ID: "steering-1", Type: sessionCommandSteering, CreatedAt: now,
		Steering: &steeringCommandPayload{Text: "use lite_downgrade", TurnID: "turn-1"},
	}
	if err := runtime.handleCommand(context.Background(), command, nil); err != nil {
		t.Fatal(err)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-steering")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("display items = %#v", items)
	}
	item := items[0]
	if item.ID != command.ID || item.Type != zotigosession.DisplayItemSteeringMessage || item.Role != string(protocol.RoleUser) {
		t.Fatalf("steering item = %#v", item)
	}
	if item.Turn == nil || item.Turn.ID != "turn-1" || len(item.Content) != 1 || item.Content[0].Text != "use lite_downgrade" {
		t.Fatalf("steering content = %#v", item)
	}
	if item.Command == nil || item.Command.Type != sessionCommandSteering || item.Command.Text != "use lite_downgrade" {
		t.Fatalf("steering command = %#v", item.Command)
	}
	if got := fmt.Sprint(rpc.methods); got != "[turn/steer]" {
		t.Fatalf("methods = %s", got)
	}
}

func TestCodexWorkerPersistsCompletedItemsInProtocolOrder(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "session-items", CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	writer := &workerClientWriter{sendCh: make(chan workerMessage, 16), done: make(chan struct{})}
	hookDispatcher := &capturingHookDispatcher{}
	runtime := &codexWorkerRuntime{
		cfg: codexWorkerConfig{workerClientConfig: workerClientConfig{SessionID: "session-items"}}, store: store, writer: writer,
		threadID: "thread-1", activeTurnID: "turn-1", turnStarted: now, messages: make(map[string]string), toolNames: make(map[string]string), hooks: hookDispatcher,
	}
	notifications := []codexapp.Message{
		{Method: "item/completed", Params: []byte(`{"threadId":"thread-1","turnId":"turn-1","completedAtMs":0,"item":{"type":"userMessage","id":"user-1","content":[{"type":"text","text":"Read AGENTS.md"}]}}`)},
		{Method: "item/agentMessage/delta", Params: []byte(`{"threadId":"thread-1","turnId":"turn-1","itemId":"message-1","delta":"Checking files."}`)},
		{Method: "item/completed", Params: []byte(`{"threadId":"thread-1","turnId":"turn-1","completedAtMs":1,"item":{"type":"agentMessage","id":"message-1","text":"Checking files."}}`)},
		{Method: "item/started", Params: []byte(`{"threadId":"thread-1","turnId":"turn-1","startedAtMs":2,"item":{"type":"commandExecution","id":"tool-1","command":"sed -n '1,20p' AGENTS.md","cwd":"/tmp/workspace","status":"inProgress","commandActions":[{"type":"read","command":"sed -n '1,20p' AGENTS.md","name":"AGENTS.md","path":"/tmp/workspace/AGENTS.md"}]}}`)},
		{Method: "item/commandExecution/outputDelta", Params: []byte(`{"threadId":"thread-1","turnId":"turn-1","itemId":"tool-1","delta":"# Instructions\n"}`)},
		{Method: "item/completed", Params: []byte(`{"threadId":"thread-1","turnId":"turn-1","completedAtMs":3,"item":{"type":"commandExecution","id":"tool-1","command":"sed -n '1,20p' AGENTS.md","cwd":"/tmp/workspace","status":"completed","commandActions":[{"type":"read","command":"sed -n '1,20p' AGENTS.md","name":"AGENTS.md","path":"/tmp/workspace/AGENTS.md"}],"aggregatedOutput":"# Instructions\n","exitCode":0,"durationMs":12}}`)},
		{Method: "item/completed", Params: []byte(`{"threadId":"thread-1","turnId":"turn-1","completedAtMs":4,"item":{"type":"reasoning","id":"reasoning-1","summary":["The instructions apply."],"content":[]}}`)},
		{Method: "thread/tokenUsage/updated", Params: []byte(`{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"total":{"totalTokens":30,"inputTokens":24,"cachedInputTokens":10,"cacheWriteInputTokens":4,"outputTokens":6},"last":{"totalTokens":30,"inputTokens":24,"cachedInputTokens":10,"cacheWriteInputTokens":4,"outputTokens":6},"modelContextWindow":128000}}`)},
		{Method: "turn/completed", Params: []byte(`{"turn":{"id":"turn-1","status":"completed","durationMs":20}}`)},
	}
	for _, notification := range notifications {
		if err := runtime.handleNotification(context.Background(), notification); err != nil {
			t.Fatalf("handle %s: %v", notification.Method, err)
		}
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-items")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 6 {
		t.Fatalf("display items = %#v", items)
	}
	if items[0].ID != "message-1" || items[0].Content[0].Text != "Checking files." {
		t.Fatalf("message item = %#v", items[0])
	}
	call := items[1].Content[0].ToolCall
	if call == nil || call.ID != "tool-1" || call.Name != "read_file" || !strings.Contains(call.Arguments, `"path":"/tmp/workspace/AGENTS.md"`) {
		t.Fatalf("tool call = %#v", items[1])
	}
	result := items[2].Content[0].ToolResult
	if result == nil || result.ToolCallID != "tool-1" || result.ToolName != "read_file" || result.Text != "# Instructions\n" || result.IsError {
		t.Fatalf("tool result = %#v", items[2])
	}
	if items[3].Content[0].Type != string(protocol.ContentTypeReasoning) || items[3].Content[0].Text != "The instructions apply." {
		t.Fatalf("reasoning item = %#v", items[3])
	}
	if items[5].Type != zotigosession.DisplayItemTurnCompleted {
		t.Fatalf("turn item = %#v", items[5])
	}
	if len(hookDispatcher.events) != 2 {
		t.Fatalf("hook events = %#v", hookDispatcher.events)
	}
	hookEvent := hookDispatcher.events[0]
	if hookEvent.EventName != hooks.PostToolUse || hookEvent.Tool == nil || hookEvent.Tool.Name != "read_file" || hookEvent.Tool.NativeName != "commandExecution" || hookEvent.Tool.Result == nil || hookEvent.Tool.Result.Status != "succeeded" {
		t.Fatalf("PostToolUse event = %#v", hookEvent)
	}
	turnEnd := hookDispatcher.events[1]
	if turnEnd.EventName != hooks.TurnEnd || turnEnd.Turn == nil || turnEnd.Turn.Status != "completed" || turnEnd.Turn.Usage == nil ||
		turnEnd.Turn.Usage.InputTokens != 10 || turnEnd.Turn.Usage.OutputTokens != 6 || turnEnd.Turn.Usage.TotalTokens != 30 ||
		turnEnd.Turn.Usage.CacheCreationInputTokens != 4 || turnEnd.Turn.Usage.CacheReadInputTokens != 10 {
		t.Fatalf("TurnEnd event = %#v", turnEnd)
	}
	firstDelta := nextWorkerDelta(t, writer.sendCh)
	secondDelta := nextWorkerDelta(t, writer.sendCh)
	if firstDelta.Delta == nil || firstDelta.Delta.PartType != string(protocol.ContentTypeText) {
		t.Fatalf("first delta = %#v", firstDelta)
	}
	if secondDelta.Delta == nil || secondDelta.Delta.PartType != "tool_progress" || secondDelta.Delta.ToolCallID != "tool-1" || secondDelta.Delta.ToolName != "read_file" {
		t.Fatalf("second delta = %#v", secondDelta)
	}
}

func nextWorkerDelta(t *testing.T, messages <-chan workerMessage) workerMessage {
	t.Helper()
	for {
		select {
		case message := <-messages:
			if message.Delta != nil {
				return message
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for worker delta")
		}
	}
}

func TestCodexToolAdaptersCoverFileAndExternalTools(t *testing.T) {
	tests := []struct {
		name       string
		item       codexThreadItem
		toolName   string
		isError    bool
		hookStatus string
	}{
		{name: "file change", item: codexThreadItem{ID: "file-1", Type: "fileChange", Status: "completed", Changes: []any{map[string]any{"path": "a.go"}}}, toolName: "apply_patch"},
		{name: "file change declined", item: codexThreadItem{ID: "file-declined", Type: "fileChange", Status: "declined"}, toolName: "apply_patch", isError: true, hookStatus: "denied"},
		{name: "command failed without exit", item: codexThreadItem{ID: "command-1", Type: "commandExecution", Status: "failed"}, toolName: "shell", isError: true},
		{name: "command declined", item: codexThreadItem{ID: "command-2", Type: "commandExecution", Status: "declined"}, toolName: "shell", isError: true, hookStatus: "denied"},
		{name: "mcp", item: codexThreadItem{ID: "mcp-1", Type: "mcpToolCall", Tool: "search", Status: "completed", Arguments: map[string]any{"query": "zotigo"}, Result: map[string]any{"content": []any{"ok"}}}, toolName: "search"},
		{name: "mcp declined", item: codexThreadItem{ID: "mcp-declined", Type: "mcpToolCall", Tool: "search", Status: "declined"}, toolName: "search", isError: true, hookStatus: "denied"},
		{name: "mcp failed with result", item: codexThreadItem{ID: "mcp-failed", Type: "mcpToolCall", Tool: "search", Status: "failed", Result: map[string]any{"content": []any{"error"}}}, toolName: "search", isError: true},
		{name: "mcp error", item: codexThreadItem{ID: "mcp-2", Type: "mcpToolCall", Tool: "fetch", Error: &struct {
			Message string `json:"message"`
		}{Message: "unavailable"}}, toolName: "fetch", isError: true},
		{name: "dynamic", item: codexThreadItem{ID: "dynamic-1", Type: "dynamicToolCall", Tool: "lookup", Arguments: map[string]any{"id": 1}, Success: boolPointer(false)}, toolName: "lookup", isError: true},
		{name: "dynamic declined", item: codexThreadItem{ID: "dynamic-declined", Type: "dynamicToolCall", Tool: "lookup", Status: "declined"}, toolName: "lookup", isError: true, hookStatus: "denied"},
		{name: "dynamic failed without success", item: codexThreadItem{ID: "dynamic-2", Type: "dynamicToolCall", Tool: "lookup", Status: "failed"}, toolName: "lookup", isError: true},
		{name: "spawn subagent thread", item: codexThreadItem{ID: "spawn-1", Type: "collabAgentToolCall", Tool: "spawnAgent", Status: "completed", Prompt: stringPointer("review code"), ReceiverThreadIDs: []string{"thread-child"}, AgentsStates: map[string]codexAgentState{"thread-child": {Status: "running"}}}, toolName: "spawn_agent"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			name, _, ok, err := codexToolCall(test.item)
			if err != nil || !ok || name != test.toolName {
				t.Fatalf("tool call = name %q, ok %v, err %v", name, ok, err)
			}
			result, ok := codexToolResult(test.item, name)
			if !ok || result.ToolName != test.toolName || result.IsError != test.isError {
				t.Fatalf("tool result = %#v, ok=%v", result, ok)
			}
			wantStatus := test.hookStatus
			if wantStatus == "" {
				wantStatus = "succeeded"
				if test.isError {
					wantStatus = "failed"
				}
			}
			if status := codexToolResultStatus(test.item.Status, result); status != wantStatus {
				t.Fatalf("hook status = %q, want %q", status, wantStatus)
			}
		})
	}
	spawn := codexThreadItem{
		ID: "spawn-details", Type: "collabAgentToolCall", Tool: "spawnAgent", Status: "completed",
		Prompt: stringPointer("review code"), ReceiverThreadIDs: []string{"thread-child"},
		AgentsStates: map[string]codexAgentState{"thread-child": {Status: "completed"}},
	}
	_, arguments, _, err := codexToolCall(spawn)
	if err != nil || !strings.Contains(arguments, `"description":"review code"`) {
		t.Fatalf("spawn arguments = %q, err=%v", arguments, err)
	}
	result, ok := codexToolResult(spawn, "spawn_agent")
	resultJSON, isMap := result.JSON.(map[string]any)
	if !ok || !isMap || resultJSON["agents_states"] == nil || result.Metadata != nil {
		t.Fatalf("spawn result = %#v, ok=%v", result, ok)
	}
}

func boolPointer(value bool) *bool { return &value }

func stringPointer(value string) *string { return &value }

func (r *codexWorkerRPC) Call(_ context.Context, method string, params any, result any) error {
	r.methods = append(r.methods, method)
	request := params.(map[string]any)
	if method == "thread/resume" {
		r.resumeApproval, _ = request["approvalPolicy"].(string)
	}
	if method == "turn/start" && r.err == nil {
		if err := sonic.Unmarshal([]byte(`{"turn":{"id":"turn-1"}}`), result); err != nil {
			return err
		}
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
