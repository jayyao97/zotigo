// Package runner orchestrates an Agent and Transport for bidirectional
// communication. It handles the event loop, approval flow, and turn-level listeners.
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

// Listeners is the set of observational callbacks the Runner invokes
// at turn-level milestones. Unlike agent.Middleware (which wraps every
// tool call and can alter control flow), Listeners are fire-and-forget:
// return values are ignored and any panic is logged via OnError but
// does not abort the turn. Use Listeners for session persistence,
// logging, metrics, or TUI updates; use agent.Middleware when you need
// to actually change what a tool does.
type Listeners struct {
	// BeforeTurn fires before processing a user message.
	BeforeTurn func(input transport.UserInput)
	// AfterTurn fires after each complete agent turn (finish with stop).
	AfterTurn func(snapshot agent.Snapshot)
	// OnPause fires when the agent pauses for tool approval.
	OnPause func(snapshot agent.Snapshot)
	// OnError fires when an error occurs during execution — including
	// panics from any of the other listeners in this set.
	OnError func(err error)
}

// Runner orchestrates the Agent and Transport for bidirectional communication.
type Runner struct {
	agent     *agent.Agent
	transport transport.Transport
	listeners Listeners

	mu       sync.Mutex
	running  bool
	cancelFn context.CancelFunc
}

// Option configures a Runner.
type Option func(*Runner)

// WithListeners sets the turn-level observational callbacks on the
// Runner. For tool-call interception (not observation), register an
// agent.Middleware via agent.WithMiddleware when constructing the
// agent.
func WithListeners(listeners Listeners) Option {
	return func(r *Runner) { r.listeners = listeners }
}

// New creates a new Runner with the given agent and transport.
func New(ag *agent.Agent, tr transport.Transport, opts ...Option) *Runner {
	r := &Runner{agent: ag, transport: tr}
	for _, opt := range opts {
		opt(r)
	}
	return r
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

// RunFullTurnStarted executes a complete conversation turn, automatically
// handling the approval loop via transport.RequestApproval(). Blocks until the
// turn truly finishes (no more tool calls). onStarted, if non-nil, is invoked
// after the agent accepts the input and before streamed events are drained.
//
// This is suitable for long-lived transports (ACP, WebSocket) where the
// transport can respond to approval requests synchronously.
func (r *Runner) RunFullTurnStarted(ctx context.Context, msg protocol.Message, onStarted func()) error {
	eventCh, err := r.agent.RunMessage(ctx, msg)
	if err != nil {
		return err
	}
	if onStarted != nil {
		onStarted()
	}

	return r.drainWithApprovalLoop(ctx, eventCh)
}

func deniedResultsFromApprovals(results []transport.ApprovalResult, pending []*agent.PendingAction) map[string]protocol.ToolResult {
	known := make(map[string]*agent.PendingAction, len(pending))
	for _, action := range pending {
		known[action.ToolCallID] = action
	}
	denied := make(map[string]protocol.ToolResult)
	for _, result := range results {
		if result.Approved {
			continue
		}
		action, ok := known[result.ToolCallID]
		if !ok {
			continue
		}
		reason := result.Reason
		if reason == "" {
			reason = "User denied permission"
		}
		denied[result.ToolCallID] = protocol.ToolResult{
			ToolCallID: result.ToolCallID,
			ToolName:   action.Name,
			Type:       protocol.ToolResultTypeExecutionDenied,
			Reason:     reason,
			IsError:    true,
		}
	}
	return denied
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
		needsApproval, err := r.streamEvents(ctx, eventCh)
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

		eventCh, err = r.agent.ResolvePendingActions(ctx, deniedResultsFromApprovals(results, snap.PendingActions))

		if err != nil {
			return err
		}
	}
}

// streamEvents drains an eventCh, forwarding events to transport. It returns
// (true, nil) when the stream ends in need_approval so the caller can resolve
// the pending tool calls.
func (r *Runner) streamEvents(ctx context.Context, eventCh <-chan protocol.Event) (needsApproval bool, err error) {
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

			sendErr := r.transport.Send(ctx, event)
			if event.Type == protocol.EventTypeSteeringApplied && event.SteeringAck != nil {
				event.SteeringAck <- sendErr
			}
			if sendErr != nil {
				r.fireOnError(sendErr)
				return false, sendErr
			}

			if event.Type == protocol.EventTypeFinish {
				if event.FinishReason == "need_approval" {
					r.fireOnPause()
					return true, nil
				}
				if event.FinishReason == protocol.FinishReasonStop {
					r.fireAfterTurn()
				}
				return false, nil
			}
		}
	}
}

// --- listener dispatch ---

func (r *Runner) safeCallListener(fn func()) {
	defer func() {
		if v := recover(); v != nil {
			r.fireOnError(fmt.Errorf("listener panicked: %v", v))
		}
	}()
	fn()
}

func (r *Runner) fireBeforeTurn(input transport.UserInput) {
	if r.listeners.BeforeTurn != nil {
		r.safeCallListener(func() { r.listeners.BeforeTurn(input) })
	}
}

func (r *Runner) fireAfterTurn() {
	if r.listeners.AfterTurn != nil {
		r.safeCallListener(func() { r.listeners.AfterTurn(r.agent.Snapshot()) })
	}
}

func (r *Runner) fireOnPause() {
	if r.listeners.OnPause != nil {
		r.safeCallListener(func() { r.listeners.OnPause(r.agent.Snapshot()) })
	}
}

func (r *Runner) fireOnError(err error) {
	if r.listeners.OnError == nil {
		return
	}
	defer func() { _ = recover() }()
	r.listeners.OnError(err)
}
