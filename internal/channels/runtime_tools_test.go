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
	if _, err := store.PutConnection(ctx, Connection{ID: "connection-1", Provider: ProviderFeishu, Name: "bot", AppID: "app", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	conversation, err := store.EnsureConversation(ctx, "connection-1", "chat-1", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	conversation.SessionStrategy = SessionStrategyShared
	conversation.SessionID = "session-1"
	conversation.Enabled = true
	conversation.WorkspaceID = "workspace-1"
	conversation.SenderPolicy = SenderPolicyAll
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
	adapter := &fakeAdapter{resolveReferenced: func(_ context.Context, chatID, messageID string) (ReferencedMessage, error) {
		if chatID != "chat-1" || messageID != "quoted-message" {
			t.Fatalf("referenced lookup chat=%q message=%q", chatID, messageID)
		}
		return ReferencedMessage{ProviderID: messageID, Sender: Sender{ID: "user-3", DisplayName: "Carol"}, Text: "1+1", CreatedAt: time.Now()}, nil
	}}
	service.runs["connection-1"] = &adapterRun{adapter: adapter, generation: 1}
	result, err := service.ExecuteRuntimeTool(ctx, "session-1", &protocol.RequestContext{
		Actor: protocol.RequestActor{ID: "user-1"}, Source: "feishu", ConnectionID: "connection-1", ConversationID: conversation.ID, ExternalConversation: "chat-1", ExternalParentMessageID: "quoted-message",
	}, RuntimeToolReadMessages, json.RawMessage(`{"limit":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, `"message_id":"message-2"`) || !strings.Contains(result.Text, `"parent_message_id":"message-1"`) || !strings.Contains(result.Text, `"text":"not mentioned"`) || !strings.Contains(result.Text, `"sender_name":"Bob"`) || !strings.Contains(result.Text, `"message_id":"quoted-message"`) || !strings.Contains(result.Text, `"text":"1+1"`) {
		t.Fatalf("result = %s", result.Text)
	}
	if _, err := service.ExecuteRuntimeTool(ctx, "session-1", &protocol.RequestContext{
		Actor: protocol.RequestActor{ID: "user-1"}, Source: "feishu", ConnectionID: "connection-1", ConversationID: conversation.ID, ExternalConversation: "another-chat",
	}, RuntimeToolReadMessages, nil); err == nil {
		t.Fatal("mismatched conversation scope was accepted")
	}
}

func TestExecuteRuntimeToolFindsReferencedMessageOutsideRecentWindow(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.PutConnection(ctx, Connection{ID: "connection-1", Provider: ProviderFeishu, Name: "bot", AppID: "app", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	conversation, err := store.EnsureConversation(ctx, "connection-1", "chat-1", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	conversation.SessionStrategy = SessionStrategyShared
	conversation.SessionID = "session-1"
	conversation.Enabled = true
	conversation.WorkspaceID = "workspace-1"
	conversation.SenderPolicy = SenderPolicyAll
	conversation, err = store.PutConversation(ctx, conversation)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []InboundMessage{
		{MessageID: "quoted-message", ChatID: "chat-1", ChatType: "group", Sender: Sender{ID: "user-1"}, Text: "1+1", CreatedAt: time.Now().Add(-time.Minute)},
		{MessageID: "latest", ParentMessageID: "quoted-message", ChatID: "chat-1", ChatType: "group", Sender: Sender{ID: "user-1"}, Text: "answer this", CreatedAt: time.Now()},
	} {
		if _, err := store.RecordMessage(ctx, "connection-1", message); err != nil {
			t.Fatal(err)
		}
	}
	secrets, err := NewSecretStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), nil)
	service.runs["connection-1"] = &adapterRun{adapter: &fakeAdapter{resolveReferenced: func(context.Context, string, string) (ReferencedMessage, error) {
		t.Fatal("provider resolver called for a locally cached referenced message")
		return ReferencedMessage{}, nil
	}}}
	result, err := service.ExecuteRuntimeTool(ctx, "session-1", &protocol.RequestContext{
		Actor: protocol.RequestActor{ID: "user-1"}, Source: "feishu", ConnectionID: "connection-1", ConversationID: conversation.ID, ExternalConversation: "chat-1", ExternalParentMessageID: "quoted-message",
	}, RuntimeToolReadMessages, json.RawMessage(`{"limit":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, `"referenced_message":{"message_id":"quoted-message"`) || !strings.Contains(result.Text, `"text":"1+1"`) {
		t.Fatalf("result = %s", result.Text)
	}
}

func TestExecuteRuntimeToolRejectsMismatchedResolvedMessage(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.PutConnection(ctx, Connection{ID: "connection-1", Provider: ProviderFeishu, Name: "bot", AppID: "app", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	conversation, err := store.EnsureConversation(ctx, "connection-1", "chat-1", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	conversation.SessionStrategy = SessionStrategyShared
	conversation.SessionID = "session-1"
	conversation.Enabled = true
	conversation.WorkspaceID = "workspace-1"
	conversation.SenderPolicy = SenderPolicyAll
	conversation, err = store.PutConversation(ctx, conversation)
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := NewSecretStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), nil)
	service.runs["connection-1"] = &adapterRun{adapter: &fakeAdapter{resolveReferenced: func(context.Context, string, string) (ReferencedMessage, error) {
		return ReferencedMessage{ProviderID: "different-message", Text: "wrong"}, nil
	}}}
	_, err = service.ExecuteRuntimeTool(ctx, "session-1", &protocol.RequestContext{
		Actor: protocol.RequestActor{ID: "user-1"}, Source: "feishu", ConnectionID: "connection-1", ConversationID: conversation.ID, ExternalConversation: "chat-1", ExternalParentMessageID: "quoted-message",
	}, RuntimeToolReadMessages, nil)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v", err)
	}
}
