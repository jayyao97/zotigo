package zotigod

import (
	"context"
	"fmt"
	"sync"

	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigotransport "github.com/jayyao97/zotigo/core/transport"
)

type workerRuntimeTransport struct {
	sessionID      string
	display        *workerDisplayLog
	notifyApproval func(context.Context, approvalRequestResponse)

	inputCh           chan zotigotransport.UserInput
	closedCh          chan struct{}
	closeOnce         sync.Once
	approvalMu        sync.Mutex
	approval          *approvalRequest
	decisionCh        chan []zotigotransport.ApprovalResult
	resolved          bool
	released          bool
	interruptedTurnID string
	interruptedReason string
}

type workerApprovalResolution struct {
	approval   approvalRequestResponse
	results    []zotigotransport.ApprovalResult
	decisionCh chan []zotigotransport.ApprovalResult
}

func newWorkerRuntimeTransport(sessionID string, display *workerDisplayLog, notifyApproval func(context.Context, approvalRequestResponse)) *workerRuntimeTransport {
	return &workerRuntimeTransport{
		sessionID:      sessionID,
		display:        display,
		notifyApproval: notifyApproval,
		inputCh:        make(chan zotigotransport.UserInput, 32),
		closedCh:       make(chan struct{}),
	}
}

func (t *workerRuntimeTransport) Send(ctx context.Context, event protocol.Event) error {
	return t.display.HandleEvent(ctx, event)
}

func (t *workerRuntimeTransport) Receive(context.Context) <-chan zotigotransport.UserInput {
	return t.inputCh
}

func (t *workerRuntimeTransport) RequestApproval(ctx context.Context, pending []zotigotransport.PendingToolCall) ([]zotigotransport.ApprovalResult, error) {
	_, decisionCh, err := t.beginApproval(ctx, pending)
	if err != nil {
		return nil, err
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.closedCh:
			return nil, zotigotransport.ErrTransportClosed
		case decisions := <-decisionCh:
			return decisions, nil
		}
	}
}

func (t *workerRuntimeTransport) Close() error {
	t.closeOnce.Do(func() {
		close(t.closedCh)
		close(t.inputCh)
	})
	return nil
}

func (t *workerRuntimeTransport) SendInput(ctx context.Context, input zotigotransport.UserInput) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.closedCh:
		return zotigotransport.ErrTransportClosed
	case t.inputCh <- input:
		return nil
	}
}

func (t *workerRuntimeTransport) beginApproval(ctx context.Context, pending []zotigotransport.PendingToolCall) (approvalRequest, <-chan []zotigotransport.ApprovalResult, error) {
	return t.registerApproval(ctx, pending, false)
}

func (t *workerRuntimeTransport) ensureApproval(ctx context.Context, pending []zotigotransport.PendingToolCall) (approvalRequest, <-chan []zotigotransport.ApprovalResult, error) {
	return t.registerApproval(ctx, pending, true)
}

func (t *workerRuntimeTransport) registerApproval(ctx context.Context, pending []zotigotransport.PendingToolCall, reuseReleased bool) (approvalRequest, <-chan []zotigotransport.ApprovalResult, error) {
	displayPending := make([]zotigosession.DisplayPendingApproval, 0, len(pending))
	for _, item := range pending {
		displayPending = append(displayPending, zotigosession.DisplayPendingApproval{
			ToolCallID:  item.ID,
			ToolName:    item.Name,
			Arguments:   item.Arguments,
			Description: item.Description,
		})
	}
	t.approvalMu.Lock()
	if err := ctx.Err(); err != nil {
		t.approvalMu.Unlock()
		return approvalRequest{}, nil, err
	}
	approval, err := newApprovalRequest(t.sessionID, t.display.CurrentTurnID(), displayPending)
	if err != nil {
		t.approvalMu.Unlock()
		return approvalRequest{}, nil, err
	}
	if t.interruptedTurnID == approval.TurnID {
		t.approvalMu.Unlock()
		return approvalRequest{}, nil, context.Canceled
	}
	if t.approval != nil {
		sameApproval := t.approval.TurnID == approval.TurnID && samePendingApprovals(t.approval.Pending, displayPending)
		if sameApproval && t.released && reuseReleased {
			existing := *t.approval
			decisionCh := t.decisionCh
			t.approvalMu.Unlock()
			return existing, decisionCh, nil
		}
		if sameApproval && !t.released {
			if t.resolved {
				resolvedID := t.approval.ID
				t.approvalMu.Unlock()
				return approvalRequest{}, nil, fmt.Errorf("approval %s is already resolved", resolvedID)
			}
			approval := *t.approval
			decisionCh := t.decisionCh
			t.approvalMu.Unlock()
			return approval, decisionCh, nil
		}
		if !t.released {
			pendingID := t.approval.ID
			t.approvalMu.Unlock()
			return approvalRequest{}, nil, fmt.Errorf("approval %s is already pending", pendingID)
		}
	}
	t.approval = &approval
	t.resolved = false
	t.released = false
	decisionCh := make(chan []zotigotransport.ApprovalResult, 1)
	t.decisionCh = decisionCh

	persisted, err := t.display.ApprovalRequested(ctx, approval)
	if err != nil {
		t.approval = nil
		t.decisionCh = nil
		t.resolved = false
		t.released = false
		t.approvalMu.Unlock()
		return approvalRequest{}, nil, fmt.Errorf("record approval request: %w", err)
	}
	approval = persisted
	*t.approval = approval
	t.display.MarkPaused()
	if t.notifyApproval != nil {
		t.notifyApproval(ctx, publicApprovalRequest(approval))
	}
	t.approvalMu.Unlock()
	return approval, decisionCh, nil
}

func samePendingApprovals(left []zotigosession.DisplayPendingApproval, right []zotigosession.DisplayPendingApproval) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].ToolCallID != right[index].ToolCallID || left[index].ToolName != right[index].ToolName || left[index].Arguments != right[index].Arguments {
			return false
		}
	}
	return true
}

func (t *workerRuntimeTransport) resolveApproval(ctx context.Context, approvalID string, decisions []zotigosession.DisplayApprovalDecision) (workerApprovalResolution, error) {
	t.approvalMu.Lock()
	defer t.approvalMu.Unlock()
	if t.approval == nil || t.approval.ID != approvalID {
		return workerApprovalResolution{}, fmt.Errorf("approval request %q is not pending", approvalID)
	}
	if t.resolved {
		return workerApprovalResolution{}, fmt.Errorf("approval request %q is already resolved", approvalID)
	}
	approval := *t.approval
	decisionCh := t.decisionCh
	if err := validateApprovalDecisions(approval.Pending, decisions); err != nil {
		return workerApprovalResolution{}, err
	}
	resolved, err := t.display.ApprovalResolved(ctx, approval, decisions)
	if err != nil {
		return workerApprovalResolution{}, fmt.Errorf("record approval decision: %w", err)
	}
	response := publicApprovalRequest(resolved)
	t.resolved = true
	return workerApprovalResolution{
		approval:   response,
		results:    approvalResultsFromResponse(response),
		decisionCh: decisionCh,
	}, nil
}

func (t *workerRuntimeTransport) releaseApproval(ctx context.Context, resolution workerApprovalResolution) error {
	t.approvalMu.Lock()
	defer t.approvalMu.Unlock()
	if t.approval == nil || t.approval.ID != resolution.approval.ID || !t.resolved || t.released {
		return fmt.Errorf("approval request %q is not awaiting release", resolution.approval.ID)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.closedCh:
		return zotigotransport.ErrTransportClosed
	case resolution.decisionCh <- resolution.results:
		t.released = true
		return nil
	}
}

func (t *workerRuntimeTransport) interruptTurn(ctx context.Context, turnID string, reason string, cancel func()) (bool, error) {
	t.approvalMu.Lock()
	defer t.approvalMu.Unlock()
	if t.approval != nil && t.approval.TurnID == turnID && !t.released {
		return false, nil
	}
	t.interruptedTurnID = turnID
	t.interruptedReason = reason
	if cancel != nil {
		cancel()
	}
	return true, t.display.Interrupt(ctx, reason)
}

func (t *workerRuntimeTransport) interruptedTurn(turnID string) (string, bool) {
	t.approvalMu.Lock()
	defer t.approvalMu.Unlock()
	if t.interruptedTurnID != turnID {
		return "", false
	}
	return t.interruptedReason, true
}

func (t *workerRuntimeTransport) hasApprovalRegistration() bool {
	t.approvalMu.Lock()
	defer t.approvalMu.Unlock()
	return t.approval != nil
}

func approvalResultsFromResponse(resp approvalRequestResponse) []zotigotransport.ApprovalResult {
	results := make([]zotigotransport.ApprovalResult, 0, len(resp.Decisions))
	for _, decision := range resp.Decisions {
		results = append(results, zotigotransport.ApprovalResult{
			ToolCallID:   decision.ToolCallID,
			Approved:     decision.Approved,
			Reason:       decision.Reason,
			ModifiedArgs: decision.ModifiedArgs,
		})
	}
	return results
}

var _ zotigotransport.Transport = (*workerRuntimeTransport)(nil)
