package zotigod

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

func TestWorkerDisplayLogPersistsTurnRuntime(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog("sess-runtime", source)
	runtime := zotigosession.DisplayRuntime{Agent: "zotigo", ProfileName: "main", Model: "gemini-test"}
	if _, err := display.StartTurnWithRuntime(context.Background(), runtime); err != nil {
		t.Fatal(err)
	}
	items, _, err := source.LoadItems(context.Background(), "sess-runtime")
	if err != nil || len(items) != 1 || items[0].Turn == nil || items[0].Turn.Runtime == nil || *items[0].Turn.Runtime != runtime {
		t.Fatalf("items=%+v err=%v", items, err)
	}
}

func TestWorkerDisplayLogPersistsAccumulatedTurnUsage(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog("sess-usage", source)
	ctx := context.Background()
	if _, err := display.StartTurn(ctx); err != nil {
		t.Fatal(err)
	}
	if err := display.HandleEvent(ctx, protocol.Event{Type: protocol.EventTypeFinish, FinishReason: protocol.FinishReason("need_approval"), Usage: &protocol.Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}}); err != nil {
		t.Fatal(err)
	}
	if err := display.HandleEvent(ctx, protocol.Event{Type: protocol.EventTypeFinish, FinishReason: protocol.FinishReasonStop, Usage: &protocol.Usage{InputTokens: 20, OutputTokens: 3, TotalTokens: 23}}); err != nil {
		t.Fatal(err)
	}
	items, _, err := source.LoadItems(ctx, "sess-usage")
	if err != nil || len(items) != 2 || items[1].Turn == nil || items[1].Turn.Usage == nil {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	want := protocol.Usage{InputTokens: 30, OutputTokens: 5, TotalTokens: 35}
	if got := *items[1].Turn.Usage; got != want {
		t.Fatalf("usage=%+v want=%+v", got, want)
	}
}

func TestWorkerUsageProgressSurvivesTerminalAndApproval(t *testing.T) {
	for _, terminal := range []string{"completed", "failed", "interrupted"} {
		t.Run(terminal, func(t *testing.T) {
			source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
			display := newWorkerDisplayLog("progress", source)
			ctx := context.Background()
			if _, err := display.StartTurn(ctx); err != nil {
				t.Fatal(err)
			}
			send := func(event protocol.Event) {
				t.Helper()
				if err := display.HandleEvent(ctx, event); err != nil {
					t.Fatal(err)
				}
			}
			usage := &protocol.Usage{InputTokens: 10, OutputTokens: 2}
			progress := protocol.Event{Type: protocol.EventTypeUsageUpdated, Usage: usage, ContextUsage: &protocol.ContextUsage{Tokens: 15, Window: 100, Source: "provider_estimate", ProfileKey: "profile"}}
			send(progress)
			send(progress) // cumulative run snapshots must not be summed
			items, _, _ := source.LoadItems(ctx, "progress")
			if got := items[len(items)-1]; got.ContextUsage.Tokens != 15 || got.Turn.Usage.TotalTokens != 12 {
				t.Fatalf("progress = %+v", got)
			}
			send(protocol.Event{Type: protocol.EventTypeFinish, FinishReason: "need_approval", Usage: usage})
			progress.Usage = &protocol.Usage{InputTokens: 20, OutputTokens: 3}
			send(progress)
			switch terminal {
			case "completed":
				send(protocol.Event{Type: protocol.EventTypeFinish, FinishReason: protocol.FinishReasonStop, Usage: progress.Usage})
			case "failed":
				send(protocol.Event{Type: protocol.EventTypeError, Error: errors.New("unavailable"), Usage: progress.Usage})
			case "interrupted":
				if err := display.Interrupt(ctx, "deadline"); err != nil {
					t.Fatal(err)
				}
			}
			items, _, _ = source.LoadItems(ctx, "progress")
			if got := items[len(items)-1].Turn.Usage; got == nil || got.TotalTokens != 35 {
				t.Fatalf("terminal usage = %+v", got)
			}
		})
	}
}

func TestWorkerDisplayLogPersistsFailedTurnUsage(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog("sess-failed-usage", source)
	ctx := context.Background()
	if _, err := display.StartTurn(ctx); err != nil {
		t.Fatal(err)
	}
	want := protocol.Usage{InputTokens: 30, OutputTokens: 5, TotalTokens: 35}
	event := protocol.NewErrorEvent(errors.New("upstream failed"))
	event.Usage = &want
	if err := display.HandleEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	items, _, err := source.LoadItems(ctx, "sess-failed-usage")
	if err != nil {
		t.Fatal(err)
	}
	last := items[len(items)-1]
	if last.Turn == nil || last.Turn.Usage == nil || *last.Turn.Usage != want {
		t.Fatalf("failed turn lost usage: %+v", last)
	}
}

func TestWorkerDisplayLogPersistsToolEventsBeforeFinishWithoutDuplicates(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	display := newWorkerDisplayLog("sess-tool-stream", source)
	ctx := context.Background()
	var wakeSequences []uint64
	display.wake = func(ctx context.Context) {
		items, _, err := source.LoadItems(ctx, "sess-tool-stream")
		if err != nil || len(items) == 0 {
			t.Errorf("wake ran before persistence: items=%#v err=%v", items, err)
			return
		}
		wakeSequences = append(wakeSequences, items[len(items)-1].Sequence)
	}
	if _, err := display.StartTurn(ctx); err != nil {
		t.Fatalf("start turn: %v", err)
	}
	if err := display.HandleEvent(ctx, protocol.NewTextDeltaEvent("checking")); err != nil {
		t.Fatalf("append text: %v", err)
	}
	if err := display.HandleEvent(ctx, protocol.Event{
		Type:     protocol.EventTypeToolCallEnd,
		ToolCall: &protocol.ToolCall{ID: "call-1", Name: "read_file", Arguments: `{"path":"README.md"}`},
	}); err != nil {
		t.Fatalf("append tool call: %v", err)
	}

	items, _, err := source.LoadItems(ctx, "sess-tool-stream")
	if err != nil {
		t.Fatalf("load items after tool call: %v", err)
	}
	if len(items) != 3 || items[1].Type != zotigosession.DisplayItemAssistantMessage || items[2].Type != zotigosession.DisplayItemAssistantMessage {
		t.Fatalf("tool call was not persisted before finish: %#v", items)
	}
	if len(items[1].Content) != 1 || items[1].Content[0].Text != "checking" || len(items[2].Content) != 1 || items[2].Content[0].ToolCall == nil {
		t.Fatalf("tool call content order was not preserved: %#v %#v", items[1].Content, items[2].Content)
	}
	page := getItems(t, handler, "/sessions/sess-tool-stream/items")
	if len(page.Items) != 3 || len(page.Items[2].Content) != 1 || page.Items[2].Content[0].ToolCall == nil {
		t.Fatalf("tool call was not observable through /items: %#v", page.Items)
	}

	if err := display.HandleEvent(ctx, protocol.Event{
		Type: protocol.EventTypeToolResultDone,
		ToolResult: &protocol.ToolResult{
			ToolCallID: "call-1",
			ToolName:   "read_file",
			Type:       protocol.ToolResultTypeText,
			Text:       "contents",
		},
	}); err != nil {
		t.Fatalf("append tool result: %v", err)
	}
	items, _, err = source.LoadItems(ctx, "sess-tool-stream")
	if err != nil {
		t.Fatalf("load items after tool result: %v", err)
	}
	if len(items) != 4 || len(items[3].Content) != 1 || items[3].Content[0].ToolResult == nil {
		t.Fatalf("tool result was not persisted before finish: %#v", items)
	}
	page = getItems(t, handler, "/sessions/sess-tool-stream/items")
	if len(page.Items) != 4 || len(page.Items[3].Content) != 1 || page.Items[3].Content[0].ToolResult == nil {
		t.Fatalf("tool result was not observable through /items: %#v", page.Items)
	}

	if err := display.HandleEvent(ctx, protocol.NewFinishEvent(protocol.FinishReasonStop)); err != nil {
		t.Fatalf("finish turn: %v", err)
	}
	items, _, err = source.LoadItems(ctx, "sess-tool-stream")
	if err != nil {
		t.Fatalf("load final items: %v", err)
	}
	if len(items) != 5 || items[4].Type != zotigosession.DisplayItemTurnCompleted {
		t.Fatalf("unexpected final display log: %#v", items)
	}
	toolCalls := 0
	toolResults := 0
	for _, item := range items {
		for _, part := range item.Content {
			if part.ToolCall != nil {
				toolCalls++
			}
			if part.ToolResult != nil {
				toolResults++
			}
		}
	}
	if toolCalls != 1 || toolResults != 1 {
		t.Fatalf("tool events duplicated after finish: calls=%d results=%d", toolCalls, toolResults)
	}
	if len(wakeSequences) != 5 || wakeSequences[0] != 1 || wakeSequences[1] != 2 || wakeSequences[2] != 3 || wakeSequences[3] != 4 || wakeSequences[4] != 5 {
		t.Fatalf("wake notifications did not follow persistence: %#v", wakeSequences)
	}
}

func TestWorkerDisplayLogStreamsToolProgressWithoutPersistingIt(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog("sess-tool-progress", source)
	var deltas []displayDeltaEvent
	display.delta = func(delta displayDeltaEvent) { deltas = append(deltas, delta) }
	ctx := context.Background()
	if _, err := display.StartTurn(ctx); err != nil {
		t.Fatal(err)
	}
	if err := display.HandleEvent(ctx, protocol.Event{
		Type: protocol.EventTypeToolProgress,
		ToolResult: &protocol.ToolResult{
			ToolCallID: "call-1", ToolName: "shell", Type: protocol.ToolResultTypeText, Text: "building...\n",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 1 || deltas[0].PartType != "tool_progress" || deltas[0].ToolCallID != "call-1" || deltas[0].Delta != "building...\n" {
		t.Fatalf("progress deltas = %#v", deltas)
	}
	items, _, err := source.LoadItems(ctx, "sess-tool-progress")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Type != zotigosession.DisplayItemTurnStarted {
		t.Fatalf("progress must remain volatile, items = %#v", items)
	}
}

func TestWorkerDisplayLogStreamsAndPersistsSubagentEvents(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	display := newWorkerDisplayLog("sess-subagent-stream", source)
	var deltas []displayDeltaEvent
	display.delta = func(delta displayDeltaEvent) { deltas = append(deltas, delta) }
	ctx := context.Background()
	if _, err := display.StartTurn(ctx); err != nil {
		t.Fatal(err)
	}

	wrap := func(event protocol.Event) protocol.Event {
		return protocol.Event{
			Type: protocol.EventTypeSubagent,
			Subagent: &protocol.SubagentEvent{
				ToolCallID: "spawn-1", Name: "reviewer", AgentType: "general-purpose",
				WorkDir: "/tmp/project", Description: "Review the change", Event: &event,
			},
		}
	}
	if err := display.HandleEvent(ctx, wrap(protocol.NewTextDeltaEvent("Checking "))); err != nil {
		t.Fatal(err)
	}
	if err := display.HandleEvent(ctx, wrap(protocol.NewTextDeltaEvent("tests"))); err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 2 || deltas[0].Subagent == nil || deltas[0].Subagent.ToolCallID != "spawn-1" || deltas[1].Delta != "tests" {
		t.Fatalf("subagent deltas = %#v", deltas)
	}
	streamedItemID := deltas[0].ItemID
	if err := display.HandleEvent(ctx, wrap(protocol.Event{
		Type:        protocol.EventTypeContentEnd,
		ContentPart: &protocol.ContentPart{Type: protocol.ContentTypeText, Text: "Checking tests"},
	})); err != nil {
		t.Fatal(err)
	}
	if err := display.HandleEvent(ctx, wrap(protocol.Event{
		Type:     protocol.EventTypeToolCallEnd,
		ToolCall: &protocol.ToolCall{ID: "child-call", Name: "shell", Arguments: `{"command":"go test ./..."}`},
	})); err != nil {
		t.Fatal(err)
	}
	if err := display.HandleEvent(ctx, wrap(protocol.Event{
		Type: protocol.EventTypeToolProgress,
		ToolResult: &protocol.ToolResult{
			ToolCallID: "child-call", ToolName: "shell", Type: protocol.ToolResultTypeText, Text: "package one passed\n",
		},
	})); err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 3 || deltas[2].PartType != "tool_progress" || deltas[2].ToolCallID != "child-call" || deltas[2].Subagent == nil {
		t.Fatalf("subagent tool progress = %#v", deltas)
	}
	if err := display.HandleEvent(ctx, wrap(protocol.NewFinishEvent(protocol.FinishReason("need_approval")))); err != nil {
		t.Fatal(err)
	}
	if err := display.HandleEvent(ctx, protocol.Event{
		Type: protocol.EventTypeSubagent,
		Subagent: &protocol.SubagentEvent{
			ToolCallID: "spawn-1", Name: "reviewer", Status: "running",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := display.HandleEvent(ctx, wrap(protocol.NewFinishEvent(protocol.FinishReasonStop))); err != nil {
		t.Fatal(err)
	}

	items, _, err := source.LoadItems(ctx, "sess-subagent-stream")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 6 {
		t.Fatalf("subagent items = %#v", items)
	}
	for _, item := range items[1:] {
		if item.Type != zotigosession.DisplayItemAssistantMessage || item.Subagent == nil || item.Subagent.ToolCallID != "spawn-1" {
			t.Fatalf("unexpected subagent item = %#v", item)
		}
	}
	if items[1].Content[0].Text != "Checking tests" || items[2].Content[0].ToolCall == nil {
		t.Fatalf("subagent content was not persisted: %#v %#v", items[1], items[2])
	}
	if items[1].ID != streamedItemID {
		t.Fatalf("streamed item ID %q did not match durable item ID %q", streamedItemID, items[1].ID)
	}
	if items[3].Subagent.Status != "waiting_approval" {
		t.Fatalf("subagent status = %q", items[3].Subagent.Status)
	}
	if items[4].Subagent.Status != "running" || items[5].Subagent.Status != "completed" {
		t.Fatalf("subagent resume statuses = %q, %q", items[4].Subagent.Status, items[5].Subagent.Status)
	}
	page := getItems(t, handler, "/sessions/sess-subagent-stream/items")
	if len(page.Items) != 6 || page.Items[1].Subagent == nil || page.Items[1].Subagent.ToolCallID != "spawn-1" {
		t.Fatalf("public subagent items = %#v", page.Items)
	}
}

func TestWorkerDisplayLogPersistsContextCompaction(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog("sess-compaction", source)
	ctx := context.Background()
	if _, err := display.StartTurn(ctx); err != nil {
		t.Fatal(err)
	}
	if err := display.HandleEvent(ctx, protocol.NewTextDeltaEvent("before compaction")); err != nil {
		t.Fatal(err)
	}
	if err := display.HandleEvent(ctx, protocol.Event{
		Type: protocol.EventTypeContextCompacted,
		ContextCompaction: &protocol.ContextCompaction{
			OriginalTokens: 183421, CompressedTokens: 91736,
			MessagesBefore: 108, MessagesAfter: 19,
		},
	}); err != nil {
		t.Fatal(err)
	}

	items, _, err := source.LoadItems(ctx, "sess-compaction")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[1].Type != zotigosession.DisplayItemAssistantMessage || items[2].Type != zotigosession.DisplayItemContextCompacted {
		t.Fatalf("unexpected compaction display log: %#v", items)
	}
	if got := items[2].ContextCompaction; got == nil || got.OriginalTokens != 183421 || got.CompressedTokens != 91736 || got.MessagesBefore != 108 || got.MessagesAfter != 19 {
		t.Fatalf("context compaction metadata = %#v", got)
	}
}

func TestWorkerDisplayLogStreamsVolatileDeltaThenPersistsSameItemID(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog("sess-text-stream", source)
	var deltas []displayDeltaEvent
	display.delta = func(delta displayDeltaEvent) {
		deltas = append(deltas, delta)
	}
	ctx := context.Background()
	if _, err := display.StartTurn(ctx); err != nil {
		t.Fatalf("start turn: %v", err)
	}
	if err := display.HandleEvent(ctx, protocol.NewReasoningDeltaEvent("think ")); err != nil {
		t.Fatalf("first delta: %v", err)
	}
	if err := display.HandleEvent(ctx, protocol.NewReasoningDeltaEvent("carefully")); err != nil {
		t.Fatalf("second delta: %v", err)
	}
	items, _, err := source.LoadItems(ctx, "sess-text-stream")
	if err != nil {
		t.Fatalf("load before content end: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("volatile deltas were persisted before block end: %#v", items)
	}
	if len(deltas) != 2 || deltas[0].ItemID == "" || deltas[0].ItemID != deltas[1].ItemID {
		t.Fatalf("deltas did not share a stable item id: %#v", deltas)
	}
	if deltas[0].PartType != string(protocol.ContentTypeReasoning) || deltas[0].Role != string(protocol.RoleAssistant) {
		t.Fatalf("unexpected delta metadata: %#v", deltas[0])
	}
	if err := display.HandleEvent(ctx, protocol.Event{
		Type:  protocol.EventTypeContentEnd,
		Index: 0,
		ContentPart: &protocol.ContentPart{
			Type: protocol.ContentTypeReasoning,
			Text: "think carefully",
		},
	}); err != nil {
		t.Fatalf("content end: %v", err)
	}
	items, _, err = source.LoadItems(ctx, "sess-text-stream")
	if err != nil {
		t.Fatalf("load after content end: %v", err)
	}
	if len(items) != 2 || items[1].ID != deltas[0].ItemID || items[1].Sequence != 2 {
		t.Fatalf("durable item did not reconcile the volatile block: %#v", items)
	}
	if len(items[1].Content) != 1 || items[1].Content[0].Text != "think carefully" {
		t.Fatalf("durable block content = %#v", items[1].Content)
	}
}

func TestWorkerDisplayLogDropsUnfinishedVolatileBlockOnFailure(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog("sess-failed-stream", source)
	ctx := context.Background()
	if _, err := display.StartTurn(ctx); err != nil {
		t.Fatalf("start turn: %v", err)
	}
	if err := display.HandleEvent(ctx, protocol.NewTextDeltaEvent("partial output")); err != nil {
		t.Fatalf("stream delta: %v", err)
	}
	if err := display.Fail(ctx, errors.New("provider disconnected")); err != nil {
		t.Fatalf("fail turn: %v", err)
	}
	items, _, err := source.LoadItems(ctx, "sess-failed-stream")
	if err != nil {
		t.Fatalf("load failed turn: %v", err)
	}
	if len(items) != 3 || items[0].Type != zotigosession.DisplayItemTurnStarted || items[1].Type != zotigosession.DisplayItemError || items[2].Type != zotigosession.DisplayItemTurnFailed {
		t.Fatalf("unexpected failed turn log: %#v", items)
	}
	for _, item := range items {
		if item.Type == zotigosession.DisplayItemAssistantMessage {
			t.Fatalf("unfinished volatile block was persisted: %#v", item)
		}
	}
}

func TestWorkerDisplayLogPersistsSteeringAfterCurrentGeneration(t *testing.T) {
	ctx := context.Background()
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog("session-steering", source)
	if _, err := display.StartTurn(ctx); err != nil {
		t.Fatalf("start turn: %v", err)
	}
	display.QueueSteering(commandResponse{
		ID:        "steering-command",
		Type:      sessionCommandSteering,
		CreatedAt: time.Now().UTC(),
		Steering:  &steeringCommandPayload{Text: "change direction"},
	})
	var deliveryOrder []string
	display.delta = func(delta displayDeltaEvent) {
		deliveryOrder = append(deliveryOrder, "delta:"+delta.Delta)
	}
	display.barrier = func(context.Context) error {
		deliveryOrder = append(deliveryOrder, "barrier")
		items, _, err := source.LoadItems(ctx, "session-steering")
		if err != nil {
			t.Fatalf("load items at barrier: %v", err)
		}
		for _, item := range items {
			if item.Type == zotigosession.DisplayItemSteeringMessage {
				t.Fatalf("steering became durable before barrier acknowledgement: %#v", item)
			}
		}
		return nil
	}
	display.wakeSync = func(context.Context) error {
		deliveryOrder = append(deliveryOrder, "wake")
		return nil
	}
	interrupted := protocol.ToolResult{
		ToolCallID: "call-1",
		ToolName:   "shell",
		Type:       protocol.ToolResultTypeExecutionDenied,
		Reason:     "interrupted_by_steering",
		IsError:    true,
	}
	for _, event := range []protocol.Event{
		protocol.NewTextDeltaEvent("old tail"),
		{Type: protocol.EventTypeContentEnd, ContentPart: &protocol.ContentPart{Type: protocol.ContentTypeText, Text: "old tail"}},
		{Type: protocol.EventTypeToolCallEnd, ToolCall: &protocol.ToolCall{ID: "call-1", Name: "shell", Arguments: "{}"}},
		{Type: protocol.EventTypeToolResultDone, ToolResult: &interrupted},
		{Type: protocol.EventTypeSteeringApplied, SteeringIDs: []string{"steering-command"}},
		protocol.NewTextDeltaEvent("new response"),
		{Type: protocol.EventTypeContentEnd, ContentPart: &protocol.ContentPart{Type: protocol.ContentTypeText, Text: "new response"}},
	} {
		if err := display.HandleEvent(ctx, event); err != nil {
			t.Fatalf("handle %s: %v", event.Type, err)
		}
	}
	items, _, err := source.LoadItems(ctx, "session-steering")
	if err != nil {
		t.Fatalf("load items: %v", err)
	}
	if len(items) != 6 {
		t.Fatalf("unexpected items: %#v", items)
	}
	steering := items[4]
	if steering.ID != "steering-command" || steering.Type != zotigosession.DisplayItemSteeringMessage || len(steering.Content) != 1 || steering.Content[0].Text != "change direction" {
		t.Fatalf("unexpected steering item: %#v", steering)
	}
	if items[1].Content[0].Text != "old tail" || items[5].Content[0].Text != "new response" {
		t.Fatalf("generation content crossed steering item: %#v", items)
	}
	if got := strings.Join(deliveryOrder, "|"); got != "delta:old tail|barrier|wake|delta:new response" {
		t.Fatalf("new generation delta crossed steering barrier: %s", got)
	}
}

func TestSteeringDisplayBoundarySurvivesStoreReload(t *testing.T) {
	ctx := t.Context()
	root := newTestStoreRoot(t)
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const id = "steering-boundary"
	putStoredSession(t, store, id, t.TempDir())
	source := storedDisplayItemSource{store: store}
	display := newWorkerDisplayLog(id, source)
	turn, err := display.StartTurn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var deltas []displayDeltaEvent
	display.delta = func(delta displayDeltaEvent) { deltas = append(deltas, delta) }
	if err := display.HandleEvent(ctx, protocol.NewTextDeltaEvent("old thinking")); err != nil {
		t.Fatal(err)
	}
	for _, inputID := range []string{"steer-1", "steer-2"} {
		command := commandResponse{ID: inputID, Type: sessionCommandSteering, Steering: &steeringCommandPayload{Text: inputID, TurnID: turn}}
		display.QueueSteering(command)
		if _, err := appendAcceptedSessionInput(ctx, source, id, command); err != nil {
			t.Fatal(err)
		}
	}
	if len(deltas) != 3 || deltas[1].BeforeInputID != "steer-1" || deltas[2].BeforeInputID != "steer-1" {
		t.Fatalf("current preview lost first steering boundary: %+v", deltas)
	}
	for _, event := range []protocol.Event{
		{Type: protocol.EventTypeContentEnd},
		{Type: protocol.EventTypeToolCallEnd, ToolCall: &protocol.ToolCall{ID: "shell", Name: "shell", Arguments: "{}"}},
		{Type: protocol.EventTypeToolResultDone, ToolResult: &protocol.ToolResult{ToolCallID: "shell", ToolName: "shell", Text: "interrupted"}},
		{Type: protocol.EventTypeSteeringApplied, SteeringIDs: []string{"steer-1", "steer-2"}},
		protocol.NewTextDeltaEvent("new output"),
		{Type: protocol.EventTypeContentEnd},
	} {
		if err := display.HandleEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	items, _, err := reopened.ListDisplayItems(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	oldOutputs := 0
	for _, item := range items {
		if item.Type != zotigosession.DisplayItemAssistantMessage {
			continue
		}
		if len(item.Content) > 0 && item.Content[0].Text == "new output" {
			if item.BeforeInputID != "" {
				t.Fatal("new generation retained old steering boundary")
			}
			continue
		}
		oldOutputs++
		if publicDisplayItem(item).BeforeInputID != "steer-1" {
			t.Fatalf("persisted/API boundary missing: %+v", item)
		}
	}
	if oldOutputs != 3 {
		t.Fatalf("expected thinking, tool call and result, got %d", oldOutputs)
	}
}

func TestWorkerDisplayLogRejectsSteeringAfterBarrierFailure(t *testing.T) {
	ctx := context.Background()
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog("session-barrier-failure", source)
	if _, err := display.StartTurn(ctx); err != nil {
		t.Fatalf("start turn: %v", err)
	}
	display.QueueSteering(commandResponse{
		ID:        "steering-command",
		Type:      sessionCommandSteering,
		CreatedAt: time.Now().UTC(),
		Steering:  &steeringCommandPayload{Text: "change direction"},
	})
	var deltas []displayDeltaEvent
	display.delta = func(delta displayDeltaEvent) { deltas = append(deltas, delta) }
	display.barrier = func(context.Context) error { return errors.New("daemon unavailable") }
	err := display.HandleEvent(ctx, protocol.Event{
		Type:        protocol.EventTypeSteeringApplied,
		SteeringIDs: []string{"steering-command"},
	})
	if err == nil || !strings.Contains(err.Error(), "establish steering display boundary") {
		t.Fatalf("barrier failure = %v", err)
	}
	if len(deltas) != 0 {
		t.Fatalf("volatile delta escaped after failed barrier: %#v", deltas)
	}
	items, _, err := source.LoadItems(ctx, "session-barrier-failure")
	if err != nil {
		t.Fatalf("load items: %v", err)
	}
	if len(items) != 1 || items[0].Type != zotigosession.DisplayItemTurnStarted {
		t.Fatalf("steering became durable after barrier failure: %#v", items)
	}
}

func TestWorkerDisplayLogMutesPreviewAfterReliableSteeringWakeFailure(t *testing.T) {
	ctx := context.Background()
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog("session-steering-wake-failure", source)
	if _, err := display.StartTurn(ctx); err != nil {
		t.Fatalf("start turn: %v", err)
	}
	display.QueueSteering(commandResponse{
		ID:        "steering-command",
		Type:      sessionCommandSteering,
		CreatedAt: time.Now().UTC(),
		Steering:  &steeringCommandPayload{Text: "change direction"},
	})
	display.barrier = func(context.Context) error { return nil }
	display.wakeSync = func(context.Context) error { return errors.New("worker connection closed") }
	var deltas []displayDeltaEvent
	display.delta = func(delta displayDeltaEvent) { deltas = append(deltas, delta) }
	if err := display.HandleEvent(ctx, protocol.Event{
		Type:        protocol.EventTypeSteeringApplied,
		SteeringIDs: []string{"steering-command"},
	}); err != nil {
		t.Fatalf("apply steering: %v", err)
	}
	if err := display.HandleEvent(ctx, protocol.NewTextDeltaEvent("new response")); err != nil {
		t.Fatalf("new response delta: %v", err)
	}
	if len(deltas) != 0 {
		t.Fatalf("volatile delta escaped after reliable wake failure: %#v", deltas)
	}
	items, _, err := source.LoadItems(ctx, "session-steering-wake-failure")
	if err != nil {
		t.Fatalf("load items: %v", err)
	}
	if len(items) != 2 || items[1].Type != zotigosession.DisplayItemSteeringMessage {
		t.Fatalf("durable steering was not preserved: %#v", items)
	}
}
