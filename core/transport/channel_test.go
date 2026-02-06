package transport

import (
	"context"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
)

func TestChannelTransport_SendReceive(t *testing.T) {
	tr := NewChannelTransport(10)
	defer tr.Close()

	ctx := context.Background()

	// Send an event
	event := protocol.NewTextDeltaEvent("hello")
	err := tr.Send(ctx, event)
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	// Receive the event
	select {
	case received := <-tr.Events():
		if received.ContentPartDelta.Text != "hello" {
			t.Errorf("expected 'hello', got %q", received.ContentPartDelta.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestChannelTransport_SendInput(t *testing.T) {
	tr := NewChannelTransport(10)
	defer tr.Close()

	ctx := context.Background()

	// Send input
	input := UserInput{
		Type: UserInputMessage,
		Text: "test message",
	}
	err := tr.SendInput(ctx, input)
	if err != nil {
		t.Fatalf("SendInput failed: %v", err)
	}

	// Receive input
	select {
	case received := <-tr.Receive(ctx):
		if received.Text != "test message" {
			t.Errorf("expected 'test message', got %q", received.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for input")
	}
}

func TestChannelTransport_Approval(t *testing.T) {
	tr := NewChannelTransport(10)
	defer tr.Close()

	ctx := context.Background()

	// Start approval request in goroutine
	done := make(chan struct{})
	var results []ApprovalResult
	var err error

	go func() {
		results, err = tr.RequestApproval(ctx, []PendingToolCall{
			{ID: "call_1", Name: "read_file"},
		})
		close(done)
	}()

	// Wait for approval event
	select {
	case event := <-tr.Events():
		if event.FinishReason != "need_approval" {
			t.Errorf("expected need_approval, got %s", event.FinishReason)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for approval event")
	}

	// Send approval
	err = tr.SendApproval(ctx, []ApprovalResult{
		{ToolCallID: "call_1", Approved: true},
	})
	if err != nil {
		t.Fatalf("SendApproval failed: %v", err)
	}

	// Wait for result
	select {
	case <-done:
		if err != nil {
			t.Fatalf("RequestApproval failed: %v", err)
		}
		if len(results) != 1 || !results[0].Approved {
			t.Errorf("unexpected results: %v", results)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for approval result")
	}
}

func TestChannelTransport_Close(t *testing.T) {
	tr := NewChannelTransport(10)

	// Close transport
	err := tr.Close()
	if err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Try to send after close
	ctx := context.Background()
	err = tr.Send(ctx, protocol.Event{})
	if err != ErrTransportClosed {
		t.Errorf("expected ErrTransportClosed, got %v", err)
	}

	// Double close should be ok
	err = tr.Close()
	if err != nil {
		t.Errorf("double close should not error: %v", err)
	}
}

func TestChannelTransport_ContextCancellation(t *testing.T) {
	tr := NewChannelTransport(1) // Small buffer
	defer tr.Close()

	ctx, cancel := context.WithCancel(context.Background())

	// Fill the buffer
	tr.Send(ctx, protocol.Event{})

	// Cancel context
	cancel()

	// Try to send - should fail with context error
	err := tr.Send(ctx, protocol.Event{})
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}
