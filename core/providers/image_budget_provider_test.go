package providers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/tools"
)

type budgetTestProvider struct {
	calls          int
	partial        bool
	finishOnCancel bool
	started        chan struct{}
}

func (*budgetTestProvider) Name() string { return "budget-test" }
func (p *budgetTestProvider) StreamChat(ctx context.Context, _ []protocol.Message, _ []tools.Tool, _ ...StreamChatOption) (<-chan protocol.Event, error) {
	p.calls++
	if p.started != nil {
		close(p.started)
	}
	ch := make(chan protocol.Event)
	go func() {
		defer close(ch)
		if !p.partial {
			<-ctx.Done()
			if p.finishOnCancel {
				ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
			}
			return
		}
		ch <- protocol.NewTextDeltaEvent("already delivered")
		budget := ctx.Value(requestBudgetKey{}).(*requestBudget)
		budget.tooLarge.Store(true)
		budget.wireBytes.Store(1000)
		ch <- protocol.NewErrorEvent(errors.New("413"))
	}()
	return ch, nil
}

func TestImageBudgetPreservesFinishAfterCancellation(t *testing.T) {
	native := &budgetTestProvider{started: make(chan struct{}), finishOnCancel: true}
	p := &imageBudgetProvider{Provider: native, limit: defaultRequestBytes}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := p.StreamChat(ctx, []protocol.Message{protocol.NewUserMessage("test")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-native.started
	cancel()
	var events []protocol.Event
	for event := range stream {
		events = append(events, event)
	}
	if len(events) != 1 || events[0].Type != protocol.EventTypeFinish {
		t.Fatalf("terminal event lost: %v", events)
	}
}

func TestImageBudgetNeverRetriesPartialOutput(t *testing.T) {
	native := &budgetTestProvider{partial: true}
	p := &imageBudgetProvider{Provider: native, limit: defaultRequestBytes}
	stream, err := p.StreamChat(context.Background(), []protocol.Message{protocol.NewUserMessage("test")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var events []protocol.Event
	for event := range stream {
		events = append(events, event)
	}
	if native.calls != 1 || len(events) != 2 || !errors.Is(events[1].Error, ErrRequestTooLarge) {
		t.Fatalf("partial stream replayed: calls=%d events=%v", native.calls, events)
	}
}

func TestImageBudgetCancellationClosesStream(t *testing.T) {
	native := &budgetTestProvider{}
	p := &imageBudgetProvider{Provider: native, limit: defaultRequestBytes}
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := p.StreamChat(ctx, []protocol.Message{protocol.NewUserMessage("test")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case _, ok := <-stream:
		if ok {
			t.Fatal("unexpected event")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not close stream")
	}
}
