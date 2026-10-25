package providers

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/tools"
)

type retryTestProvider struct {
	calls  int
	events []protocol.Event
}

func (p *retryTestProvider) Name() string { return "test" }
func (p *retryTestProvider) StreamChat(context.Context, []protocol.Message, []tools.Tool, ...StreamChatOption) (<-chan protocol.Event, error) {
	p.calls++
	ch := make(chan protocol.Event, len(p.events))
	for _, event := range p.events {
		ch <- event
	}
	close(ch)
	return ch, nil
}

func TestTransientErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		want   bool
	}{
		{"http503", errors.New("unavailable"), 503, true},
		{"network timeout", &net.DNSError{Err: "TLS handshake timeout", IsTimeout: true}, 0, true},
		{"http401", errors.New("unauthorized"), 401, false},
		{"http413", errors.New("too large"), 413, false},
		{"gemini", errors.New("iterateResponseStream: invalid stream chunk: event: error\ndata: {\"error\":{\"code\":503,\"message\":\"no capacity\"}}"), 200, true},
		{"anthropic", errors.New(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`), 200, true},
		{"openai", errors.New(`{"error":{"code":"rate_limit_exceeded"}}`), 200, true},
		{"prose", errors.New("server_error 503 maybe"), 200, false},
		{"cancel", context.Canceled, 503, false},
		{"request deadline", context.DeadlineExceeded, 0, true},
		{"context", ErrContextLengthExceeded, 503, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := transientError(tc.err, &retryAttempt{status: tc.status}); got != tc.want {
				t.Fatalf("retry = %v, want %v", got, tc.want)
			}
		})
	}
	if transientError(errors.New("busy"), &retryAttempt{status: 503, header: http.Header{"X-Should-Retry": {"false"}}}) {
		t.Fatal("ignored upstream retry veto")
	}
}

func TestRetryHTTPTimeoutWithLiveCaller(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = 50 * time.Millisecond
	ctx := context.Background()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(req)
	if response != nil {
		response.Body.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("expected inner timeout with live caller: err=%v caller=%v", err, ctx.Err())
	}
	inner := &retryTestProvider{events: []protocol.Event{protocol.NewErrorEvent(err)}}
	stream, _ := (&retryProvider{Provider: inner}).StreamChat(ctx, nil, nil)
	for range stream {
	}
	if inner.calls != 3 {
		t.Fatalf("request timeout calls=%d, want 3", inner.calls)
	}
}

func TestRetryCallerDeadlineStopsRequests(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	inner := &retryTestProvider{events: []protocol.Event{protocol.NewErrorEvent(context.DeadlineExceeded)}}
	stream, _ := (&retryProvider{Provider: inner}).StreamChat(ctx, nil, nil)
	var failures int
	for event := range stream {
		if errors.Is(event.Error, context.DeadlineExceeded) {
			failures++
		}
		if event.Retry != nil {
			t.Errorf("retry after caller deadline: %+v", event.Retry)
		}
	}
	if inner.calls != 0 || failures != 1 {
		t.Fatalf("calls=%d failures=%d", inner.calls, failures)
	}
}

func TestRetryNeverReplaysPublishedOutput(t *testing.T) {
	for _, event := range []protocol.Event{
		protocol.NewTextDeltaEvent("partial"), protocol.NewReasoningDeltaEvent("thinking"),
		{Type: protocol.EventTypeToolCallEnd, ToolCall: &protocol.ToolCall{ID: "call"}},
		{Type: protocol.EventTypeFinish, Usage: &protocol.Usage{InputTokens: 3}},
	} {
		t.Run(string(event.Type), func(t *testing.T) {
			inner := &retryTestProvider{events: []protocol.Event{event, protocol.NewErrorEvent(errors.New(`{"error":{"code":500}}`))}}
			stream, _ := (&retryProvider{Provider: inner}).StreamChat(context.Background(), nil, nil)
			count := 0
			for range stream {
				count++
			}
			if inner.calls != 1 || count != 2 {
				t.Fatalf("calls=%d events=%d", inner.calls, count)
			}
		})
	}
}

func TestRetryCancellationDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inner := &retryTestProvider{events: []protocol.Event{protocol.NewErrorEvent(errors.New(`{"error":{"code":503}}`))}}
	stream, _ := (&retryProvider{Provider: inner}).StreamChat(ctx, nil, nil)
	first := <-stream
	if first.Retry == nil || first.Retry.Attempt != 1 {
		t.Fatalf("expected retry event: %#v", first)
	}
	cancel()
	stopped := <-stream
	if stopped.Retry == nil || stopped.Retry.Status != "canceled" || stopped.Retry.RequestID != first.Retry.RequestID {
		t.Fatalf("missing stopped event: %+v", stopped)
	}
	last := <-stream
	if !errors.Is(last.Error, context.Canceled) {
		t.Fatalf("error = %v", last.Error)
	}
	for range stream {
	}
	if inner.calls != 1 {
		t.Fatal("request dispatched after cancellation")
	}
}

func TestRetryAfter(t *testing.T) {
	if got := retryDelay(http.Header{"Retry-After": {"10"}}, 0); got != 10*time.Second {
		t.Fatalf("delay = %v", got)
	}
}

func TestRetryBudgetIsBounded(t *testing.T) {
	inner := &retryTestProvider{events: []protocol.Event{protocol.NewErrorEvent(errors.New(`{"error":{"code":503}}`))}}
	stream, _ := (&retryProvider{Provider: inner}).StreamChat(context.Background(), nil, nil)
	retries, failures := 0, 0
	var statuses []string
	for event := range stream {
		if event.Retry != nil {
			statuses = append(statuses, event.Retry.Status)
		}
		if event.Retry != nil && event.Retry.Status == "waiting" {
			retries++
		}
		if event.Error != nil {
			failures++
		}
	}
	if inner.calls != 3 || retries != 2 || failures != 1 {
		t.Fatalf("calls=%d retries=%d failures=%d", inner.calls, retries, failures)
	}
	if len(statuses) != 5 || statuses[4] != "failed" {
		t.Fatalf("missing terminal retry state: %v", statuses)
	}
}
