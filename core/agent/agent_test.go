package agent_test

import (
	"context"
	"testing"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

// --- Mock Provider ---

type StepMockProvider struct {
	Step int
}

func (p *StepMockProvider) Name() string { return "mock" }

func (p *StepMockProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event, 10)

	go func() {
		defer close(ch)
		p.Step++

		if p.Step == 1 {
			ch <- protocol.Event{
				Type:  protocol.EventTypeToolCallDelta,
				Index: 0,
				ToolCallDelta: &protocol.ToolCallDelta{
					ID:   "call_1",
					Name: "get_time",
				},
			}
			ch <- protocol.Event{
				Type:  protocol.EventTypeToolCallEnd,
				Index: 0,
				ToolCall: &protocol.ToolCall{
					ID:        "call_1",
					Name:      "get_time",
					Arguments: "{}",
				},
			}
			ch <- protocol.NewFinishEvent(protocol.FinishReasonToolCalls)
		} else {
			ch <- protocol.NewTextDeltaEvent("It is 12:00")
			ch <- protocol.Event{
				Type:        protocol.EventTypeContentEnd,
				ContentPart: &protocol.ContentPart{Type: protocol.ContentTypeText, Text: "It is 12:00"},
			}
			ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
		}
	}()

	return ch, nil
}

// --- Mock Tool ---

type TimeTool struct{}

func (t *TimeTool) Name() string             { return "get_time" }
func (t *TimeTool) Description() string      { return "Returns current time" }
func (t *TimeTool) Schema() any              { return nil }
func (t *TimeTool) Safety() tools.ToolSafety { return tools.ToolSafety{ReadOnly: false} }
func (t *TimeTool) Execute(ctx context.Context, exec executor.Executor, args string) (any, error) {
	return "12:00", nil
}

// --- Test ---

func TestAgentReActLoop(t *testing.T) {
	providers.Register("mock", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &StepMockProvider{}, nil
	})

	// Create a temp directory for executor
	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "mock"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	ag.RegisterTool(&TimeTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyManual)

	events, err := ag.Run(context.Background(), "What time is it?")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	var lastEvent protocol.Event
	for e := range events {
		lastEvent = e
	}

	if string(lastEvent.FinishReason) != "need_approval" {
		t.Fatalf("Expected need_approval, got %s", lastEvent.FinishReason)
	}

	toolRes := protocol.NewTextToolResult("call_1", "12:00", false)

	events2, err := ag.SubmitToolOutputs(context.Background(), []protocol.ToolResult{toolRes})
	if err != nil {
		t.Fatalf("Submit error: %v", err)
	}

	var content string
	for e := range events2 {
		if e.Type == protocol.EventTypeContentDelta {
			content += e.ContentPartDelta.Text
		}
		lastEvent = e
	}

	if lastEvent.FinishReason != protocol.FinishReasonStop {
		t.Errorf("Expected stop, got %s", lastEvent.FinishReason)
	}
	if content != "It is 12:00" {
		t.Errorf("Expected 'It is 12:00', got '%s'", content)
	}
}

// ============ Compression Integration Tests ============

func TestAgentGetContextStats(t *testing.T) {
	providers.Register("mock-stats", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &StatsMockProvider{}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "mock-stats"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	// Initial stats
	stats := ag.GetContextStats()
	if stats["message_count"] != 0 {
		t.Errorf("Expected 0 messages initially, got %d", stats["message_count"])
	}

	// Run a conversation
	events, err := ag.Run(context.Background(), "Hello")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	for range events {
		// Drain events
	}

	// Stats should update
	stats = ag.GetContextStats()
	if stats["message_count"] < 2 {
		t.Errorf("Expected at least 2 messages (user + assistant), got %d", stats["message_count"])
	}
	if stats["estimated_tokens"] <= 0 {
		t.Errorf("Expected positive token count, got %d", stats["estimated_tokens"])
	}

	t.Logf("Context stats: messages=%d, tokens=%d", stats["message_count"], stats["estimated_tokens"])
}

func TestAgentForceCompress(t *testing.T) {
	providers.Register("mock-compress", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &VerboseMockProvider{}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "mock-compress"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	// Run multiple conversations to build up history
	for i := 0; i < 5; i++ {
		events, err := ag.Run(context.Background(), "Tell me a long story about topic "+string(rune('A'+i)))
		if err != nil {
			t.Fatalf("Run error: %v", err)
		}
		for range events {
			// Drain events
		}
	}

	// Get stats before compression
	statsBefore := ag.GetContextStats()
	t.Logf("Before compression: messages=%d, tokens=%d",
		statsBefore["message_count"], statsBefore["estimated_tokens"])

	// Force compress
	result, err := ag.ForceCompress(context.Background())
	if err != nil {
		t.Fatalf("ForceCompress error: %v", err)
	}

	t.Logf("Compression result: compressed=%v, before=%d, after=%d tokens",
		result.Compressed, result.OriginalTokens, result.CompressedTokens)

	// If compression happened, verify tokens reduced
	if result.Compressed {
		if result.CompressedTokens >= result.OriginalTokens {
			t.Errorf("Compression should reduce tokens: %d >= %d",
				result.CompressedTokens, result.OriginalTokens)
		}

		statsAfter := ag.GetContextStats()
		t.Logf("After compression: messages=%d, tokens=%d",
			statsAfter["message_count"], statsAfter["estimated_tokens"])
	}
}

func TestAgentResetLoopDetector(t *testing.T) {
	providers.Register("mock-loop", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &StatsMockProvider{}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "mock-loop"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	// Reset should not panic even with no history
	ag.ResetLoopDetector()

	// Run a conversation
	events, _ := ag.Run(context.Background(), "Test")
	for range events {
	}

	// Reset again
	ag.ResetLoopDetector()

	// Loop stats should be reset
	stats := ag.GetContextStats()
	if loopCalls, ok := stats["loop_total_calls"]; ok && loopCalls > 0 {
		t.Logf("Loop stats after reset: %v", stats)
	}
}

// --- Additional Mock Providers ---

type StatsMockProvider struct{}

func (p *StatsMockProvider) Name() string { return "stats-mock" }

func (p *StatsMockProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event, 10)
	go func() {
		defer close(ch)
		ch <- protocol.NewTextDeltaEvent("Hello! I'm a helpful assistant.")
		ch <- protocol.Event{
			Type:        protocol.EventTypeContentEnd,
			ContentPart: &protocol.ContentPart{Type: protocol.ContentTypeText, Text: "Hello! I'm a helpful assistant."},
		}
		ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	}()
	return ch, nil
}

type VerboseMockProvider struct{}

func (p *VerboseMockProvider) Name() string { return "verbose-mock" }

func (p *VerboseMockProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event, 10)
	go func() {
		defer close(ch)
		// Generate a long response to build up token count
		longResponse := "This is a very detailed and verbose response. " +
			"Let me explain in great detail about the topic you mentioned. " +
			"Here are some important points to consider: First, we need to understand the basics. " +
			"Second, we should explore the implications. Third, let's look at some examples. " +
			"Finally, we can draw some conclusions from our analysis. " +
			"I hope this explanation was helpful and comprehensive!"
		ch <- protocol.NewTextDeltaEvent(longResponse)
		ch <- protocol.Event{
			Type:        protocol.EventTypeContentEnd,
			ContentPart: &protocol.ContentPart{Type: protocol.ContentTypeText, Text: longResponse},
		}
		ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	}()
	return ch, nil
}
