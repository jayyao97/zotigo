package zotigod

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/providers"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigotransport "github.com/jayyao97/zotigo/core/transport"
)

func TestWorkerRuntimeApprovalIsPersistedAndResolvedWithoutPolling(t *testing.T) {
	const sessionID = "sess-event-driven-approval"
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog(sessionID, source)
	if _, err := display.StartTurn(context.Background()); err != nil {
		t.Fatalf("start display turn: %v", err)
	}
	notifiedCh := make(chan approvalRequestResponse, 1)
	transport := newWorkerRuntimeTransport(sessionID, display, func(_ context.Context, approval approvalRequestResponse) {
		notifiedCh <- approval
	})
	resultCh := make(chan []zotigotransport.ApprovalResult, 1)
	errCh := make(chan error, 1)
	go func() {
		results, err := transport.RequestApproval(context.Background(), []zotigotransport.PendingToolCall{{
			ID:   "call-1",
			Name: "glob",
		}})
		resultCh <- results
		errCh <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	var approval approvalRequest
	for time.Now().Before(deadline) {
		items, _, err := source.LoadItems(context.Background(), sessionID)
		if err != nil {
			t.Fatalf("load approval items: %v", err)
		}
		for _, item := range items {
			if item.Type == zotigosession.DisplayItemApprovalRequest && item.Approval != nil {
				approval, _ = approvalFromDisplayItems(sessionID, item.Approval.ID, items)
				break
			}
		}
		if approval.ID != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if approval.ID == "" {
		t.Fatal("worker did not persist approval request")
	}
	notified := <-notifiedCh
	if notified.ID != approval.ID || notified.Status != approvalStatusPending {
		t.Fatalf("unexpected approval notification: %#v", notified)
	}
	approved := []zotigosession.DisplayApprovalDecision{{ToolCallID: "call-1", Approved: true}}
	resolution, err := transport.resolveApproval(context.Background(), approval.ID, approved)
	if err != nil {
		t.Fatalf("resolve approval: %v", err)
	}
	if resolution.approval.Status != approvalStatusResolved {
		t.Fatalf("resolved status = %q", resolution.approval.Status)
	}
	select {
	case <-resultCh:
		t.Fatal("runner resumed before approval acknowledgement was queued")
	default:
	}
	if err := transport.releaseApproval(context.Background(), resolution); err != nil {
		t.Fatalf("release approval: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("request approval: %v", err)
	}
	results := <-resultCh
	if len(results) != 1 || results[0].ToolCallID != "call-1" || !results[0].Approved {
		t.Fatalf("unexpected approval results: %#v", results)
	}
}

func TestWorkerRuntimeApprovalDecisionIsAppliedOnce(t *testing.T) {
	const sessionID = "sess-single-decision"
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog(sessionID, source)
	if _, err := display.StartTurn(context.Background()); err != nil {
		t.Fatalf("start display turn: %v", err)
	}
	notifiedCh := make(chan approvalRequestResponse, 1)
	transport := newWorkerRuntimeTransport(sessionID, display, func(_ context.Context, approval approvalRequestResponse) {
		notifiedCh <- approval
	})
	resultCh := make(chan []zotigotransport.ApprovalResult, 1)
	go func() {
		results, _ := transport.RequestApproval(context.Background(), []zotigotransport.PendingToolCall{{ID: "call-1", Name: "glob"}})
		resultCh <- results
	}()
	approval := <-notifiedCh

	resolution, err := transport.resolveApproval(context.Background(), approval.ID, []zotigosession.DisplayApprovalDecision{{ToolCallID: "call-1", Approved: true}})
	if err != nil {
		t.Fatalf("resolve first decision: %v", err)
	}
	if _, err := transport.resolveApproval(context.Background(), approval.ID, []zotigosession.DisplayApprovalDecision{{ToolCallID: "call-1", Approved: false}}); err == nil {
		t.Fatal("expected duplicate decision to be rejected")
	}
	items, _, err := source.LoadItems(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load display items: %v", err)
	}
	decisionCount := 0
	for _, item := range items {
		if item.Type == zotigosession.DisplayItemApprovalDecision {
			decisionCount++
		}
	}
	if decisionCount != 1 {
		t.Fatalf("approval decision item count = %d, want 1", decisionCount)
	}
	if err := transport.releaseApproval(context.Background(), resolution); err != nil {
		t.Fatalf("release approval: %v", err)
	}
	results := <-resultCh
	if len(results) != 1 || !results[0].Approved {
		t.Fatalf("unexpected approval results: %#v", results)
	}
}

func TestWorkerRuntimeCloseDoesNotCreateApprovalAfterReleaseBeforeAgentResumes(t *testing.T) {
	const sessionID = "sess-close-resolved-approval"
	const providerName = "approval-close-resolved-boundary-test"
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	localExec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	ag, err := agent.New(config.ProfileConfig{Provider: providerName, Model: "test"}, localExec)
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	ag.Restore(agent.Snapshot{
		State: agent.StatePaused,
		PendingActions: []*agent.PendingAction{{
			ToolCallID: "call-1",
			Name:       "glob",
		}},
	})
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog(sessionID, source)
	if _, err := display.StartTurn(context.Background()); err != nil {
		t.Fatalf("start display turn: %v", err)
	}
	transport := newWorkerRuntimeTransport(sessionID, display, nil)
	approval, _, err := transport.beginApproval(context.Background(), []zotigotransport.PendingToolCall{{ID: "call-1", Name: "glob"}})
	if err != nil {
		t.Fatalf("begin approval: %v", err)
	}
	resolution, err := transport.resolveApproval(context.Background(), approval.ID, []zotigosession.DisplayApprovalDecision{{ToolCallID: "call-1", Approved: true}})
	if err != nil {
		t.Fatalf("resolve approval: %v", err)
	}
	if err := transport.releaseApproval(context.Background(), resolution); err != nil {
		t.Fatalf("release approval: %v", err)
	}
	done := make(chan struct{})
	close(done)
	runtime := &workerRuntime{
		agent:      ag,
		transport:  transport,
		display:    display,
		turnActive: true,
		turnCancel: func() {},
		turnDone:   done,
		cleanup: func() {
			_ = localExec.Close()
		},
	}
	runtime.Close()

	items, _, err := source.LoadItems(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load display items: %v", err)
	}
	requestCount := 0
	for _, item := range items {
		if item.Type == zotigosession.DisplayItemApprovalRequest {
			requestCount++
		}
	}
	if requestCount != 1 || hasPendingApproval(items) {
		t.Fatalf("close created a ghost approval after durable decision: %#v", items)
	}
}

func TestWorkerRuntimePauseWinsBeforeApprovalRegistration(t *testing.T) {
	const sessionID = "sess-pause-before-approval"
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog(sessionID, source)
	turnID, err := display.StartTurn(context.Background())
	if err != nil {
		t.Fatalf("start display turn: %v", err)
	}
	transport := newWorkerRuntimeTransport(sessionID, display, nil)
	canceled := false
	turnCtx, cancelTurn := context.WithCancel(context.Background())
	interrupted, err := transport.interruptTurn(context.Background(), turnID, "user requested", func() {
		canceled = true
		cancelTurn()
	})
	if err != nil {
		t.Fatalf("interrupt turn: %v", err)
	}
	if !interrupted || !canceled {
		t.Fatalf("pause did not win arbitration: interrupted=%v canceled=%v", interrupted, canceled)
	}
	if _, _, err := transport.beginApproval(turnCtx, []zotigotransport.PendingToolCall{{ID: "call-1", Name: "glob"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("approval error after interruption = %v, want context canceled", err)
	}
	items, _, err := source.LoadItems(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load display items: %v", err)
	}
	if hasPendingApproval(items) {
		t.Fatalf("interrupted turn has pending approval: %#v", items)
	}
}

func TestWorkerRuntimeApprovalWinsBeforePause(t *testing.T) {
	const sessionID = "sess-approval-before-pause"
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog(sessionID, source)
	turnID, err := display.StartTurn(context.Background())
	if err != nil {
		t.Fatalf("start display turn: %v", err)
	}
	transport := newWorkerRuntimeTransport(sessionID, display, nil)
	if _, _, err := transport.beginApproval(context.Background(), []zotigotransport.PendingToolCall{{ID: "call-1", Name: "glob"}}); err != nil {
		t.Fatalf("begin approval: %v", err)
	}
	canceled := false
	interrupted, err := transport.interruptTurn(context.Background(), turnID, "user requested", func() { canceled = true })
	if err != nil {
		t.Fatalf("interrupt turn: %v", err)
	}
	if interrupted || canceled {
		t.Fatalf("pause interrupted a pending approval: interrupted=%v canceled=%v", interrupted, canceled)
	}
	items, _, err := source.LoadItems(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load display items: %v", err)
	}
	if got := lastOpenTurnID(items); got != turnID || !hasPendingApproval(items) {
		t.Fatalf("approval turn did not remain pending: %#v", items)
	}
}

func TestWorkerRuntimeReleasedApprovalDoesNotBlockPauseOrNextTurn(t *testing.T) {
	const sessionID = "sess-released-approval"
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog(sessionID, source)
	turnID, err := display.StartTurn(context.Background())
	if err != nil {
		t.Fatalf("start display turn: %v", err)
	}
	transport := newWorkerRuntimeTransport(sessionID, display, nil)
	pending := []zotigotransport.PendingToolCall{{ID: "call-1", Name: "glob"}}
	approval, _, err := transport.beginApproval(context.Background(), pending)
	if err != nil {
		t.Fatalf("begin approval: %v", err)
	}
	resolution, err := transport.resolveApproval(context.Background(), approval.ID, []zotigosession.DisplayApprovalDecision{{ToolCallID: "call-1", Approved: true}})
	if err != nil {
		t.Fatalf("resolve approval: %v", err)
	}
	if err := transport.releaseApproval(context.Background(), resolution); err != nil {
		t.Fatalf("release approval: %v", err)
	}
	canceled := false
	interrupted, err := transport.interruptTurn(context.Background(), turnID, "user requested", func() { canceled = true })
	if err != nil {
		t.Fatalf("interrupt released approval turn: %v", err)
	}
	if !interrupted || !canceled {
		t.Fatalf("released approval blocked pause: interrupted=%v canceled=%v", interrupted, canceled)
	}
	if _, err := display.StartTurn(context.Background()); err != nil {
		t.Fatalf("start next display turn: %v", err)
	}
	next, _, err := transport.beginApproval(context.Background(), pending)
	if err != nil {
		t.Fatalf("begin same approval in next turn: %v", err)
	}
	if next.ID == approval.ID {
		t.Fatalf("next turn reused released approval %q", next.ID)
	}
}

func TestWorkerRuntimeRepairsPausedAgentWhenPauseWinsApprovalRace(t *testing.T) {
	const providerName = "approval-pause-repair-test"
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	localExec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: providerName, Model: "test"}, localExec)
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	ag.Restore(agent.Snapshot{
		State: agent.StatePaused,
		PendingActions: []*agent.PendingAction{{
			ToolCallID: "call-1",
			Name:       "glob",
		}},
	})
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog("sess-pause-repair", source)
	turnID, err := display.StartTurn(context.Background())
	if err != nil {
		t.Fatalf("start display turn: %v", err)
	}
	transport := newWorkerRuntimeTransport("sess-pause-repair", display, nil)
	if _, err := transport.interruptTurn(context.Background(), turnID, "user requested", func() {}); err != nil {
		t.Fatalf("interrupt turn: %v", err)
	}
	runtime := &workerRuntime{agent: ag, transport: transport}
	snapshot := runtime.snapshotAfterTurn(turnID)
	if snapshot.State != agent.StateIdle || len(snapshot.PendingActions) != 0 || len(snapshot.DeferredActions) != 0 {
		t.Fatalf("paused agent was not repaired: %#v", snapshot)
	}
	if got := ag.Snapshot(); got.State != agent.StateIdle {
		t.Fatalf("runtime agent state = %q, want idle", got.State)
	}
}

func TestWorkerRuntimeApprovalCannotResolveDuringInitialization(t *testing.T) {
	const sessionID = "sess-approval-initializing"
	persistedCh := make(chan string, 1)
	continueCh := make(chan struct{})
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	source.appendHook = func(_ string, item zotigosession.DisplayItem) {
		if item.Type == zotigosession.DisplayItemApprovalRequest {
			persistedCh <- item.Approval.ID
			<-continueCh
		}
	}
	display := newWorkerDisplayLog(sessionID, source)
	if _, err := display.StartTurn(context.Background()); err != nil {
		t.Fatalf("start display turn: %v", err)
	}
	notifiedCh := make(chan approvalRequestResponse, 1)
	transport := newWorkerRuntimeTransport(sessionID, display, func(_ context.Context, approval approvalRequestResponse) {
		notifiedCh <- approval
	})
	requestDone := make(chan error, 1)
	go func() {
		_, err := transport.RequestApproval(context.Background(), []zotigotransport.PendingToolCall{{ID: "call-1", Name: "glob"}})
		requestDone <- err
	}()
	approvalID := <-persistedCh

	resolutionCh := make(chan workerApprovalResolution, 1)
	resolveErrCh := make(chan error, 1)
	resolveStarted := make(chan struct{})
	go func() {
		close(resolveStarted)
		resolution, err := transport.resolveApproval(context.Background(), approvalID, []zotigosession.DisplayApprovalDecision{{ToolCallID: "call-1", Approved: true}})
		resolutionCh <- resolution
		resolveErrCh <- err
	}()
	<-resolveStarted
	select {
	case err := <-resolveErrCh:
		t.Fatalf("decision observed partially initialized approval: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(continueCh)
	resolution := <-resolutionCh
	if err := <-resolveErrCh; err != nil {
		t.Fatalf("resolve approval after initialization: %v", err)
	}
	if notified := <-notifiedCh; notified.ID != approvalID {
		t.Fatalf("notified approval ID = %q, want %q", notified.ID, approvalID)
	}
	if err := transport.releaseApproval(context.Background(), resolution); err != nil {
		t.Fatalf("release approval: %v", err)
	}
	if err := <-requestDone; err != nil {
		t.Fatalf("request approval: %v", err)
	}
}

func TestWorkerDisplayLogResolvesPendingApprovalBeforeInterruptingRestartedTurn(t *testing.T) {
	const sessionID = "sess-restarted-approval"
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog(sessionID, source)
	turnID, err := display.StartTurn(context.Background())
	if err != nil {
		t.Fatalf("start display turn: %v", err)
	}
	approval := approvalRequest{
		ID:        "apr-restarted",
		SessionID: sessionID,
		TurnID:    turnID,
		Status:    approvalStatusPending,
		Pending:   []zotigosession.DisplayPendingApproval{{ToolCallID: "call-1", ToolName: "shell"}},
	}
	if _, err := display.ApprovalRequested(context.Background(), approval); err != nil {
		t.Fatalf("record approval request: %v", err)
	}
	recovered, err := display.ResolvePendingApprovalsForOpenTurn(context.Background(), approvalWorkerRestartedReason)
	if err != nil {
		t.Fatalf("resolve restarted approval: %v", err)
	}
	if !recovered {
		t.Fatal("expected pending approval recovery")
	}
	if err := display.InterruptOpenTurn(context.Background(), workerRestartedReason); err != nil {
		t.Fatalf("interrupt restarted turn: %v", err)
	}
	items, _, err := source.LoadItems(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load display items: %v", err)
	}
	if len(items) != 5 || items[3].Type != zotigosession.DisplayItemApprovalDecision || items[4].Type != zotigosession.DisplayItemTurnInterrupted {
		t.Fatalf("unexpected recovery items: %#v", items)
	}
	decision := items[3].Approval.Decisions[0]
	if decision.Approved || decision.Reason != approvalWorkerRestartedReason {
		t.Fatalf("unexpected recovery decision: %#v", decision)
	}
}

func TestWorkerRuntimeRecoversPendingApprovalFromPreviousWorker(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const (
		sessionID    = "sess-runtime-approval-recovery"
		providerName = "approval-recovery-runtime-test"
	)
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	workDir := t.TempDir()
	projectConfig := fmt.Sprintf("default_profile: test\nprofiles:\n  test:\n    provider: %s\n    model: test\n", providerName)
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:               sessionID,
			WorkingDirectory: workDir,
			ProfileName:      "test",
			CreatedAt:        now,
			UpdatedAt:        now,
		},
		AgentSnapshot: agent.Snapshot{
			State: agent.StatePaused,
			PendingActions: []*agent.PendingAction{{
				ToolCallID: "call-1",
				Name:       "shell",
			}},
			CreatedAt: now,
		},
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	for _, item := range []zotigosession.DisplayItem{
		{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
		{Type: zotigosession.DisplayItemApprovalRequest, Approval: &zotigosession.DisplayApproval{
			ID:      "apr-1",
			TurnID:  "turn-1",
			Pending: []zotigosession.DisplayPendingApproval{{ToolCallID: "call-1", ToolName: "shell"}},
		}},
		{Type: zotigosession.DisplayItemTurnPaused, Turn: &zotigosession.DisplayTurn{ID: "turn-1", Reason: "need_approval"}},
	} {
		if _, err := store.AppendDisplayItem(context.Background(), sessionID, item); err != nil {
			t.Fatalf("append display item: %v", err)
		}
	}

	runtime, err := newWorkerRuntime(context.Background(), workerRuntimeConfig{SessionID: sessionID, Store: store})
	if err != nil {
		t.Fatalf("create replacement worker runtime: %v", err)
	}
	defer runtime.Close()
	snapshot := runtime.agent.Snapshot()
	if snapshot.State != agent.StateIdle || len(snapshot.PendingActions) != 0 {
		t.Fatalf("unexpected recovered agent snapshot: %#v", snapshot)
	}
	items, ok, err := store.ListDisplayItems(context.Background(), sessionID)
	if err != nil || !ok {
		t.Fatalf("load recovered display items: ok=%v err=%v", ok, err)
	}
	if len(items) != 5 || items[3].Type != zotigosession.DisplayItemApprovalDecision || items[4].Type != zotigosession.DisplayItemTurnInterrupted {
		t.Fatalf("unexpected recovered display items: %#v", items)
	}
	if items[3].Approval.Decisions[0].Approved {
		t.Fatalf("replacement worker approved abandoned action: %#v", items[3].Approval.Decisions[0])
	}
}

func TestWorkerRuntimeCloseLeavesPendingApprovalTurnForReplacement(t *testing.T) {
	const sessionID = "sess-close-pending-approval"
	const providerName = "approval-close-boundary-test"
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	localExec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	ag, err := agent.New(config.ProfileConfig{Provider: providerName, Model: "test"}, localExec)
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	ag.Restore(agent.Snapshot{
		State: agent.StatePaused,
		PendingActions: []*agent.PendingAction{{
			ToolCallID: "call-1",
			Name:       "shell",
			Arguments:  `{"command":"touch file"}`,
			Decision:   agent.ActionDecision{Reason: "writes files"},
		}},
	})
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog(sessionID, source)
	if _, err := display.StartTurn(context.Background()); err != nil {
		t.Fatalf("start display turn: %v", err)
	}
	transport := newWorkerRuntimeTransport(sessionID, display, nil)
	turnCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	close(done)
	runtime := &workerRuntime{
		agent:      ag,
		transport:  transport,
		display:    display,
		turnActive: true,
		turnCancel: cancel,
		turnDone:   done,
		cleanup: func() {
			_ = localExec.Close()
		},
	}
	runtime.Close()
	if err := turnCtx.Err(); err != context.Canceled {
		t.Fatalf("turn context error = %v, want canceled", err)
	}
	items, _, err := source.LoadItems(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load display items: %v", err)
	}
	if got := lastOpenTurnID(items); got == "" {
		t.Fatalf("pending approval turn was interrupted during close: %#v", items)
	}
	if !hasPendingApproval(items) {
		t.Fatalf("close did not persist paused snapshot approval: %#v", items)
	}
}

func TestWorkerRuntimeApprovalClearsInMemoryStateWhenPersistenceFails(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	source.appendErr = func(_ string, item zotigosession.DisplayItem) error {
		if item.Type == zotigosession.DisplayItemApprovalRequest {
			return context.Canceled
		}
		return nil
	}
	display := newWorkerDisplayLog("sess-persist-failure", source)
	if _, err := display.StartTurn(context.Background()); err != nil {
		t.Fatalf("start display turn: %v", err)
	}
	transport := newWorkerRuntimeTransport("sess-persist-failure", display, nil)
	_, _, err := transport.beginApproval(context.Background(), []zotigotransport.PendingToolCall{{ID: "call-1", Name: "glob"}})
	if err == nil {
		t.Fatal("expected approval persistence failure")
	}
	transport.approvalMu.Lock()
	pending := transport.approval
	transport.approvalMu.Unlock()
	if pending != nil {
		t.Fatalf("in-memory approval was not cleared: %#v", pending)
	}
}

func TestWorkerRuntimeApprovalRegistrationIsIdempotent(t *testing.T) {
	const sessionID = "sess-idempotent-registration"
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog(sessionID, source)
	if _, err := display.StartTurn(context.Background()); err != nil {
		t.Fatalf("start display turn: %v", err)
	}
	transport := newWorkerRuntimeTransport(sessionID, display, nil)
	pending := []zotigotransport.PendingToolCall{{ID: "call-1", Name: "shell", Arguments: `{"command":"touch file"}`}}
	first, firstDecisionCh, err := transport.beginApproval(context.Background(), pending)
	if err != nil {
		t.Fatalf("begin first approval: %v", err)
	}
	second, secondDecisionCh, err := transport.beginApproval(context.Background(), pending)
	if err != nil {
		t.Fatalf("reuse approval registration: %v", err)
	}
	if first.ID != second.ID || firstDecisionCh != secondDecisionCh {
		t.Fatalf("approval registration was replaced: first=%#v second=%#v", first, second)
	}
	items, _, err := source.LoadItems(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load display items: %v", err)
	}
	requestCount := 0
	for _, item := range items {
		if item.Type == zotigosession.DisplayItemApprovalRequest {
			requestCount++
		}
	}
	if requestCount != 1 {
		t.Fatalf("approval request count = %d, want 1", requestCount)
	}
}
