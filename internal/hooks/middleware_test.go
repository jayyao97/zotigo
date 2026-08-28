package hooks

import (
	"context"
	"testing"

	"github.com/jayyao97/zotigo/core/agent"
)

func TestToolMiddlewareDeniesAndEmitsPostEvent(t *testing.T) {
	dispatcher := New(Config{Version: ConfigVersion, Hooks: map[EventName][]Handler{
		PreToolUse:  {{Command: "policy"}},
		PostToolUse: {{Command: "audit", Async: false}},
	}})
	var events []Event
	dispatcher.execute = func(_ context.Context, _ Handler, event Event) processResult {
		events = append(events, event)
		if event.EventName == PreToolUse {
			return processResult{started: true, stdout: `{"decision":"deny","reason":"blocked by hook"}`}
		}
		return processResult{started: true}
	}
	middleware := ToolMiddleware(dispatcher, ToolContext{
		SessionID: "sess-test", Agent: "zotigo", CWD: "/tmp", TurnID: func() string { return "turn-test" },
	})
	nextCalled := false
	invoke := middleware(func(context.Context, *agent.ToolCall) (any, error) {
		nextCalled = true
		return "ok", nil
	})

	_, err := invoke(context.Background(), &agent.ToolCall{ToolCallID: "call-test", Name: "shell", Arguments: `{}`})
	if !agent.IsToolExecutionDenied(err) || nextCalled {
		t.Fatalf("tool should be denied before execution: err=%v next=%t", err, nextCalled)
	}
	if len(events) != 2 || events[0].EventName != PreToolUse || events[1].EventName != PostToolUse {
		t.Fatalf("unexpected events: %#v", events)
	}
	if events[1].Tool.Result == nil || events[1].Tool.Result.Status != "denied" {
		t.Fatalf("unexpected post result: %#v", events[1].Tool.Result)
	}
}
