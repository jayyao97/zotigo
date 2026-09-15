package zotigod

import (
	"context"
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
)

func TestChannelReadMessagesToolUsesActiveRequestContext(t *testing.T) {
	results := make(chan workerRuntimeToolResult, 1)
	writer := &workerClientWriter{sendCh: make(chan workerMessage, 1), done: make(chan struct{})}
	state := &channelRuntimeToolState{client: &workerRuntimeToolClient{writer: writer, results: results}}
	state.setRequestContext(&protocol.RequestContext{
		Source: "feishu", ConnectionID: "connection-1", ConversationID: "conversation-1", ExternalConversation: "chat-1",
	})
	tool := &channelReadMessagesTool{state: state}

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
	results <- workerRuntimeToolResult{RequestID: request.RequestID, Text: `{"messages":[]}`}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if result := <-resultCh; result != `{"messages":[]}` {
		t.Fatalf("result = %#v", result)
	}
}

func TestChannelReadMessagesToolRequiresChannelTurn(t *testing.T) {
	tool := &channelReadMessagesTool{state: &channelRuntimeToolState{}}
	if _, err := tool.Execute(context.Background(), nil, `{}`); err == nil {
		t.Fatal("tool accepted a call outside a Channel turn")
	}
}
