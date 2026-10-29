package services

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

type summaryTestProvider struct {
	events   []protocol.Event
	messages []protocol.Message
}

func (*summaryTestProvider) Name() string { return "summary-test" }
func (p *summaryTestProvider) StreamChat(_ context.Context, messages []protocol.Message, tools []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	if len(tools) != 0 {
		return nil, errors.New("summary must not execute tools")
	}
	p.messages = messages
	ch := make(chan protocol.Event, len(p.events))
	for _, e := range p.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}
func TestProviderSummarizerRequiresCompleteText(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []protocol.Event
		ok     bool
	}{
		{"success", []protocol.Event{protocol.NewTextDeltaEvent("summary"), protocol.NewFinishEvent(protocol.FinishReasonStop)}, true},
		{"partial error", []protocol.Event{protocol.NewTextDeltaEvent("partial"), protocol.NewErrorEvent(errors.New("upstream failed"))}, false},
		{"closed stream", []protocol.Event{protocol.NewTextDeltaEvent("partial")}, false},
		{"truncated", []protocol.Event{protocol.NewTextDeltaEvent("partial"), protocol.NewFinishEvent(protocol.FinishReasonLength)}, false},
		{"empty", []protocol.Event{protocol.NewFinishEvent(protocol.FinishReasonStop)}, false},
		{"whitespace", []protocol.Event{protocol.NewTextDeltaEvent(" \n"), protocol.NewFinishEvent(protocol.FinishReasonStop)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewProviderSummarizer(&summaryTestProvider{events: tc.events})
			result, err := s.SummarizeMessages(context.Background(), []protocol.Message{protocol.NewUserMessage("history")})
			if (err == nil) != tc.ok || (!tc.ok && result != "") {
				t.Fatalf("result=%q err=%v", result, err)
			}
		})
	}
}
func TestProviderSummarizerKeepsToolEvidenceAndHonorsCancellation(t *testing.T) {
	p := &summaryTestProvider{events: []protocol.Event{protocol.NewTextDeltaEvent("summary"), protocol.NewFinishEvent(protocol.FinishReasonStop)}}
	s := NewProviderSummarizer(p)
	history := []protocol.Message{
		{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{{Type: protocol.ContentTypeToolCall, ToolCall: &protocol.ToolCall{Name: "shell", Arguments: `{"command":"query metrics"}`}}}},
		protocol.NewToolMessage([]protocol.ToolResult{{Text: strings.Repeat("数据", 600) + "关键结论", JSON: map[string]any{"metric": 42}}}),
	}
	if _, err := s.SummarizeMessages(context.Background(), history); err != nil {
		t.Fatal(err)
	}
	input := p.messages[0].Content[0].Text
	for _, want := range []string{"query metrics", "关键结论", `"metric":42`} {
		if !strings.Contains(input, want) {
			t.Fatalf("missing %s", want)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.SummarizeMessages(ctx, history); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

type cancelledSummaryProvider struct {
	started chan struct{}
	exited  chan struct{}
}

func (*cancelledSummaryProvider) Name() string { return "cancelled-summary" }
func (p *cancelledSummaryProvider) StreamChat(ctx context.Context, _ []protocol.Message, _ []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	events := make(chan protocol.Event)
	go func() {
		defer close(p.exited)
		defer close(events)
		close(p.started)
		<-ctx.Done()
		events <- protocol.NewErrorEvent(ctx.Err())
	}()
	return events, nil
}
func TestSummarizerCancellationWaitsForProviderCleanup(t *testing.T) {
	p := &cancelledSummaryProvider{started: make(chan struct{}), exited: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := NewProviderSummarizer(p).SummarizeText(ctx, "history", "summarize"); result <- err }()
	<-p.started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("summary cancellation did not finish")
	}
	select {
	case <-p.exited:
	case <-time.After(2 * time.Second):
		t.Fatal("provider still blocked after summary returned")
	}
}
