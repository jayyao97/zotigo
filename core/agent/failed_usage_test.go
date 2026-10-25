package agent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

type failedUsageProvider struct {
	name    string
	scripts [][]protocol.Event
}

func (p *failedUsageProvider) Name() string { return p.name }
func (p *failedUsageProvider) StreamChat(context.Context, []protocol.Message, []tools.Tool, ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	events := p.scripts[0]
	p.scripts = p.scripts[1:]
	stream := make(chan protocol.Event, len(events))
	for _, event := range events {
		stream <- event
	}
	close(stream)
	return stream, nil
}

func TestAgentFailedCallUsageIncludesEarlierCallsAndSnapshot(t *testing.T) {
	for _, previousCall := range []bool{false, true} {
		name := "failed-usage-first"
		if previousCall {
			name = "failed-usage-after-tool"
		}
		t.Run(name, func(t *testing.T) {
			successUsage := protocol.Usage{InputTokens: 100, OutputTokens: 10}.Normalized()
			failedUsage := protocol.Usage{InputTokens: 50, OutputTokens: 2}.Normalized()
			failure := protocol.NewErrorEvent(errors.New("stream failed"))
			failure.Usage = &failedUsage
			mock := &failedUsageProvider{name: name}
			want := failedUsage
			if previousCall {
				mock.scripts = append(mock.scripts, []protocol.Event{
					{Type: protocol.EventTypeToolCallEnd, ToolCall: &protocol.ToolCall{ID: "call-time", Name: "get_time", Arguments: "{}"}},
					{Type: protocol.EventTypeFinish, FinishReason: protocol.FinishReasonToolCalls, Usage: &successUsage},
				})
				want = want.Add(successUsage).Normalized()
			}
			mock.scripts = append(mock.scripts, []protocol.Event{failure})
			providers.Register(name, func(config.ProfileConfig) (providers.Provider, error) { return mock, nil })
			exec, err := executor.NewLocalExecutor(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer exec.Close()
			ag, err := agent.New(config.ProfileConfig{Provider: name}, exec, agent.WithTools(&TimeTool{}))
			if err != nil {
				t.Fatal(err)
			}
			stream, err := ag.Run(context.Background(), "check time")
			if err != nil {
				t.Fatal(err)
			}
			var terminal *protocol.Usage
			for event := range stream {
				if event.Type == protocol.EventTypeError {
					terminal = event.Usage
				}
			}
			if terminal == nil || *terminal != want {
				t.Fatalf("terminal=%+v want=%+v", terminal, want)
			}
			snapshot := ag.Snapshot()
			if snapshot.CumulativeUsage != want {
				t.Fatalf("hook snapshot usage=%+v want=%+v", snapshot.CumulativeUsage, want)
			}
			ag.Restore(snapshot)
			if ag.Snapshot().CumulativeUsage != want {
				t.Fatal("restore lost failed call usage")
			}
		})
	}
}
