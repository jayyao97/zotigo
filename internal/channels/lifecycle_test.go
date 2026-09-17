package channels

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
)

func TestRuntimeToolRechecksRevokedAccess(t *testing.T) {
	for _, revoke := range []string{"connection", "group", "workspace", "sender", "topic"} {
		t.Run(revoke, func(t *testing.T) {
			ctx := context.Background()
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			connection, err := store.PutConnection(ctx, Connection{ID: "bot", Provider: ProviderFeishu, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			group, err := store.EnsureConversation(ctx, "bot", "chat", "group", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			group.Enabled, group.WorkspaceID, group.SenderPolicy = true, "workspace", SenderPolicySelected
			group.AllowedSenderIDs = []string{"owner"}
			if _, err = store.PutConversation(ctx, group); err != nil {
				t.Fatal(err)
			}
			topic, err := store.EnsureConversationRoot(ctx, "bot", "chat", "root", "", "group", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			topic.SessionID, topic.Enabled = "session", true
			if _, err = store.PutConversation(ctx, topic); err != nil {
				t.Fatal(err)
			}
			secrets, err := NewSecretStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			service := NewService(store, secrets, log.New(io.Discard, "", 0), nil)
			request := &protocol.RequestContext{Source: ProviderFeishu, ConnectionID: "bot", ConversationID: topic.ID, ExternalConversation: "chat", Actor: protocol.RequestActor{ID: "owner"}}
			if _, err = service.ExecuteRuntimeTool(ctx, "session", request, RuntimeToolReadMessages, nil); err != nil {
				t.Fatal(err)
			}
			switch revoke {
			case "connection":
				connection.Enabled = false
			case "group":
				group.Enabled = false
			case "workspace":
				group.WorkspaceID = ""
			case "sender":
				group.AllowedSenderIDs = nil
			case "topic":
				topic.Enabled = false
			}
			if _, err = store.PutConnection(ctx, connection); err != nil {
				t.Fatal(err)
			}
			if _, err = store.PutConversation(ctx, group); err != nil {
				t.Fatal(err)
			}
			if _, err = store.PutConversation(ctx, topic); err != nil {
				t.Fatal(err)
			}
			if _, err = service.ExecuteRuntimeTool(ctx, "session", request, RuntimeToolReadMessages, nil); err == nil || !strings.Contains(err.Error(), "no longer allowed") {
				t.Fatalf("revoked read: %v", err)
			}
		})
	}
}

func TestDeleteConnectionRequiresNoBoundSessions(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err = store.PutConnection(ctx, Connection{ID: "bot", Provider: ProviderFeishu}); err != nil {
		t.Fatal(err)
	}
	conversation, err := store.EnsureConversationRoot(ctx, "bot", "chat", "root", "", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	conversation.SessionID = "session"
	if _, err = store.PutConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	secrets, err := NewSecretStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = secrets.Set("bot", "secret"); err != nil {
		t.Fatal(err)
	}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), nil)
	if err = service.DeleteConnection(ctx, "bot"); !errors.Is(err, ErrConnectionBound) {
		t.Fatalf("delete bound: %v", err)
	}
	if _, exists, err := secrets.Get("bot"); err != nil || !exists {
		t.Fatalf("secret lost: %v", err)
	}
	unbind, release, err := service.GuardSessionUnbinding(ctx, "session")
	if err != nil {
		t.Fatal(err)
	}
	err = unbind()
	release()
	if err != nil {
		t.Fatal(err)
	}
	if err = service.DeleteConnection(ctx, "bot"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.GetConnection(ctx, "bot"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("connection remains: %v", err)
	}
}
