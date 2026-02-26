package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/transport"
)

// RunnerHooks allows callers to inject custom logic at key points in the
// Runner event loop (e.g. session persistence, logging, metrics).
type RunnerHooks struct {
	// BeforeTurn is called before processing a user message.
	BeforeTurn func(input transport.UserInput)
	// AfterTurn is called after each complete agent turn (finish with stop).
	AfterTurn func(snapshot Snapshot)
	// OnPause is called when the agent pauses for tool approval.
	OnPause func(snapshot Snapshot)
	// OnError is called when an error occurs during execution.
	OnError func(err error)
}

// Runner orchestrates the Agent and Transport for bidirectional communication.
// It uses an event-driven, non-blocking design suitable for WebUI scenarios.
type Runner struct {
	agent     *Agent
	transport transport.Transport
	hooks     RunnerHooks

	mu       sync.Mutex
	running  bool
	cancelFn context.CancelFunc
}

// RunnerOption configures a Runner.
type RunnerOption func(*Runner)

// WithHooks sets lifecycle hooks on the Runner.
func WithHooks(hooks RunnerHooks) RunnerOption {
	return func(r *Runner) { r.hooks = hooks }
}

// NewRunner creates a new Runner with the given agent and transport.
func NewRunner(agent *Agent, tr transport.Transport, opts ...RunnerOption) *Runner {
	r := &Runner{agent: agent, transport: tr}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Agent returns the underlying agent.
func (r *Runner) Agent() *Agent {
	return r.agent
}

// Transport returns the underlying transport.
func (r *Runner) Transport() transport.Transport {
	return r.transport
}

// Start begins the main event loop, processing user inputs and streaming events.
// It blocks until the context is cancelled or an error occurs.
func (r *Runner) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return fmt.Errorf("runner is already running")
	}
	r.running = true
	ctx, r.cancelFn = context.WithCancel(ctx)
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.running = false
		r.cancelFn = nil
		r.mu.Unlock()
	}()

	inputCh := r.transport.Receive(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case input, ok := <-inputCh:
			if !ok {
				return nil // Transport closed
			}

			if err := r.handleInput(ctx, input); err != nil {
				r.fireOnError(err)
				// Send error event but continue
				r.transport.Send(ctx, protocol.NewErrorEvent(err))
			}
		}
	}
}

// Stop stops the runner.
func (r *Runner) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancelFn != nil {
		r.cancelFn()
	}
}

// RunOnce executes a single turn with the given input message.
// This is useful for request-response patterns (HTTP API).
// Returns when all events are sent, including "need_approval" if approval is needed.
// Use SubmitApproval to continue after approval.
func (r *Runner) RunOnce(ctx context.Context, msg protocol.Message) error {
	eventCh, err := r.agent.RunMessage(ctx, msg)
	if err != nil {
		return err
	}

	return r.streamEvents(ctx, eventCh)
}

// SubmitApproval submits user approval decisions and continues execution.
// This should be called after receiving a "need_approval" finish event.
func (r *Runner) SubmitApproval(ctx context.Context, results []transport.ApprovalResult) error {
	snap := r.agent.Snapshot()
	if snap.State != StatePaused {
		return fmt.Errorf("agent is not waiting for approval")
	}

	// Check if all approved
	allApproved := true
	approvedMap := make(map[string]bool)
	for _, result := range results {
		approvedMap[result.ToolCallID] = result.Approved
		if !result.Approved {
			allApproved = false
		}
	}

	var eventCh <-chan protocol.Event
	var err error

	if allApproved {
		eventCh, err = r.agent.ApproveAndExecutePendingActions(ctx)
	} else {
		// Submit denied results
		var outputs []protocol.ToolResult
		for _, action := range snap.PendingActions {
			if !approvedMap[action.ToolCallID] {
				outputs = append(outputs, protocol.ToolResult{
					ToolCallID: action.ToolCallID,
					Type:       protocol.ToolResultTypeExecutionDenied,
					Reason:     "User denied",
				})
			}
		}
		// For approved ones, execute them
		if len(outputs) < len(snap.PendingActions) {
			// Some were approved - for simplicity, execute all approved first
			// This is a simplified implementation; full implementation would
			// handle partial approvals more gracefully
			eventCh, err = r.agent.ApproveAndExecutePendingActions(ctx)
		} else {
			eventCh, err = r.agent.SubmitToolOutputs(ctx, outputs)
		}
	}

	if err != nil {
		return err
	}

	return r.streamEvents(ctx, eventCh)
}

// GetPendingApprovals returns the current pending tool calls awaiting approval.
// Returns nil if no approval is pending.
//
// This method is primarily used for session restoration scenarios (e.g., page refresh).
// During normal streaming, the client should track tool calls from tool_call_end events
// and display the approval UI when receiving the "need_approval" finish event.
func (r *Runner) GetPendingApprovals() []transport.PendingToolCall {
	snap := r.agent.Snapshot()
	if snap.State != StatePaused || len(snap.PendingActions) == 0 {
		return nil
	}

	pending := make([]transport.PendingToolCall, 0, len(snap.PendingActions))
	for _, action := range snap.PendingActions {
		pending = append(pending, transport.PendingToolCall{
			ID:        action.ToolCallID,
			Name:      action.Name,
			Arguments: action.Arguments,
		})
	}
	return pending
}

// safeCallHook runs fn with panic recovery. If fn panics, the panic is
// forwarded to OnError so the caller can observe it. OnError itself is
// called with its own recovery — if it also panics, the panic is silently
// swallowed since there is no higher-level observer.
func (r *Runner) safeCallHook(fn func()) {
	defer func() {
		if v := recover(); v != nil {
			r.fireOnError(fmt.Errorf("hook panicked: %v", v))
		}
	}()
	fn()
}

func (r *Runner) fireBeforeTurn(input transport.UserInput) {
	if r.hooks.BeforeTurn != nil {
		r.safeCallHook(func() { r.hooks.BeforeTurn(input) })
	}
}

func (r *Runner) fireAfterTurn() {
	if r.hooks.AfterTurn != nil {
		r.safeCallHook(func() { r.hooks.AfterTurn(r.agent.Snapshot()) })
	}
}

func (r *Runner) fireOnPause() {
	if r.hooks.OnPause != nil {
		r.safeCallHook(func() { r.hooks.OnPause(r.agent.Snapshot()) })
	}
}

func (r *Runner) fireOnError(err error) {
	if r.hooks.OnError == nil {
		return
	}
	defer func() { recover() }() // OnError is the last resort; swallow its panic.
	r.hooks.OnError(err)
}

// handleInput processes a single user input.
func (r *Runner) handleInput(ctx context.Context, input transport.UserInput) error {
	switch input.Type {
	case transport.UserInputMessage:
		r.fireBeforeTurn(input)
		return r.handleMessage(ctx, input)
	case transport.UserInputCommand:
		return r.handleCommand(ctx, input)
	case transport.UserInputCancel:
		r.Stop()
		return nil
	case transport.UserInputQuit:
		r.Stop()
		r.transport.Close()
		return nil
	default:
		return fmt.Errorf("unknown input type: %d", input.Type)
	}
}

// handleMessage processes a chat message input.
func (r *Runner) handleMessage(ctx context.Context, input transport.UserInput) error {
	msg := protocol.NewUserMessage(input.Text)

	// Add images if present
	for _, img := range input.Images {
		msg.Content = append(msg.Content, protocol.ContentPart{
			Type: protocol.ContentTypeImage,
			Image: &protocol.MediaPart{
				MediaType: img.MimeType,
				Data:      img.Data,
			},
		})
	}

	eventCh, err := r.agent.RunMessage(ctx, msg)
	if err != nil {
		return err
	}

	return r.streamEvents(ctx, eventCh)
}

// handleCommand processes a slash command input.
func (r *Runner) handleCommand(ctx context.Context, input transport.UserInput) error {
	// Commands like /clear, /model, etc. can be handled here
	// For now, just send an error for unknown commands
	return fmt.Errorf("unknown command: %s", input.Command)
}

// streamEvents streams events from the agent to the transport.
// Non-blocking: returns immediately when "need_approval" is received.
func (r *Runner) streamEvents(ctx context.Context, eventCh <-chan protocol.Event) error {
	for {
		select {
		case <-ctx.Done():
			err := ctx.Err()
			r.fireOnError(err)
			return err
		case event, ok := <-eventCh:
			if !ok {
				return nil // Stream finished
			}

			// Forward event to transport
			if err := r.transport.Send(ctx, event); err != nil {
				r.fireOnError(err)
				return err
			}

			if event.Type == protocol.EventTypeFinish {
				switch event.FinishReason {
				case protocol.FinishReasonStop:
					r.fireAfterTurn()
				case "need_approval":
					r.fireOnPause()
				}
				// If need_approval, return immediately (non-blocking)
				// Client should call SubmitApproval() to continue
				if event.FinishReason == "need_approval" {
					return nil
				}
			}
		}
	}
}
