package agent

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

// scriptedProvider returns a preconfigured sequence of stream channels
// on successive StreamChat calls. Each element is a list of events
// the provider will emit; a returnErr > 0 at that position instead
// returns an immediate error from StreamChat (simulates transport
// failure before any streaming begins).
type scriptedProvider struct {
	name      string
	scripts   [][]protocol.Event
	errs      []error
	callCount atomic.Int32
}

func (p *scriptedProvider) Name() string { return p.name }

func (p *scriptedProvider) StreamChat(ctx context.Context, messages []protocol.Message, tls []tools.Tool, opts ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	idx := int(p.callCount.Add(1) - 1)
	if idx < len(p.errs) && p.errs[idx] != nil {
		return nil, p.errs[idx]
	}
	ch := make(chan protocol.Event, 8)
	go func() {
		defer close(ch)
		if idx >= len(p.scripts) {
			return
		}
		for _, e := range p.scripts[idx] {
			ch <- e
		}
	}()
	return ch, nil
}

// newToolCallEvent returns a tool_call_end event carrying a decision
// arguments JSON the classifier can parse.
func newToolCallEvent(decision, reason string, snapshot bool) protocol.Event {
	args := fmt.Sprintf(`{"decision":%q,"reason":%q,"requires_snapshot":%v}`, decision, reason, snapshot)
	return protocol.Event{
		Type: protocol.EventTypeToolCallEnd,
		ToolCall: &protocol.ToolCall{
			Name:      classifierToolName,
			Arguments: args,
		},
	}
}

func TestClassifier_RetriesOnStreamSetupError(t *testing.T) {
	// First call fails at StreamChat setup; second call succeeds.
	prov := &scriptedProvider{
		name: "mock",
		errs: []error{errors.New("transport: connection reset"), nil},
		scripts: [][]protocol.Event{
			nil,
			{newToolCallEvent("allow", "looks fine", false), protocol.NewFinishEvent(protocol.FinishReasonStop)},
		},
	}
	c := NewProviderSafetyClassifier(prov, config.SafetyClassifierConfig{TimeoutMs: 1000})

	resp, err := c.Classify(context.Background(), SafetyClassifierRequest{ToolName: "shell", RiskLevel: "normal"})
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if resp.Decision != SafetyClassifierDecisionAllow {
		t.Errorf("expected allow, got %s", resp.Decision)
	}
	if got := prov.callCount.Load(); got != 2 {
		t.Errorf("expected 2 provider calls (original + retry), got %d", got)
	}
}

func TestClassifier_RetriesOnStreamErrorEvent(t *testing.T) {
	prov := &scriptedProvider{
		name: "mock",
		scripts: [][]protocol.Event{
			// Attempt 1: error event mid-stream.
			{protocol.NewErrorEvent(errors.New("upstream 502"))},
			// Attempt 2: proper tool-call decision.
			{newToolCallEvent("deny", "blocked", false), protocol.NewFinishEvent(protocol.FinishReasonStop)},
		},
	}
	c := NewProviderSafetyClassifier(prov, config.SafetyClassifierConfig{TimeoutMs: 1000})

	resp, err := c.Classify(context.Background(), SafetyClassifierRequest{ToolName: "shell", RiskLevel: "high"})
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if resp.Decision != SafetyClassifierDecisionDeny {
		t.Errorf("expected deny, got %s", resp.Decision)
	}
	if got := prov.callCount.Load(); got != 2 {
		t.Errorf("expected 2 provider calls, got %d", got)
	}
}

func TestClassifier_DoesNotRetryOnEmptyResponse(t *testing.T) {
	// Empty stream (no tool call, no text) is a semantic failure — the
	// same request will almost certainly produce the same bad output,
	// so retrying costs another LLM call for no expected benefit.
	prov := &scriptedProvider{
		name: "mock",
		scripts: [][]protocol.Event{
			{protocol.NewFinishEvent(protocol.FinishReasonStop)},
		},
	}
	c := NewProviderSafetyClassifier(prov, config.SafetyClassifierConfig{TimeoutMs: 1000})

	_, err := c.Classify(context.Background(), SafetyClassifierRequest{ToolName: "shell"})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := prov.callCount.Load(); got != 1 {
		t.Errorf("expected 1 provider call (no retry), got %d", got)
	}
}

func TestClassifier_DoesNotRetryOnUnparseableDecision(t *testing.T) {
	// Provider returns a tool call with malformed JSON args. Semantic
	// failure, same as empty — no retry.
	prov := &scriptedProvider{
		name: "mock",
		scripts: [][]protocol.Event{
			{
				protocol.Event{
					Type: protocol.EventTypeToolCallEnd,
					ToolCall: &protocol.ToolCall{
						Name:      classifierToolName,
						Arguments: `not-json`,
					},
				},
				protocol.NewFinishEvent(protocol.FinishReasonStop),
			},
		},
	}
	c := NewProviderSafetyClassifier(prov, config.SafetyClassifierConfig{TimeoutMs: 1000})

	_, err := c.Classify(context.Background(), SafetyClassifierRequest{ToolName: "shell"})
	if err == nil {
		t.Fatal("expected parse error")
	}
	if got := prov.callCount.Load(); got != 1 {
		t.Errorf("expected 1 provider call, got %d", got)
	}
}

func TestClassifier_GivesUpAfterMaxAttempts(t *testing.T) {
	// Every attempt fails at transport level. After
	// classifierMaxAttempts we return the last error rather than
	// retrying forever.
	prov := &scriptedProvider{
		name: "mock",
		errs: []error{
			errors.New("first failure"),
			errors.New("second failure"),
		},
	}
	c := NewProviderSafetyClassifier(prov, config.SafetyClassifierConfig{TimeoutMs: 1000})

	_, err := c.Classify(context.Background(), SafetyClassifierRequest{ToolName: "shell"})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if got := prov.callCount.Load(); got != int32(classifierMaxAttempts) {
		t.Errorf("expected %d calls, got %d", classifierMaxAttempts, got)
	}
}

func TestClassifier_DefaultTimeoutApplied(t *testing.T) {
	// TimeoutMs==0 should fall back to the sane 20s default rather
	// than instantly deadlining every call.
	prov := &scriptedProvider{
		name: "mock",
		scripts: [][]protocol.Event{
			{newToolCallEvent("allow", "ok", false), protocol.NewFinishEvent(protocol.FinishReasonStop)},
		},
	}
	c := NewProviderSafetyClassifier(prov, config.SafetyClassifierConfig{TimeoutMs: 0})
	if c.timeout <= 0 {
		t.Fatalf("expected positive timeout, got %s", c.timeout)
	}

	resp, err := c.Classify(context.Background(), SafetyClassifierRequest{ToolName: "shell"})
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if resp.Decision != SafetyClassifierDecisionAllow {
		t.Errorf("expected allow, got %s", resp.Decision)
	}
}
