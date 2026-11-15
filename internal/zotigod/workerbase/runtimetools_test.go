package workerbase

import (
	"context"
	"errors"
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

func TestChannelReadMessagesToolUsesActiveRequestContext(t *testing.T) {
	results := make(chan wire.RuntimeToolResult, 1)
	writer := &Writer{sendCh: make(chan wire.Message, 1), done: make(chan struct{})}
	state := &ChannelRuntimeToolState{Client: &RuntimeToolClient{Writer: writer, Results: results}}
	state.SetRequestContext(&protocol.RequestContext{
		Source: "feishu", ConnectionID: "connection-1", ConversationID: "conversation-1", ExternalConversation: "chat-1",
	})
	tool := &ChannelReadMessagesTool{State: state}

	resultCh := make(chan any, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := tool.Execute(context.Background(), nil, `{"limit":5}`)
		resultCh <- result
		errCh <- err
	}()

	message := <-writer.sendCh
	request := message.RuntimeToolRequest
	if request == nil || request.Name != "read_messages" || request.RequestContext.ConnectionID != "connection-1" || string(request.Arguments) != `{"limit":5}` {
		t.Fatalf("runtime tool request = %#v", request)
	}
	results <- wire.RuntimeToolResult{RequestID: request.RequestID, Text: `{"messages":[]}`}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if result := <-resultCh; result != `{"messages":[]}` {
		t.Fatalf("result = %#v", result)
	}
}

func TestChannelReadMessagesToolRequiresChannelTurn(t *testing.T) {
	tool := &ChannelReadMessagesTool{State: &ChannelRuntimeToolState{}}
	if _, err := tool.Execute(context.Background(), nil, `{}`); err == nil {
		t.Fatal("tool accepted a call outside a Channel turn")
	}
}

func TestSessionToolTransportCancellation(t *testing.T) {
	writer := &Writer{sendCh: make(chan wire.Message, 1), done: make(chan struct{})}
	client := &RuntimeToolClient{Writer: writer, Results: make(chan wire.RuntimeToolResult)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.request(ctx, wire.RuntimeToolRequest{Namespace: "zotigo", Name: "list_sessions", CallID: "stable-call"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
}
