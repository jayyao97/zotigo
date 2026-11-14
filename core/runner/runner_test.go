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

func (p *SimpleMockProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
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

func (p *ToolCallMockProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
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

func TestRunner_RunFullTurnStarted(t *testing.T) {
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

	// Run in goroutine since RunFullTurnStarted sends events
	done := make(chan error, 1)
	go func() {
		done <- r.RunFullTurnStarted(ctx, protocol.NewUserMessage("Hi"), nil)
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

	// Wait for RunFullTurnStarted to complete
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunFullTurnStarted error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for RunFullTurnStarted to complete")
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
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- r.RunFullTurnStarted(ctx, protocol.NewUserMessage("Do something"), nil)
	}()

	// These are liveness guards, not latency assertions. Race-enabled CI
	// may spend seconds scheduling each phase under package-level load.
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	nextEvent := func(phase string) protocol.Event {
		t.Helper()
		select {
		case event := <-tr.Events():
			return event
		case <-timeout.C:
			t.Fatalf("Timeout %s", phase)
		}
		return protocol.Event{}
	}

	// Step 1: the agent's stream ends with need_approval.
	var lastEvent protocol.Event
	for {
		lastEvent = nextEvent("waiting for events")
		if lastEvent.Type == protocol.EventTypeFinish {
			break
		}
	}
	if lastEvent.FinishReason != "need_approval" {
		t.Fatalf("Expected need_approval, got %s", lastEvent.FinishReason)
	}

	// The runner then asks the transport for approval; ChannelTransport
	// surfaces that request as its own need_approval finish event.
	request := nextEvent("waiting for the transport approval request")
	if request.Type != protocol.EventTypeFinish || request.FinishReason != "need_approval" {
		t.Fatalf("Expected transport approval request, got %#v", request)
	}

	// The turn stays open until the approval is answered.
	select {
	case err := <-done:
		t.Fatalf("RunFullTurnStarted returned before approval was answered: %v", err)
	default:
	}

	// Verify the pending tool calls awaiting approval
	snap := ag.Snapshot()
	if snap.State != agent.StatePaused {
		t.Fatalf("Expected state paused, got %s", snap.State)
	}
	if len(snap.PendingActions) != 1 {
		t.Fatalf("Expected 1 pending approval, got %d", len(snap.PendingActions))
	}
	if snap.PendingActions[0].Name != "test_tool" {
		t.Errorf("Expected test_tool, got %s", snap.PendingActions[0].Name)
	}
	if snap.PendingActions[0].ToolCallID != "call_1" {
		t.Errorf("Expected call_1, got %s", snap.PendingActions[0].ToolCallID)
	}

	// The approval continuation gets its own budget; do not reuse the
	// deadline that started before the initial model response.
	timeout.Reset(10 * time.Second)

	// Step 2: Answer the approval request through the transport
	if err := tr.SendApproval(ctx, []transport.ApprovalResult{
		{ToolCallID: "call_1", Approved: true},
	}); err != nil {
		t.Fatalf("SendApproval error: %v", err)
	}

	// Collect events until final finish
	var toolResult *protocol.ToolResult
	for {
		lastEvent = nextEvent("waiting for final events")
		if lastEvent.Type == protocol.EventTypeToolResultDone {
			toolResult = lastEvent.ToolResult
		}
		if lastEvent.Type == protocol.EventTypeFinish && lastEvent.FinishReason == protocol.FinishReasonStop {
			break
		}
	}

	// Wait for RunFullTurnStarted to complete
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunFullTurnStarted error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Timeout waiting for RunFullTurnStarted to complete")
	}

	// Verify final state
	if lastEvent.FinishReason != protocol.FinishReasonStop {
		t.Errorf("Expected stop, got %s", lastEvent.FinishReason)
	}

	// The approved tool ran and its result was delivered
	if toolResult == nil {
		t.Fatal("Expected a tool result event after approval")
	}
	if toolResult.ToolCallID != "call_1" || toolResult.IsError || toolResult.Text != "tool result" {
		t.Errorf("Expected successful result of call_1, got %#v", toolResult)
	}

	// No more pending approvals
	snap = ag.Snapshot()
	if snap.State == agent.StatePaused {
		t.Errorf("Expected agent to leave paused state, got %s", snap.State)
	}
	if len(snap.PendingActions) != 0 {
		t.Errorf("Expected no pending approvals, got %d", len(snap.PendingActions))
	}
}

func TestRunner_ApprovalFlow_Denied(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- r.RunFullTurnStarted(ctx, protocol.NewUserMessage("Do something"), nil)
	}()

	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	nextEvent := func(phase string) protocol.Event {
		t.Helper()
		select {
		case event := <-tr.Events():
			return event
		case <-timeout.C:
			t.Fatalf("Timeout %s", phase)
		}
		return protocol.Event{}
	}

	for approvalRequests := 0; approvalRequests < 2; {
		event := nextEvent("waiting for the transport approval request")
		if event.Type != protocol.EventTypeFinish {
			continue
		}
		if event.FinishReason != "need_approval" {
			t.Fatalf("Expected need_approval, got %s", event.FinishReason)
		}
		approvalRequests++
	}

	timeout.Reset(10 * time.Second)
	if err := tr.SendApproval(ctx, []transport.ApprovalResult{
		{ToolCallID: "call_1", Approved: false, Reason: "not now"},
	}); err != nil {
		t.Fatalf("SendApproval error: %v", err)
	}

	var toolResult *protocol.ToolResult
	for {
		event := nextEvent("waiting for final events")
		if event.Type == protocol.EventTypeToolResultDone {
			toolResult = event.ToolResult
		}
		if event.Type == protocol.EventTypeFinish {
			if event.FinishReason != protocol.FinishReasonStop {
				t.Fatalf("Expected stop, got %s", event.FinishReason)
			}
			break
		}
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunFullTurnStarted error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Timeout waiting for RunFullTurnStarted to complete")
	}

	if toolResult == nil {
		t.Fatal("Expected a denied tool result event")
	}
	if toolResult.ToolCallID != "call_1" ||
		toolResult.ToolName != "test_tool" ||
		toolResult.Type != protocol.ToolResultTypeExecutionDenied ||
		toolResult.Reason != "not now" ||
		!toolResult.IsError {
		t.Errorf("Expected execution-denied result for call_1, got %#v", toolResult)
	}
	if len(ag.Snapshot().PendingActions) != 0 {
		t.Errorf("Expected no pending approvals, got %d", len(ag.Snapshot().PendingActions))
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

func TestRunner_Listeners_AfterTurn(t *testing.T) {
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
	listeners := runner.Listeners{
		AfterTurn: func(snap agent.Snapshot) {
			called = true
			gotSnapshot = snap
		},
	}

	r := runner.New(ag, tr, runner.WithListeners(listeners))
	ctx := context.Background()

	done := make(chan error, 1)
	go func() {
		done <- r.RunFullTurnStarted(ctx, protocol.NewUserMessage("Hi"), nil)
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
			t.Fatalf("RunFullTurnStarted error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for RunFullTurnStarted")
	}

	if !called {
		t.Fatal("AfterTurn hook was not called")
	}
	if len(gotSnapshot.History) == 0 {
		t.Error("Expected non-empty history in snapshot")
	}
}

func TestRunner_Listeners_OnPause(t *testing.T) {
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

	paused := make(chan agent.Snapshot, 1)
	listeners := runner.Listeners{
		OnPause: func(snap agent.Snapshot) {
			paused <- snap
		},
	}

	r := runner.New(ag, tr, runner.WithListeners(listeners))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- r.RunFullTurnStarted(ctx, protocol.NewUserMessage("Do something"), nil)
	}()

	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	nextEvent := func(phase string) protocol.Event {
		t.Helper()
		select {
		case event := <-tr.Events():
			return event
		case <-timeout.C:
			t.Fatalf("Timeout %s", phase)
		}
		return protocol.Event{}
	}

	for {
		event := nextEvent("waiting for events")
		if event.Type == protocol.EventTypeFinish {
			if event.FinishReason != "need_approval" {
				t.Fatalf("Expected need_approval, got %s", event.FinishReason)
			}
			break
		}
	}

	var gotSnapshot agent.Snapshot
	select {
	case gotSnapshot = <-paused:
	case err := <-done:
		t.Fatalf("RunFullTurnStarted returned before OnPause fired: %v", err)
	case <-timeout.C:
		t.Fatal("OnPause hook was not called")
	}
	if gotSnapshot.State != agent.StatePaused {
		t.Errorf("Expected state paused, got %s", gotSnapshot.State)
	}

	// The turn is still waiting on the transport; answer so it can finish.
	timeout.Reset(10 * time.Second)
	if err := tr.SendApproval(ctx, []transport.ApprovalResult{
		{ToolCallID: "call_1", Approved: true},
	}); err != nil {
		t.Fatalf("SendApproval error: %v", err)
	}
	for {
		event := nextEvent("waiting for final events")
		if event.Type == protocol.EventTypeFinish && event.FinishReason == protocol.FinishReasonStop {
			break
		}
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunFullTurnStarted error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Timeout waiting for RunFullTurnStarted")
	}

	select {
	case extra := <-paused:
		t.Errorf("OnPause fired more than once, second snapshot state %s", extra.State)
	default:
	}
}

func TestRunner_Listeners_BeforeTurn(t *testing.T) {
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
	listeners := runner.Listeners{
		BeforeTurn: func(input transport.UserInput) {
			called = true
			gotInput = input
		},
	}

	r := runner.New(ag, tr, runner.WithListeners(listeners))

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

func TestRunner_Listeners_OnError(t *testing.T) {
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
	listeners := runner.Listeners{
		OnError: func(err error) {
			gotErr = err
		},
	}

	r := runner.New(ag, tr, runner.WithListeners(listeners))

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

func TestRunner_Listeners_PanicRecover(t *testing.T) {
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
	listeners := runner.Listeners{
		AfterTurn: func(snap agent.Snapshot) {
			panic("boom")
		},
		OnError: func(err error) {
			recoveredErr = err
		},
	}

	r := runner.New(ag, tr, runner.WithListeners(listeners))
	ctx := context.Background()

	done := make(chan error, 1)
	go func() {
		done <- r.RunFullTurnStarted(ctx, protocol.NewUserMessage("Hi"), nil)
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
			t.Fatalf("RunFullTurnStarted error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for RunFullTurnStarted")
	}

	// AfterTurn panicked -> should be recovered and forwarded to OnError
	if recoveredErr == nil {
		t.Fatal("OnError was not called after AfterTurn panic")
	}
	if recoveredErr.Error() != "listener panicked: boom" {
		t.Errorf("Expected 'listener panicked: boom', got '%s'", recoveredErr.Error())
	}
}

func TestRunner_Listeners_Nil(t *testing.T) {
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
		done <- r.RunFullTurnStarted(ctx, protocol.NewUserMessage("Hi"), nil)
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
			t.Fatalf("RunFullTurnStarted error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for RunFullTurnStarted")
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
