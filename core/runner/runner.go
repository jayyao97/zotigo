// Package runner orchestrates an Agent and Transport for bidirectional
// communication. It handles the event loop, approval flow, and lifecycle hooks.
//
// The Runner sits between the agent (conversation logic) and transport
// (communication channel), keeping both packages decoupled.
package runner

import (
	"context"
	"fmt"
	"sync"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/transport"
)

// Hooks allows callers to inject custom logic at key points in the
// event loop (e.g. session persistence, logging, metrics).
type Hooks struct {
	// BeforeTurn is called before processing a user message.
	BeforeTurn func(input transport.UserInput)
	// AfterTurn is called after each complete agent turn (finish with stop).
	AfterTurn func(snapshot agent.Snapshot)
	// OnPause is called when the agent pauses for tool approval.
	OnPause func(snapshot agent.Snapshot)
	// OnError is called when an error occurs during execution.
	OnError func(err error)
}

// Runner orchestrates the Agent and Transport for bidirectional communication.
type Runner struct {
	agent     *agent.Agent
	transport transport.Transport
	hooks     Hooks

	mu       sync.Mutex
	running  bool
	cancelFn context.CancelFunc
}

// Option configures a Runner.
type Option func(*Runner)

// WithHooks sets lifecycle hooks on the Runner.
func WithHooks(hooks Hooks) Option {
	return func(r *Runner) { r.hooks = hooks }
}

// New creates a new Runner with the given agent and transport.
func New(ag *agent.Agent, tr transport.Transport, opts ...Option) *Runner {
	r := &Runner{agent: ag, transport: tr}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Agent returns the underlying agent.
func (r *Runner) Agent() *agent.Agent {
	return r.agent
}

// Transport returns the underlying transport.
func (r *Runner) Transport() transport.Transport {
	return r.transport
}

// Start begins the main event loop, processing user inputs and streaming events.
// When an agent turn requires tool approval, it automatically calls
// transport.RequestApproval() and continues — callers never need to manage
// the approval loop externally.
//
// Blocks until the context is cancelled or the transport is closed.
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
				_ = r.transport.Send(ctx, protocol.NewErrorEvent(err))
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
// Returns when all events are sent, including "need_approval" if approval is needed.
// Use SubmitApproval to continue after approval.
//
// This is useful for serverless/HTTP patterns where the process may be killed
// between approval cycles.
func (r *Runner) RunOnce(ctx context.Context, msg protocol.Message) error {
	eventCh, err := r.agent.RunMessage(ctx, msg)
	if err != nil {
		return err
	}

	_, err = r.streamEvents(ctx, eventCh, false)
	return err
}

// RunFullTurn executes a complete conversation turn, automatically handling
// the approval loop via transport.RequestApproval(). Blocks until the turn
// truly finishes (no more tool calls).
//
// This is suitable for long-lived transports (ACP, WebSocket) where the
// transport can respond to approval requests synchronously.
func (r *Runner) RunFullTurn(ctx context.Context, msg protocol.Message) error {
	eventCh, err := r.agent.RunMessage(ctx, msg)
	if err != nil {
		return err
	}

	return r.drainWithApprovalLoop(ctx, eventCh)
}

// SubmitApproval submits user approval decisions and continues execution.
// This should be called after RunOnce returns with a "need_approval" finish.
func (r *Runner) SubmitApproval(ctx context.Context, results []transport.ApprovalResult) error {
	snap := r.agent.Snapshot()
	if snap.State != agent.StatePaused {
		return fmt.Errorf("agent is not waiting for approval")
	}

	allApproved := true
	for _, result := range results {
		if !result.Approved {
			allApproved = false
			break
		}
	}

	var eventCh <-chan protocol.Event
	var err error

	if allApproved {
		eventCh, err = r.agent.ApproveAndExecutePendingActions(ctx)
	} else {
		// Any denial → deny all pending actions (consistent with drainWithApprovalLoop).
		// The agent doesn't support partial approval natively.
		outputs := make([]protocol.ToolResult, len(snap.PendingActions))
		for i, action := range snap.PendingActions {
			outputs[i] = protocol.ToolResult{
				ToolCallID: action.ToolCallID,
				Type:       protocol.ToolResultTypeExecutionDenied,
				Reason:     "User denied permission",
				IsError:    true,
			}
		}
		eventCh, err = r.agent.SubmitToolOutputs(ctx, outputs)
	}

	if err != nil {
		return err
	}

	_, err = r.streamEvents(ctx, eventCh, false)
	return err
}

// GetPendingApprovals returns the current pending tool calls awaiting approval.
func (r *Runner) GetPendingApprovals() []transport.PendingToolCall {
	snap := r.agent.Snapshot()
	if snap.State != agent.StatePaused || len(snap.PendingActions) == 0 {
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

// --- internal ---

// handleInput processes a single user input.
func (r *Runner) handleInput(ctx context.Context, input transport.UserInput) error {
	switch input.Type {
	case transport.UserInputMessage:
		r.fireBeforeTurn(input)
		return r.handleMessage(ctx, input)
	case transport.UserInputCommand:
		return fmt.Errorf("unknown command: %s", input.Command)
	case transport.UserInputCancel:
		r.Stop()
		return nil
	case transport.UserInputQuit:
		r.Stop()
		_ = r.transport.Close()
		return nil
	default:
		return fmt.Errorf("unknown input type: %d", input.Type)
	}
}

// handleMessage processes a chat message and runs the full approval loop.
func (r *Runner) handleMessage(ctx context.Context, input transport.UserInput) error {
	msg := protocol.NewUserMessage(input.Text)

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

	// Start() uses the full approval loop — need_approval is handled
	// automatically via transport.RequestApproval().
	return r.drainWithApprovalLoop(ctx, eventCh)
}

// drainWithApprovalLoop drains events, and when hitting need_approval,
// calls transport.RequestApproval() and continues until the turn finishes.
func (r *Runner) drainWithApprovalLoop(ctx context.Context, eventCh <-chan protocol.Event) error {
	for {
		needsApproval, err := r.streamEvents(ctx, eventCh, true)
		if err != nil {
			return err
		}
		if !needsApproval {
			return nil
		}

		// Ask transport for approval
		snap := r.agent.Snapshot()
		if len(snap.PendingActions) == 0 {
			return nil
		}

		pending := make([]transport.PendingToolCall, len(snap.PendingActions))
		for i, pa := range snap.PendingActions {
			pending[i] = transport.PendingToolCall{
				ID:        pa.ToolCallID,
				Name:      pa.Name,
				Arguments: pa.Arguments,
			}
		}

		results, err := r.transport.RequestApproval(ctx, pending)
		if err != nil {
			r.fireOnError(err)
			return err
		}

		allApproved := len(results) > 0
		for _, res := range results {
			if !res.Approved {
				allApproved = false
				break
			}
		}

		if allApproved {
			eventCh, err = r.agent.ApproveAndExecutePendingActions(ctx)
		} else {
			outputs := make([]protocol.ToolResult, len(snap.PendingActions))
			for i, pa := range snap.PendingActions {
				outputs[i] = protocol.ToolResult{
					ToolCallID: pa.ToolCallID,
					Type:       protocol.ToolResultTypeExecutionDenied,
					Reason:     "User denied permission",
					IsError:    true,
				}
			}
			eventCh, err = r.agent.SubmitToolOutputs(ctx, outputs)
		}

		if err != nil {
			return err
		}
	}
}

// streamEvents drains an eventCh, forwarding events to transport.
// If handleApproval is true, returns (true, nil) on need_approval so the
// caller can handle it. If false, returns immediately on need_approval
// (for RunOnce/SubmitApproval patterns).
func (r *Runner) streamEvents(ctx context.Context, eventCh <-chan protocol.Event, handleApproval bool) (needsApproval bool, err error) {
	for {
		select {
		case <-ctx.Done():
			err := ctx.Err()
			r.fireOnError(err)
			return false, err
		case event, ok := <-eventCh:
			if !ok {
				return false, nil
			}

			if err := r.transport.Send(ctx, event); err != nil {
				r.fireOnError(err)
				return false, err
			}

			if event.Type == protocol.EventTypeFinish {
				if event.FinishReason == "need_approval" {
					r.fireOnPause()
					if handleApproval {
						return true, nil
					}
					return false, nil
				}
				if event.FinishReason == protocol.FinishReasonStop {
					r.fireAfterTurn()
				}
				return false, nil
			}
		}
	}
}

// --- hooks ---

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
	defer func() { recover() }()
	r.hooks.OnError(err)
}
