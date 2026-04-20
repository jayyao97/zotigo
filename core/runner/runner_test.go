package runner_test

import (
	"context"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/runner"
	"github.com/jayyao97/zotigo/core/tools"
	"github.com/jayyao97/zotigo/core/transport"
)

// SimpleMockProvider returns a simple text response
type SimpleMockProvider struct{}

func (p *SimpleMockProvider) Name() string { return "simple_mock" }

func (p *SimpleMockProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event, 10)
	go func() {
		defer close(ch)
		ch <- protocol.NewTextDeltaEvent("Hello!")
		ch <- protocol.Event{
			Type:        protocol.EventTypeContentEnd,
			ContentPart: &protocol.ContentPart{Type: protocol.ContentTypeText, Text: "Hello!"},
		}
		ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	}()
	return ch, nil
}

// ToolCallMockProvider returns a tool call that needs approval
type ToolCallMockProvider struct {
	step int
}

func (p *ToolCallMockProvider) Name() string { return "toolcall_mock" }

func (p *ToolCallMockProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event, 10)
	go func() {
		defer close(ch)
		p.step++

		if p.step == 1 {
			// First call: return a tool call
			ch <- protocol.Event{
				Type:  protocol.EventTypeToolCallDelta,
				Index: 0,
				ToolCallDelta: &protocol.ToolCallDelta{
					ID:   "call_1",
					Name: "test_tool",
				},
			}
			ch <- protocol.Event{
				Type:  protocol.EventTypeToolCallEnd,
				Index: 0,
				ToolCall: &protocol.ToolCall{
					ID:        "call_1",
					Name:      "test_tool",
					Arguments: "{}",
				},
			}
			ch <- protocol.NewFinishEvent(protocol.FinishReasonToolCalls)
		} else {
			// After tool execution: return final response
			ch <- protocol.NewTextDeltaEvent("Done!")
			ch <- protocol.Event{
				Type:        protocol.EventTypeContentEnd,
				ContentPart: &protocol.ContentPart{Type: protocol.ContentTypeText, Text: "Done!"},
			}
			ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
		}
	}()
	return ch, nil
}

// MockTool is a simple test tool
type MockTool struct{}

func (t *MockTool) Name() string        { return "test_tool" }
func (t *MockTool) Description() string { return "A test tool" }
func (t *MockTool) Schema() any         { return nil }
func (t *MockTool) Classify(_ tools.SafetyCall) tools.SafetyDecision {
	return tools.SafetyDecision{Level: tools.LevelMedium}
}
func (t *MockTool) Execute(ctx context.Context, exec executor.Executor, args string) (any, error) {
	return "tool result", nil
}

func TestRunner_RunOnce(t *testing.T) {
	providers.Register("simple_mock", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &SimpleMockProvider{}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "simple_mock"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	tr := transport.NewChannelTransport(10)
	defer tr.Close()

	r := runner.New(ag, tr)

	ctx := context.Background()

	// Run in goroutine since RunOnce sends events
	done := make(chan error, 1)
	go func() {
		done <- r.RunOnce(ctx, protocol.NewUserMessage("Hi"))
	}()

	// Collect events
	var events []protocol.Event
	timeout := time.After(2 * time.Second)

collectLoop:
	for {
		select {
		case event := <-tr.Events():
			events = append(events, event)
			if event.Type == protocol.EventTypeFinish {
				break collectLoop
			}
		case <-timeout:
			t.Fatal("Timeout waiting for events")
		}
	}

	// Wait for RunOnce to complete
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunOnce error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for RunOnce to complete")
	}

	// Verify events
	if len(events) < 2 {
		t.Errorf("Expected at least 2 events, got %d", len(events))
	}

	// Should have content delta and finish
	hasContent := false
	hasFinish := false
	for _, e := range events {
		if e.Type == protocol.EventTypeContentDelta {
			hasContent = true
		}
		if e.Type == protocol.EventTypeFinish {
			hasFinish = true
		}
	}

	if !hasContent {
		t.Error("Expected content delta event")
	}
	if !hasFinish {
		t.Error("Expected finish event")
	}
}

func TestRunner_ApprovalFlow(t *testing.T) {
	mockProvider := &ToolCallMockProvider{}
	providers.Register("toolcall_mock", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return mockProvider, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "toolcall_mock"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	ag.RegisterTool(&MockTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyManual)

	tr := transport.NewChannelTransport(10)
	defer tr.Close()

	r := runner.New(ag, tr)
	ctx := context.Background()

	// Step 1: Run and expect need_approval
	done := make(chan error, 1)
	go func() {
		done <- r.RunOnce(ctx, protocol.NewUserMessage("Do something"))
	}()

	// Collect events until need_approval
	var lastEvent protocol.Event
	timeout := time.After(2 * time.Second)

collectLoop1:
	for {
		select {
		case event := <-tr.Events():
			lastEvent = event
			if event.Type == protocol.EventTypeFinish {
				break collectLoop1
			}
		case <-timeout:
			t.Fatal("Timeout waiting for events")
		}
	}

	// Wait for RunOnce to return (non-blocking)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunOnce error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout - RunOnce should return immediately on need_approval")
	}

	// Verify we got need_approval
	if lastEvent.FinishReason != "need_approval" {
		t.Fatalf("Expected need_approval, got %s", lastEvent.FinishReason)
	}

	// Verify GetPendingApprovals returns the pending tool calls
	pending := r.GetPendingApprovals()
	if len(pending) != 1 {
		t.Fatalf("Expected 1 pending approval, got %d", len(pending))
	}
	if pending[0].Name != "test_tool" {
		t.Errorf("Expected test_tool, got %s", pending[0].Name)
	}

	// Step 2: Submit approval
	go func() {
		done <- r.SubmitApproval(ctx, []transport.ApprovalResult{
			{ToolCallID: "call_1", Approved: true},
		})
	}()

	// Collect events until final finish
collectLoop2:
	for {
		select {
		case event := <-tr.Events():
			lastEvent = event
			if event.Type == protocol.EventTypeFinish && event.FinishReason == protocol.FinishReasonStop {
				break collectLoop2
			}
		case <-timeout:
			t.Fatal("Timeout waiting for final events")
		}
	}

	// Wait for SubmitApproval to complete
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SubmitApproval error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for SubmitApproval to complete")
	}

	// Verify final state
	if lastEvent.FinishReason != protocol.FinishReasonStop {
		t.Errorf("Expected stop, got %s", lastEvent.FinishReason)
	}

	// No more pending approvals
	pending = r.GetPendingApprovals()
	if pending != nil {
		t.Errorf("Expected no pending approvals, got %d", len(pending))
	}
}

func TestRunner_Start(t *testing.T) {
	providers.Register("simple_mock", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &SimpleMockProvider{}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "simple_mock"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	tr := transport.NewChannelTransport(10)
	defer tr.Close()

	r := runner.New(ag, tr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start runner in goroutine
	done := make(chan error, 1)
	go func() {
		done <- r.Start(ctx)
	}()

	// Give runner time to start
	time.Sleep(50 * time.Millisecond)

	// Send a message
	err = tr.SendInput(ctx, transport.UserInput{
		Type: transport.UserInputMessage,
		Text: "Hello",
	})
	if err != nil {
		t.Fatalf("SendInput error: %v", err)
	}

	// Collect events
	var events []protocol.Event
	timeout := time.After(2 * time.Second)

collectLoop:
	for {
		select {
		case event := <-tr.Events():
			events = append(events, event)
			if event.Type == protocol.EventTypeFinish {
				break collectLoop
			}
		case <-timeout:
			t.Fatal("Timeout waiting for events")
		}
	}

	// Stop runner
	cancel()

	// Wait for runner to stop
	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Fatalf("Start error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for runner to stop")
	}

	// Verify we got events
	if len(events) < 2 {
		t.Errorf("Expected at least 2 events, got %d", len(events))
	}
}

func TestRunner_Hooks_AfterTurn(t *testing.T) {
	providers.Register("simple_mock", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &SimpleMockProvider{}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "simple_mock"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	tr := transport.NewChannelTransport(10)
	defer tr.Close()

	var gotSnapshot agent.Snapshot
	called := false
	hooks := runner.Hooks{
		AfterTurn: func(snap agent.Snapshot) {
			called = true
			gotSnapshot = snap
		},
	}

	r := runner.New(ag, tr, runner.WithHooks(hooks))
	ctx := context.Background()

	done := make(chan error, 1)
	go func() {
		done <- r.RunOnce(ctx, protocol.NewUserMessage("Hi"))
	}()

	// Drain events
	timeout := time.After(2 * time.Second)
drainLoop:
	for {
		select {
		case event := <-tr.Events():
			if event.Type == protocol.EventTypeFinish {
				break drainLoop
			}
		case <-timeout:
			t.Fatal("Timeout waiting for events")
		}
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunOnce error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for RunOnce")
	}

	if !called {
		t.Fatal("AfterTurn hook was not called")
	}
	if len(gotSnapshot.History) == 0 {
		t.Error("Expected non-empty history in snapshot")
	}
}

func TestRunner_Hooks_OnPause(t *testing.T) {
	mockProvider := &ToolCallMockProvider{}
	providers.Register("toolcall_mock", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return mockProvider, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "toolcall_mock"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	ag.RegisterTool(&MockTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyManual)

	tr := transport.NewChannelTransport(10)
	defer tr.Close()

	var gotSnapshot agent.Snapshot
	called := false
	hooks := runner.Hooks{
		OnPause: func(snap agent.Snapshot) {
			called = true
			gotSnapshot = snap
		},
	}

	r := runner.New(ag, tr, runner.WithHooks(hooks))
	ctx := context.Background()

	done := make(chan error, 1)
	go func() {
		done <- r.RunOnce(ctx, protocol.NewUserMessage("Do something"))
	}()

	timeout := time.After(2 * time.Second)
drainLoop:
	for {
		select {
		case event := <-tr.Events():
			if event.Type == protocol.EventTypeFinish {
				break drainLoop
			}
		case <-timeout:
			t.Fatal("Timeout waiting for events")
		}
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunOnce error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for RunOnce")
	}

	if !called {
		t.Fatal("OnPause hook was not called")
	}
	if gotSnapshot.State != agent.StatePaused {
		t.Errorf("Expected state paused, got %s", gotSnapshot.State)
	}
}

func TestRunner_Hooks_BeforeTurn(t *testing.T) {
	providers.Register("simple_mock", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &SimpleMockProvider{}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "simple_mock"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	tr := transport.NewChannelTransport(10)
	defer tr.Close()

	var gotInput transport.UserInput
	called := false
	hooks := runner.Hooks{
		BeforeTurn: func(input transport.UserInput) {
			called = true
			gotInput = input
		},
	}

	r := runner.New(ag, tr, runner.WithHooks(hooks))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- r.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	err = tr.SendInput(ctx, transport.UserInput{
		Type: transport.UserInputMessage,
		Text: "Hello hooks",
	})
	if err != nil {
		t.Fatalf("SendInput error: %v", err)
	}

	// Drain events
	timeout := time.After(2 * time.Second)
drainLoop:
	for {
		select {
		case event := <-tr.Events():
			if event.Type == protocol.EventTypeFinish {
				break drainLoop
			}
		case <-timeout:
			t.Fatal("Timeout waiting for events")
		}
	}

	cancel()

	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Fatalf("Start error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for runner to stop")
	}

	if !called {
		t.Fatal("BeforeTurn hook was not called")
	}
	if gotInput.Text != "Hello hooks" {
		t.Errorf("Expected input text 'Hello hooks', got '%s'", gotInput.Text)
	}
}

func TestRunner_Hooks_OnError(t *testing.T) {
	providers.Register("simple_mock", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &SimpleMockProvider{}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "simple_mock"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	tr := transport.NewChannelTransport(10)
	defer tr.Close()

	var gotErr error
	hooks := runner.Hooks{
		OnError: func(err error) {
			gotErr = err
		},
	}

	r := runner.New(ag, tr, runner.WithHooks(hooks))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- r.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	// Send an unknown command to trigger handleInput error -> OnError
	err = tr.SendInput(ctx, transport.UserInput{
		Type:    transport.UserInputCommand,
		Command: "/bogus",
	})
	if err != nil {
		t.Fatalf("SendInput error: %v", err)
	}

	// Drain the error event the runner sends back
	timeout := time.After(2 * time.Second)
	select {
	case <-tr.Events():
	case <-timeout:
		t.Fatal("Timeout waiting for error event")
	}

	cancel()

	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Fatalf("Start error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for runner to stop")
	}

	if gotErr == nil {
		t.Fatal("OnError hook was not called")
	}
	if gotErr.Error() != "unknown command: /bogus" {
		t.Errorf("Expected 'unknown command: /bogus', got '%s'", gotErr.Error())
	}
}

func TestRunner_Hooks_PanicRecover(t *testing.T) {
	providers.Register("simple_mock", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &SimpleMockProvider{}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "simple_mock"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	tr := transport.NewChannelTransport(10)
	defer tr.Close()

	var recoveredErr error
	hooks := runner.Hooks{
		AfterTurn: func(snap agent.Snapshot) {
			panic("boom")
		},
		OnError: func(err error) {
			recoveredErr = err
		},
	}

	r := runner.New(ag, tr, runner.WithHooks(hooks))
	ctx := context.Background()

	done := make(chan error, 1)
	go func() {
		done <- r.RunOnce(ctx, protocol.NewUserMessage("Hi"))
	}()

	timeout := time.After(2 * time.Second)
drainLoop:
	for {
		select {
		case event := <-tr.Events():
			if event.Type == protocol.EventTypeFinish {
				break drainLoop
			}
		case <-timeout:
			t.Fatal("Timeout waiting for events")
		}
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunOnce error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for RunOnce")
	}

	// AfterTurn panicked -> should be recovered and forwarded to OnError
	if recoveredErr == nil {
		t.Fatal("OnError was not called after AfterTurn panic")
	}
	if recoveredErr.Error() != "hook panicked: boom" {
		t.Errorf("Expected 'hook panicked: boom', got '%s'", recoveredErr.Error())
	}
}

func TestRunner_Hooks_Nil(t *testing.T) {
	providers.Register("simple_mock", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &SimpleMockProvider{}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "simple_mock"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	tr := transport.NewChannelTransport(10)
	defer tr.Close()

	// No hooks — should not panic
	r := runner.New(ag, tr)

	ctx := context.Background()

	done := make(chan error, 1)
	go func() {
		done <- r.RunOnce(ctx, protocol.NewUserMessage("Hi"))
	}()

	timeout := time.After(2 * time.Second)
drainLoop:
	for {
		select {
		case event := <-tr.Events():
			if event.Type == protocol.EventTypeFinish {
				break drainLoop
			}
		case <-timeout:
			t.Fatal("Timeout waiting for events")
		}
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunOnce error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for RunOnce")
	}
}

func TestRunner_Stop(t *testing.T) {
	providers.Register("simple_mock", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &SimpleMockProvider{}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "simple_mock"}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	tr := transport.NewChannelTransport(10)
	defer tr.Close()

	r := runner.New(ag, tr)

	ctx := context.Background()

	// Start runner
	done := make(chan error, 1)
	go func() {
		done <- r.Start(ctx)
	}()

	// Give runner time to start
	time.Sleep(50 * time.Millisecond)

	// Stop runner
	r.Stop()

	// Wait for runner to stop
	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Fatalf("Unexpected error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for runner to stop")
	}
}
