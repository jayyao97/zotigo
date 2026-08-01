package tui

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/tools/builtin"
	"github.com/jayyao97/zotigo/core/transport"
)

func TestSpawnApprovalBrokerRoutesDecisionThroughModel(t *testing.T) {
	broker := NewSpawnApprovalBroker()
	model := NewModel(newDisplayLogTestAgent(t), nil, "", nil, WithSpawnApprovalBroker(broker))
	resultsCh := make(chan []transport.ApprovalResult, 1)
	errCh := make(chan error, 1)
	go func() {
		results, err := broker.RequestSpawnApproval(context.Background(), builtin.SpawnApprovalRequest{
			AgentName: "reviewer",
			Actions: []*agent.PendingAction{{
				ToolCallID: "child-call-1",
				Name:       "shell",
				Arguments:  `{"command":"go test ./..."}`,
			}},
		})
		if err != nil {
			errCh <- err
			return
		}
		resultsCh <- results
	}()

	requestMsg := waitForSpawnApproval(broker)()
	updated, _ := model.Update(requestMsg)
	model = updated.(*Model)
	if !model.approving || model.approvalContext != "Subagent reviewer" {
		t.Fatalf("unexpected approval state: approving=%v context=%q", model.approving, model.approvalContext)
	}

	_, cmd := model.acceptCurrentApproval()
	approvalMsg := cmd()
	batch, ok := approvalMsg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("approval command = %T, want tea.BatchMsg", approvalMsg)
	}
	for _, childCmd := range batch {
		if childCmd != nil {
			childCmd()
		}
	}

	select {
	case err := <-errCh:
		t.Fatalf("RequestSpawnApproval: %v", err)
	case results := <-resultsCh:
		if len(results) != 1 || results[0].ToolCallID != "child-call-1" || !results[0].Approved {
			t.Fatalf("approval results = %+v", results)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for child approval result")
	}
}

func TestSpawnApprovalBrokerRejectsAlreadyCanceledRequest(t *testing.T) {
	broker := NewSpawnApprovalBroker()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := broker.RequestSpawnApproval(ctx, builtin.SpawnApprovalRequest{}); err == nil {
		t.Fatal("RequestSpawnApproval should reject an already canceled context")
	}
	if got := len(broker.requests); got != 0 {
		t.Fatalf("queued requests = %d, want 0", got)
	}
}

func TestSpawnApprovalBrokerSkipsCanceledQueuedRequest(t *testing.T) {
	broker := NewSpawnApprovalBroker()
	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := broker.RequestSpawnApproval(ctx, builtin.SpawnApprovalRequest{AgentName: "stale"})
		firstDone <- err
	}()
	waitForQueuedSpawnApproval(t, broker)
	cancel()

	secondDone := make(chan []transport.ApprovalResult, 1)
	go func() {
		results, _ := broker.RequestSpawnApproval(context.Background(), builtin.SpawnApprovalRequest{AgentName: "current"})
		secondDone <- results
	}()
	msg := waitForSpawnApproval(broker)().(spawnApprovalRequestMsg)
	if msg.request.request.AgentName != "current" {
		t.Fatalf("received request for %q, want current", msg.request.request.AgentName)
	}
	msg.request.resolve([]transport.ApprovalResult{{ToolCallID: "call-2", Approved: true}})

	select {
	case err := <-firstDone:
		if err == nil {
			t.Fatal("canceled request returned nil error")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for canceled request")
	}
	select {
	case results := <-secondDone:
		if len(results) != 1 || results[0].ToolCallID != "call-2" {
			t.Fatalf("second results = %+v", results)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for current request")
	}
}

func TestSpawnApprovalModelClearsDisplayedCanceledRequest(t *testing.T) {
	broker := NewSpawnApprovalBroker()
	model := NewModel(newDisplayLogTestAgent(t), nil, "", nil, WithSpawnApprovalBroker(broker))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := broker.RequestSpawnApproval(ctx, builtin.SpawnApprovalRequest{
			AgentName: "reviewer",
			Actions:   []*agent.PendingAction{{ToolCallID: "child-call-1", Name: "shell"}},
		})
		done <- err
	}()

	requestMsg := waitForSpawnApproval(broker)()
	updated, cancelCmd := model.Update(requestMsg)
	model = updated.(*Model)
	if !model.approving {
		t.Fatal("model should display child approval")
	}
	cancel()
	canceledMsg := cancelCmd()
	updated, _ = model.Update(canceledMsg)
	model = updated.(*Model)
	if model.approving || model.spawnRequest != nil || len(model.pendingApprovals) != 0 {
		t.Fatalf("stale approval was not cleared: approving=%v request=%v pending=%d", model.approving, model.spawnRequest != nil, len(model.pendingApprovals))
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled request returned nil error")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for canceled displayed request")
	}
}

func waitForQueuedSpawnApproval(t *testing.T, broker *SpawnApprovalBroker) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for len(broker.requests) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for queued spawn approval")
		}
		time.Sleep(time.Millisecond)
	}
}
