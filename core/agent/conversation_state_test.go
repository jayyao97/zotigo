package agent_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

type failingHistoryRecorder struct{}

func (failingHistoryRecorder) RecordHistory(agent.HistoryMutation) error {
	return errors.New("disk full")
}

type failingToolResultRecorder struct{}

func (failingToolResultRecorder) RecordHistory(mutation agent.HistoryMutation) error {
	for _, message := range mutation.Messages {
		if message.Role == protocol.RoleTool {
			return errors.New("disk full")
		}
	}
	return nil
}

type countingHistoryProvider struct{ calls atomic.Int32 }

func (p *countingHistoryProvider) Name() string { return "history-journal-test" }
func (p *countingHistoryProvider) StreamChat(context.Context, []protocol.Message, []tools.Tool, ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	p.calls.Add(1)
	ch := make(chan protocol.Event)
	close(ch)
	return ch, nil
}

type toolStartRecorder struct {
	started atomic.Bool
	err     error
}

func (r *toolStartRecorder) RecordToolExecutionStarted(context.Context, *agent.ToolCall) error {
	r.started.Store(true)
	return r.err
}

func TestHistoryRecorderFailureStopsBeforeProviderCall(t *testing.T) {
	provider := &countingHistoryProvider{}
	providers.Register("history-journal-test", func(config.ProfileConfig) (providers.Provider, error) { return provider, nil })
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: "history-journal-test", Model: "test"}, exec, agent.WithHistoryRecorder(failingHistoryRecorder{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Run(context.Background(), "question"); err == nil {
		t.Fatal("expected WAL failure")
	}
	if provider.calls.Load() != 0 {
		t.Fatal("provider was called before user history became durable")
	}
	if len(ag.Snapshot().History) != 0 {
		t.Fatal("in-memory history advanced after WAL failure")
	}
}

func TestToolExecutionRecorderRunsBeforeMiddleware(t *testing.T) {
	const providerName = "tool-start-order-test"
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) {
		return &StepMockProvider{}, nil
	})
	recorder := &toolStartRecorder{}
	middlewareRan := atomic.Bool{}
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: providerName}, exec,
		agent.WithTools(&TimeTool{}),
		agent.WithToolExecutionRecorder(recorder),
		agent.WithMiddleware(func(next agent.Next) agent.Next {
			return func(ctx context.Context, call *agent.ToolCall) (any, error) {
				if !recorder.started.Load() {
					return nil, errors.New("middleware ran before durable tool start")
				}
				middlewareRan.Store(true)
				return next(ctx, call)
			}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	events, err := ag.Run(context.Background(), "use a tool")
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if !middlewareRan.Load() {
		t.Fatal("middleware did not run after durable tool start")
	}
}

func TestToolExecutionRecorderFailureSkipsMiddleware(t *testing.T) {
	const providerName = "tool-start-failure-test"
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) {
		return &StepMockProvider{}, nil
	})
	recorder := &toolStartRecorder{err: errors.New("disk full")}
	middlewareRan := atomic.Bool{}
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: providerName}, exec,
		agent.WithTools(&TimeTool{}),
		agent.WithToolExecutionRecorder(recorder),
		agent.WithMiddleware(func(next agent.Next) agent.Next {
			return func(ctx context.Context, call *agent.ToolCall) (any, error) {
				middlewareRan.Store(true)
				return next(ctx, call)
			}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	events, err := ag.Run(context.Background(), "use a tool")
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if middlewareRan.Load() {
		t.Fatal("middleware ran after durable tool start failed")
	}
}

func TestToolOutputJournalFailurePreservesPausedState(t *testing.T) {
	const providerName = "tool-output-journal-failure-test"
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) {
		return &countingHistoryProvider{}, nil
	})
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: providerName}, exec, agent.WithHistoryRecorder(failingHistoryRecorder{}))
	if err != nil {
		t.Fatal(err)
	}
	ag.Restore(agent.Snapshot{
		State: agent.StatePaused,
		PendingActions: []*agent.PendingAction{{
			ToolCallID: "call-1",
			Name:       "shell",
		}},
	})
	_, err = ag.SubmitToolOutputs(context.Background(), []protocol.ToolResult{
		protocol.NewTextToolResult("call-1", "denied", true),
	})
	if err == nil {
		t.Fatal("expected tool output journal failure")
	}
	snapshot := ag.Snapshot()
	if snapshot.State != agent.StatePaused || len(snapshot.PendingActions) != 1 {
		t.Fatalf("approval state advanced before tool output was durable: %#v", snapshot)
	}
}

func TestExecutedToolJournalFailureCannotBeRetried(t *testing.T) {
	const providerName = "executed-tool-journal-failure-test"
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) {
		return &countingHistoryProvider{}, nil
	})
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exec.Close()
	calls := 0
	ag, err := agent.New(config.ProfileConfig{Provider: providerName}, exec,
		agent.WithTools(&CountingApprovalTool{name: "side_effect", calls: &calls}),
		agent.WithHistoryRecorder(failingHistoryRecorder{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	ag.Restore(agent.Snapshot{
		State: agent.StatePaused,
		PendingActions: []*agent.PendingAction{{
			ToolCallID: "call-1",
			Name:       "side_effect",
		}},
	})
	events, err := ag.ApproveAndExecutePendingActions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if calls != 1 {
		t.Fatalf("tool calls = %d, want 1", calls)
	}
	snapshot := ag.Snapshot()
	if snapshot.State != agent.StateDurabilityFailed || len(snapshot.PendingActions) != 0 {
		t.Fatalf("post-execution journal failure remained retryable: %#v", snapshot)
	}
	if _, err := ag.ApproveAndExecutePendingActions(context.Background()); err == nil {
		t.Fatal("expected second approval to be rejected")
	}
	if _, err := ag.Run(context.Background(), "retry"); err == nil {
		t.Fatal("expected new input to be rejected after durability failure")
	}
	if calls != 1 {
		t.Fatalf("tool executed again after durability failure: %d calls", calls)
	}
}

func TestAutoExecutedToolJournalFailureCannotContinue(t *testing.T) {
	const providerName = "auto-tool-journal-failure-test"
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) {
		return &StepMockProvider{}, nil
	})
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exec.Close()
	var calls atomic.Int32
	ag, err := agent.New(config.ProfileConfig{Provider: providerName}, exec,
		agent.WithTools(&TimeTool{}),
		agent.WithHistoryRecorder(failingToolResultRecorder{}),
		agent.WithMiddleware(func(next agent.Next) agent.Next {
			return func(ctx context.Context, call *agent.ToolCall) (any, error) {
				calls.Add(1)
				return next(ctx, call)
			}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	events, err := ag.Run(context.Background(), "use a tool")
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if calls.Load() != 1 {
		t.Fatalf("tool calls = %d, want 1", calls.Load())
	}
	if snapshot := ag.Snapshot(); snapshot.State != agent.StateDurabilityFailed {
		t.Fatalf("state = %q, want durability failure", snapshot.State)
	}
	if _, err := ag.Run(context.Background(), "continue"); err == nil {
		t.Fatal("expected continuation to be rejected after durability failure")
	}
	if calls.Load() != 1 {
		t.Fatalf("tool executed again after durability failure: %d calls", calls.Load())
	}
}

func TestConversationSnapshotOwnsNestedHistory(t *testing.T) {
	const providerName = "conversation-snapshot-ownership-test"
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) {
		return &countingHistoryProvider{}, nil
	})
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: providerName}, exec)
	if err != nil {
		t.Fatal(err)
	}
	sharedSlice := []int{1, 2}
	seed := []protocol.Message{{
		Role: protocol.RoleTool,
		Content: []protocol.ContentPart{{
			Type:  protocol.ContentTypeToolResult,
			Image: &protocol.MediaPart{Data: []byte("image")},
			ToolResult: &protocol.ToolResult{
				ToolCallID: "call-1",
				JSON:       map[string]any{"nested": []any{"original"}},
				Metadata:   map[string]any{"status": map[string]any{"value": "original"}},
			},
		}},
		Metadata: &protocol.MessageMetadata{Raw: map[string]any{
			"nested":      map[string]any{"value": "original"},
			"typed_map":   map[string]int{"value": 1},
			"typed_slice": []int{1, 2},
			"full_slice":  sharedSlice,
			"short_slice": sharedSlice[:1],
			"usage":       &protocol.Usage{InputTokens: 1},
		}},
	}}
	ag.Restore(agent.Snapshot{History: seed})
	seed[0].Content[0].Image.Data[0] = 'X'
	seed[0].Content[0].ToolResult.JSON.(map[string]any)["nested"].([]any)[0] = "changed input"
	seed[0].Metadata.Raw["nested"].(map[string]any)["value"] = "changed input"
	seed[0].Metadata.Raw["typed_map"].(map[string]int)["value"] = 2
	seed[0].Metadata.Raw["typed_slice"].([]int)[0] = 2
	seed[0].Metadata.Raw["usage"].(*protocol.Usage).InputTokens = 2
	first := ag.Snapshot()
	part := first.History[0].Content[0]
	if len(first.History[0].Metadata.Raw["full_slice"].([]int)) != 2 || len(first.History[0].Metadata.Raw["short_slice"].([]int)) != 1 {
		t.Fatalf("Snapshot changed same-start subslice lengths: %#v", first.History[0].Metadata.Raw)
	}
	if string(part.Image.Data) != "image" || part.ToolResult.JSON.(map[string]any)["nested"].([]any)[0] != "original" || first.History[0].Metadata.Raw["nested"].(map[string]any)["value"] != "original" || first.History[0].Metadata.Raw["typed_map"].(map[string]int)["value"] != 1 || first.History[0].Metadata.Raw["typed_slice"].([]int)[0] != 1 || first.History[0].Metadata.Raw["usage"].(*protocol.Usage).InputTokens != 1 {
		t.Fatalf("Restore retained caller-owned nested history: %#v", first.History)
	}
	part.Image.Data[0] = 'Y'
	part.ToolResult.Metadata["status"].(map[string]any)["value"] = "changed snapshot"
	first.History[0].Metadata.Raw["nested"].(map[string]any)["value"] = "changed snapshot"
	first.History[0].Metadata.Raw["typed_map"].(map[string]int)["value"] = 3
	first.History[0].Metadata.Raw["typed_slice"].([]int)[0] = 3
	first.History[0].Metadata.Raw["usage"].(*protocol.Usage).InputTokens = 3
	second := ag.Snapshot()
	part = second.History[0].Content[0]
	if string(part.Image.Data) != "image" || part.ToolResult.Metadata["status"].(map[string]any)["value"] != "original" || second.History[0].Metadata.Raw["nested"].(map[string]any)["value"] != "original" || second.History[0].Metadata.Raw["typed_map"].(map[string]int)["value"] != 1 || second.History[0].Metadata.Raw["typed_slice"].([]int)[0] != 1 || second.History[0].Metadata.Raw["usage"].(*protocol.Usage).InputTokens != 1 {
		t.Fatalf("Snapshot exposed nested history: %#v", second.History)
	}
}
