package tui

import (
	"context"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/jayyao97/zotigo/core/tools/builtin"
	"github.com/jayyao97/zotigo/core/transport"
)

// SpawnApprovalBroker carries child-agent approval requests into the TUI loop.
type SpawnApprovalBroker struct {
	requests chan *spawnApprovalRequest
}

type spawnApprovalRequest struct {
	request  builtin.SpawnApprovalRequest
	ctx      context.Context
	reply    chan []transport.ApprovalResult
	resolved chan struct{}
	once     sync.Once
}

type spawnApprovalRequestMsg struct {
	request *spawnApprovalRequest
}

type spawnApprovalResolvedMsg struct{}

type spawnApprovalCanceledMsg struct {
	request *spawnApprovalRequest
}

func NewSpawnApprovalBroker() *SpawnApprovalBroker {
	return &SpawnApprovalBroker{requests: make(chan *spawnApprovalRequest, 1)}
}

func (b *SpawnApprovalBroker) RequestSpawnApproval(ctx context.Context, request builtin.SpawnApprovalRequest) ([]transport.ApprovalResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pending := &spawnApprovalRequest{
		request:  request,
		ctx:      ctx,
		reply:    make(chan []transport.ApprovalResult, 1),
		resolved: make(chan struct{}),
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case b.requests <- pending:
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case results := <-pending.reply:
		return results, nil
	}
}

func (r *spawnApprovalRequest) resolve(results []transport.ApprovalResult) {
	r.once.Do(func() {
		close(r.resolved)
		r.reply <- results
	})
}

func waitForSpawnApproval(broker *SpawnApprovalBroker) tea.Cmd {
	if broker == nil {
		return nil
	}
	return func() tea.Msg {
		for {
			request := <-broker.requests
			if request == nil || request.ctx.Err() != nil {
				continue
			}
			return spawnApprovalRequestMsg{request: request}
		}
	}
}

func waitForSpawnApprovalCancellation(request *spawnApprovalRequest) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-request.resolved:
			return nil
		case <-request.ctx.Done():
			return spawnApprovalCanceledMsg{request: request}
		}
	}
}

var _ builtin.SpawnApprovalRequester = (*SpawnApprovalBroker)(nil)
