package agent_test

import (
	"context"
	"sync"
	"testing"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

// mixedBatchProvider answers the first request with two tool calls in one
// response and every later request with text. It keeps the messages of each
// request so a test can inspect what the model was sent.
type mixedBatchProvider struct {
	mu       sync.Mutex
	requests [][]protocol.Message
}

func (p *mixedBatchProvider) Name() string { return "mixed-batch-approval" }

func (p *mixedBatchProvider) StreamChat(_ context.Context, messages []protocol.Message, _ []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	p.mu.Lock()
	p.requests = append(p.requests, append([]protocol.Message(nil), messages...))
	first := len(p.requests) == 1
	p.mu.Unlock()

	ch := make(chan protocol.Event, 3)
	if first {
		ch <- protocol.Event{Type: protocol.EventTypeToolCallEnd, ToolCall: &protocol.ToolCall{ID: "call-safe", Name: "safe_probe", Arguments: "{}"}}
		ch <- protocol.Event{Type: protocol.EventTypeToolCallEnd, ToolCall: &protocol.ToolCall{ID: "call-gated", Name: "get_time", Arguments: "{}"}}
		ch <- protocol.NewFinishEvent(protocol.FinishReasonToolCalls)
	} else {
		ch <- protocol.NewTextDeltaEvent("done")
		ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	}
	close(ch)
	return ch, nil
}

// safeProbeTool is auto-allowed under every approval policy.
type safeProbeTool struct{ calls int }

func (t *safeProbeTool) Name() string        { return "safe_probe" }
func (t *safeProbeTool) Description() string { return "A read-only probe" }
func (t *safeProbeTool) Schema() any         { return nil }
func (t *safeProbeTool) Classify(tools.SafetyCall) tools.SafetyDecision {
	return tools.SafetyDecision{Level: tools.LevelSafe}
}
func (t *safeProbeTool) Execute(context.Context, executor.Executor, string) (any, error) {
	t.calls++
	return "probe-ok", nil
}

// A model may put an auto-allowed call and a call that needs approval in one
// response. The auto-allowed call waits with the batch; once the user
// approves, both run, and the model must receive exactly one result per call.
func TestApprovingMixedBatchSendsOneResultPerToolCall(t *testing.T) {
	provider := &mixedBatchProvider{}
	providers.Register(provider.Name(), func(config.ProfileConfig) (providers.Provider, error) { return provider, nil })

	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: provider.Name()}, exec)
	if err != nil {
		t.Fatal(err)
	}
	probe := &safeProbeTool{}
	ag.RegisterTool(probe)
	ag.RegisterTool(&TimeTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyManual)

	events, err := ag.Run(context.Background(), "probe, then tell me the time")
	if err != nil {
		t.Fatal(err)
	}
	var last protocol.Event
	for event := range events {
		last = event
	}
	if string(last.FinishReason) != "need_approval" {
		t.Fatalf("finish reason = %q, want need_approval", last.FinishReason)
	}
	snap := ag.Snapshot()
	if len(snap.PendingActions) != 1 || snap.PendingActions[0].ToolCallID != "call-gated" {
		t.Fatalf("pending actions = %+v, want only call-gated", snap.PendingActions)
	}
	if probe.calls != 0 {
		t.Fatalf("auto-allowed call ran %d times before the batch was approved", probe.calls)
	}

	resumed, err := ag.ResolvePendingActions(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for event := range resumed {
		if event.Type == protocol.EventTypeError {
			t.Fatalf("resumed turn failed: %v", event.Error)
		}
		last = event
	}
	if last.FinishReason != protocol.FinishReasonStop {
		t.Fatalf("finish reason = %q, want stop", last.FinishReason)
	}
	if probe.calls != 1 {
		t.Fatalf("auto-allowed call ran %d times, want 1", probe.calls)
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(provider.requests))
	}
	results := map[string][]protocol.ToolResult{}
	for _, message := range provider.requests[1] {
		for _, part := range message.Content {
			if part.ToolResult != nil {
				results[part.ToolResult.ToolCallID] = append(results[part.ToolResult.ToolCallID], *part.ToolResult)
			}
		}
	}
	for _, id := range []string{"call-safe", "call-gated"} {
		got := results[id]
		if len(got) != 1 {
			t.Fatalf("%s has %d tool results in the follow-up request, want 1: %+v", id, len(got), got)
		}
		if got[0].IsError || got[0].Type == protocol.ToolResultTypeExecutionDenied {
			t.Fatalf("%s result = %+v, want the executed tool's output", id, got[0])
		}
	}
}
