package zotigod

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/runner"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/tools"
	"github.com/jayyao97/zotigo/internal/hooks"
)

type channelHookDispatcher struct {
	events chan hooks.Event
}

func (d *channelHookDispatcher) Dispatch(_ context.Context, event hooks.Event) hooks.DispatchResult {
	d.events <- event
	return hooks.DispatchResult{}
}

type turnUsageProvider struct {
	release <-chan struct{}
}

func (p *turnUsageProvider) Name() string { return "turn-usage" }

func (p *turnUsageProvider) StreamChat(context.Context, []protocol.Message, []tools.Tool, ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	events := make(chan protocol.Event, 2)
	go func() {
		if p.release != nil {
			<-p.release
		}
		events <- protocol.NewTextDeltaEvent("done")
		finish := protocol.NewFinishEvent(protocol.FinishReasonStop)
		finish.Usage = &protocol.Usage{
			InputTokens: 10, OutputTokens: 4, CacheReadInputTokens: 6,
		}
		events <- finish
		close(events)
	}()
	return events, nil
}

type turnErrorProvider struct{}

func (p *turnErrorProvider) Name() string { return "turn-error" }

func (p *turnErrorProvider) StreamChat(context.Context, []protocol.Message, []tools.Tool, ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	events := make(chan protocol.Event, 1)
	events <- protocol.NewErrorEvent(errors.New("provider unavailable"))
	close(events)
	return events, nil
}

func TestWorkerRuntimeDispatchesTurnHooksWithUsage(t *testing.T) {
	const providerName = "zotigod-turn-hooks-test"
	release := make(chan struct{})
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) {
		return &turnUsageProvider{release: release}, nil
	})

	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "sess-turn-hooks", CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}

	localExec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = localExec.Close() })
	ag, err := agent.New(config.ProfileConfig{Provider: providerName}, localExec)
	if err != nil {
		t.Fatal(err)
	}
	display := newWorkerDisplayLog("sess-turn-hooks", &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}})
	transport := newWorkerRuntimeTransport("sess-turn-hooks", display, nil)
	t.Cleanup(func() { _ = transport.Close() })
	dispatcher := &channelHookDispatcher{events: make(chan hooks.Event, 3)}
	runtime := &workerRuntime{
		sessionID: "sess-turn-hooks", workDir: "/workspace", store: store, agent: ag,
		runner: runner.New(ag, transport), transport: transport, display: display,
		hooks: dispatcher, hookModel: "model-test",
	}

	if err := runtime.startMessageTurn(context.Background(), "message-1", 1, &messageCommandPayload{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	runtime.profileMu.Lock()
	runtime.hookModel = "model-after-start"
	runtime.profileMu.Unlock()
	close(release)
	events := make([]hooks.Event, 0, 3)
	for len(events) < 3 {
		select {
		case event := <-dispatcher.events:
			events = append(events, event)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for turn hooks: %#v", events)
		}
	}

	if events[0].EventName != hooks.TurnStart || events[0].Turn == nil || events[0].Turn.Status != "running" || events[0].Turn.Model != "model-test" {
		t.Fatalf("unexpected TurnStart event: %#v", events[0])
	}
	if events[1].EventName != hooks.UserPromptSubmit || events[1].Prompt == nil || events[1].Prompt.Text != "hello" {
		t.Fatalf("unexpected UserPromptSubmit event: %#v", events[1])
	}
	end := events[2]
	if end.EventName != hooks.TurnEnd || end.TurnID == "" || end.Turn == nil || end.Turn.Status != "completed" || end.Turn.Model != "model-test" || end.Turn.Usage == nil {
		t.Fatalf("unexpected TurnEnd event: %#v", end)
	}
	if got, want := *end.Turn.Usage, (hooks.UsagePayload{
		InputTokens: 10, OutputTokens: 4, TotalTokens: 20, CacheReadInputTokens: 6,
	}); got != want {
		t.Fatalf("turn usage = %#v, want %#v", got, want)
	}
	if events[0].TurnID != end.TurnID || events[1].TurnID != end.TurnID {
		t.Fatalf("turn IDs do not match: %#v", events)
	}
}

func TestWorkerRuntimeUsesPersistedProviderErrorForTurnEndStatus(t *testing.T) {
	const providerName = "zotigod-turn-error-hooks-test"
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) {
		return &turnErrorProvider{}, nil
	})

	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "sess-turn-error", CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}

	localExec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = localExec.Close() })
	ag, err := agent.New(config.ProfileConfig{Provider: providerName}, localExec)
	if err != nil {
		t.Fatal(err)
	}
	displayItems := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog("sess-turn-error", displayItems)
	transport := newWorkerRuntimeTransport("sess-turn-error", display, nil)
	t.Cleanup(func() { _ = transport.Close() })
	dispatcher := &channelHookDispatcher{events: make(chan hooks.Event, 3)}
	runtime := &workerRuntime{
		sessionID: "sess-turn-error", workDir: "/workspace", store: store, agent: ag,
		runner: runner.New(ag, transport), transport: transport, display: display,
		hooks: dispatcher, hookModel: "model-test",
	}

	if err := runtime.startMessageTurn(context.Background(), "message-1", 1, &messageCommandPayload{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	var end hooks.Event
	for end.EventName != hooks.TurnEnd {
		select {
		case end = <-dispatcher.events:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for TurnEnd")
		}
	}
	if end.Turn == nil || end.Turn.Status != "failed" {
		t.Fatalf("TurnEnd event = %#v", end)
	}
	items := displayItems.items["sess-turn-error"]
	if len(items) < 3 || items[len(items)-1].Type != zotigosession.DisplayItemTurnFailed || items[len(items)-1].Turn == nil || items[len(items)-1].Turn.ID != end.TurnID {
		t.Fatalf("terminal display items = %#v", items)
	}
}

func TestTurnUsageDeltaUsesCumulativeLedger(t *testing.T) {
	before := protocol.Usage{InputTokens: 8, OutputTokens: 2, CacheReadInputTokens: 10}.Normalized()
	after := before.Add(protocol.Usage{InputTokens: 5, OutputTokens: 3, CacheCreationInputTokens: 4}.Normalized())
	if got, want := turnUsageDelta(before, after), (protocol.Usage{
		InputTokens: 5, OutputTokens: 3, TotalTokens: 12, CacheCreationInputTokens: 4,
	}); got != want {
		t.Fatalf("usage delta = %#v, want %#v", got, want)
	}
}

func TestTurnUsageDeltaDoesNotEmitNegativeCounts(t *testing.T) {
	before := protocol.Usage{InputTokens: 8, OutputTokens: 4, TotalTokens: 12, CacheReadInputTokens: 3}
	after := protocol.Usage{InputTokens: 2, OutputTokens: 5, TotalTokens: 7, CacheReadInputTokens: 1}
	if got, want := turnUsageDelta(before, after), (protocol.Usage{OutputTokens: 1}); got != want {
		t.Fatalf("usage delta = %#v, want %#v", got, want)
	}
}
