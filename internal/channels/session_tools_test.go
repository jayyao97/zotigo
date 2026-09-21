package channels

import (
	"context"
	"github.com/jayyao97/zotigo/core/protocol"
	"testing"
	"time"
)

func TestSessionToolAccessRequiresCurrentBotOwner(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	connection, err := store.PutConnection(ctx, Connection{ID: "bot", Provider: ProviderFeishu, AppID: "test", Enabled: true, OwnerSenderIDs: []string{"owner"}})
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := store.EnsureConversation(ctx, "bot", "chat", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	conversation.Enabled = true
	conversation.SessionID = "session"
	conversation.WorkspaceID = "workspace"
	conversation.SenderPolicy = SenderPolicyAll
	conversation, err = store.PutConversation(ctx, conversation)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, nil, nil, nil)
	origin := &protocol.RequestContext{Source: ProviderFeishu, ConnectionID: "bot", ConversationID: conversation.ID, ExternalConversation: "chat", Actor: protocol.RequestActor{ID: "member", Role: "owner"}}
	called := false
	run := func() error { called = true; return nil }
	if err := service.WithSessionToolAccess(ctx, "session", origin, "workspace", true, run); err == nil || called {
		t.Fatal("self-reported owner bypassed authorization")
	}
	if err := service.WithSessionToolAccess(ctx, "session", origin, "workspace", false, run); err != nil || !called {
		t.Fatalf("member read err=%v", err)
	}
	origin.Actor.ID = "owner"
	called = false
	if err := service.WithSessionToolAccess(ctx, "session", origin, "workspace", true, run); err != nil || !called {
		t.Fatalf("owner write err=%v", err)
	}
	connection.OwnerSenderIDs = nil
	if _, err := store.PutConnection(ctx, connection); err != nil {
		t.Fatal(err)
	}
	called = false
	if err := service.WithSessionToolAccess(ctx, "session", origin, "workspace", true, run); err == nil || called {
		t.Fatal("revoked owner authorized")
	}
	if err := service.WithSessionToolAccess(ctx, "session", origin, "other-workspace", false, run); err == nil {
		t.Fatal("workspace escape authorized")
	}
}
