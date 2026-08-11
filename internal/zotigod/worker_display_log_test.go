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
