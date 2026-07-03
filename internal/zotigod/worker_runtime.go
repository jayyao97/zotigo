package zotigod

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigotransport "github.com/jayyao97/zotigo/core/transport"
)

const approvalPollInterval = 500 * time.Millisecond

type workerRuntimeTransport struct {
	sessionID string
	daemonURL string
	client    *http.Client
	display   *workerDisplayLog

	inputCh   chan zotigotransport.UserInput
	closedCh  chan struct{}
	closeOnce sync.Once
}

func newWorkerRuntimeTransport(sessionID string, daemonURL string, client *http.Client, display *workerDisplayLog) *workerRuntimeTransport {
	return &workerRuntimeTransport{
		sessionID: sessionID,
		daemonURL: strings.TrimRight(daemonURL, "/"),
		client:    client,
		display:   display,
		inputCh:   make(chan zotigotransport.UserInput, 32),
		closedCh:  make(chan struct{}),
	}
}

func (t *workerRuntimeTransport) Send(ctx context.Context, event protocol.Event) error {
	return t.display.HandleEvent(ctx, event)
}

func (t *workerRuntimeTransport) Receive(context.Context) <-chan zotigotransport.UserInput {
	return t.inputCh
}

func (t *workerRuntimeTransport) RequestApproval(ctx context.Context, pending []zotigotransport.PendingToolCall) ([]zotigotransport.ApprovalResult, error) {
	approval, err := t.createApproval(ctx, pending)
	if err != nil {
		return nil, err
	}
	t.display.MarkPaused()

	ticker := time.NewTicker(approvalPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.closedCh:
			return nil, zotigotransport.ErrTransportClosed
		case <-ticker.C:
			latest, err := t.getApproval(ctx, approval.ID)
			if err != nil {
				return nil, err
			}
			if latest.Status == approvalStatusResolved {
				return approvalResultsFromResponse(latest), nil
			}
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

func (t *workerRuntimeTransport) createApproval(ctx context.Context, pending []zotigotransport.PendingToolCall) (approvalRequestResponse, error) {
	req := createApprovalRequest{
		TurnID:  t.display.CurrentTurnID(),
		Pending: make([]pendingApprovalRequestDTO, 0, len(pending)),
	}
	for _, item := range pending {
		req.Pending = append(req.Pending, pendingApprovalRequestDTO{
			ToolCallID:  item.ID,
			ToolName:    item.Name,
			Arguments:   item.Arguments,
			Description: item.Description,
		})
	}
	var resp approvalRequestResponse
	err := t.postJSON(ctx, "/internal/sessions/"+url.PathEscape(t.sessionID)+"/approvals", req, http.StatusCreated, &resp)
	return resp, err
}

func (t *workerRuntimeTransport) getApproval(ctx context.Context, approvalID string) (approvalRequestResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.daemonURL+"/internal/sessions/"+url.PathEscape(t.sessionID)+"/approvals/"+url.PathEscape(approvalID), nil)
	if err != nil {
		return approvalRequestResponse{}, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return approvalRequestResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return approvalRequestResponse{}, fmt.Errorf("get approval failed: %s", resp.Status)
	}
	var body approvalRequestResponse
	if err := sonic.ConfigDefault.NewDecoder(resp.Body).Decode(&body); err != nil {
		return approvalRequestResponse{}, err
	}
	return body, nil
}

func (t *workerRuntimeTransport) postJSON(ctx context.Context, path string, value any, wantStatus int, out any) error {
	data, err := sonic.Marshal(value)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.daemonURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != wantStatus {
		return fmt.Errorf("post %s failed: %s", path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return sonic.ConfigDefault.NewDecoder(resp.Body).Decode(out)
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
