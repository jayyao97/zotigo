package agent_test

import (
	"context"
	"testing"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

type calibratedUsageProvider struct{ calls int }

func (*calibratedUsageProvider) Name() string { return "calibrated-integration" }
func (p *calibratedUsageProvider) StreamChat(_ context.Context, _ []protocol.Message, _ []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	p.calls++
	events := make(chan protocol.Event, 2)
	events <- protocol.NewTextDeltaEvent("answer")
	usage := &protocol.Usage{InputTokens: 100}
	if p.calls == 1 {
		usage = &protocol.Usage{InputTokens: 2, CacheCreationInputTokens: 835, CacheReadInputTokens: 526326}
	}
	finish := protocol.NewFinishEvent(protocol.FinishReasonStop)
	finish.Usage = usage
	events <- finish
	close(events)
	return events, nil
}

func TestAgentCalibratesActualProviderResponseAcrossRestore(t *testing.T) {
	p := &calibratedUsageProvider{}
	providers.Register(p.Name(), func(config.ProfileConfig) (providers.Provider, error) { return p, nil })
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exec.Close()
	cfg := config.ProfileConfig{Provider: p.Name(), Model: "test", ContextWindow: 512000}
	ag, err := agent.New(cfg, exec)
	if err != nil {
		t.Fatal(err)
	}
	ag.Restore(agent.Snapshot{History: buildSeedHistory(40)})
	run := func(text string) int {
		events, err := ag.Run(context.Background(), text)
		if err != nil {
			t.Fatal(err)
		}
		compacted := 0
		for event := range events {
			if event.Error != nil {
				t.Fatal(event.Error)
			}
			if event.Type == protocol.EventTypeContextCompacted {
				compacted++
			}
		}
		return compacted
	}
	run("first request")
	snapshot := ag.Snapshot()
	last := snapshot.History[len(snapshot.History)-1]
	if last.Metadata == nil || last.Metadata.ContextInput == nil {
		t.Fatal("request calibration was not persisted")
	}
	ag, err = agent.New(cfg, exec)
	if err != nil {
		t.Fatal(err)
	}
	ag.Restore(snapshot)
	if got := run("continue"); got != 1 {
		t.Fatalf("expected one API-driven compaction after restore, got %d", got)
	}
	if got := run("continue again"); got != 0 {
		t.Fatalf("repeated compaction: %d", got)
	}
}
