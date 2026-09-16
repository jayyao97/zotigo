package channels

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
)

func TestExecuteRuntimeToolReadsOnlyBoundConversation(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.PutConnection(ctx, Connection{ID: "connection-1", Provider: ProviderFeishu, Name: "bot", AppID: "app"}); err != nil {
		t.Fatal(err)
	}
	conversation, err := store.EnsureConversation(ctx, "connection-1", "chat-1", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	conversation.SessionStrategy = SessionStrategyShared
	conversation.SessionID = "session-1"
	conversation, err = store.PutConversation(ctx, conversation)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []InboundMessage{
		{MessageID: "message-1", ChatID: "chat-1", ChatType: "group", Sender: Sender{ID: "user-1", DisplayName: "Alice"}, Text: "not mentioned", CreatedAt: time.Now().Add(-time.Minute)},
		{MessageID: "message-2", ParentMessageID: "message-1", ChatID: "chat-1", ChatType: "group", Sender: Sender{ID: "user-2", DisplayName: "Bob"}, Text: "latest", CreatedAt: time.Now()},
	} {
		if _, err := store.RecordMessage(ctx, "connection-1", message); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := store.GetMessageByProviderID(ctx, "connection-1", "message-2")
	if err != nil || stored.ParentProviderID != "message-1" {
		t.Fatalf("stored parent message = %+v err=%v", stored, err)
	}
	secrets, err := NewSecretStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), nil)
	result, err := service.ExecuteRuntimeTool(ctx, "session-1", &protocol.RequestContext{
		Source: "feishu", ConnectionID: "connection-1", ConversationID: conversation.ID, ExternalConversation: "chat-1",
	}, RuntimeToolReadMessages, json.RawMessage(`{"limit":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, `"message_id":"message-2"`) || !strings.Contains(result.Text, `"parent_message_id":"message-1"`) || !strings.Contains(result.Text, `"text":"not mentioned"`) || !strings.Contains(result.Text, `"sender_name":"Bob"`) {
		t.Fatalf("result = %s", result.Text)
	}
	if _, err := service.ExecuteRuntimeTool(ctx, "session-1", &protocol.RequestContext{
		Source: "feishu", ConnectionID: "connection-1", ConversationID: conversation.ID, ExternalConversation: "another-chat",
	}, RuntimeToolReadMessages, nil); err == nil {
		t.Fatal("mismatched conversation scope was accepted")
	}
}
