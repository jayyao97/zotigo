package zotigod

import (
	"context"
	"errors"
	"testing"

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
