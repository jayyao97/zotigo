package channels

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"testing"
	"time"
)

func TestOmittedChannelPolicyDoesNotChangePermissions(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	secrets, err := NewSecretStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{&fakeAdapter{}}})
	input := ConnectionInput{Provider: ProviderFeishu, Name: "bot", AppID: "app"}
	connection, err := service.PutConnection(ctx, "bot", input)
	if err != nil || connection.ApprovalPolicy != "auto" {
		t.Fatalf("default=%+v err=%v", connection, err)
	}
	full := "bypass_permissions"
	input.ApprovalPolicy = &full
	connection, err = service.PutConnection(ctx, "bot", input)
	if err != nil {
		t.Fatal(err)
	}
	var legacy ConnectionInput
	if err := json.Unmarshal([]byte(`{"provider":"feishu","name":"renamed","app_id":"app"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	connection, err = service.PutConnection(ctx, "bot", legacy)
	if err != nil || connection.ApprovalPolicy != full {
		t.Fatalf("legacy connection update=%+v err=%v", connection, err)
	}
	group, err := store.EnsureConversation(ctx, "bot", "chat", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if group.ApprovalPolicy != "" {
		t.Fatalf("new group policy=%q", group.ApprovalPolicy)
	}
	auto := "auto"
	group, err = service.PutConversation(ctx, group.ID, ConversationInput{ApprovalPolicy: &auto})
	if err != nil {
		t.Fatal(err)
	}
	var legacyGroup ConversationInput
	if err := json.Unmarshal([]byte(`{"display_name":"renamed group"}`), &legacyGroup); err != nil {
		t.Fatal(err)
	}
	group, err = service.PutConversation(ctx, group.ID, legacyGroup)
	if err != nil || resolvePromptSnapshot(connection, group).ApprovalPolicy != "auto" {
		t.Fatalf("legacy group update escalated permissions: %+v err=%v", group, err)
	}
	inherit := ""
	group, err = service.PutConversation(ctx, group.ID, ConversationInput{ApprovalPolicy: &inherit})
	if err != nil || resolvePromptSnapshot(connection, group).ApprovalPolicy != full {
		t.Fatalf("explicit inheritance=%+v err=%v", group, err)
	}
	input.ApprovalPolicy = &auto
	connection, err = service.PutConnection(ctx, "bot", input)
	if err != nil || connection.ApprovalPolicy != auto {
		t.Fatalf("explicit Auto=%+v err=%v", connection, err)
	}
}

func TestChannelApprovalPolicyInheritanceAndPersistence(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	connection, err := store.PutConnection(ctx, Connection{ID: "bot", Provider: ProviderFeishu, ApprovalPolicy: "bypass_permissions"})
	if err != nil {
		t.Fatal(err)
	}
	group, err := store.EnsureConversationRoot(ctx, connection.ID, "chat", "", "", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := resolvePromptSnapshot(connection, group).ApprovalPolicy; got != "bypass_permissions" {
		t.Fatalf("inherited=%q", got)
	}
	group.ApprovalPolicy = "auto"
	group, err = store.PutConversation(ctx, group)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolvePromptSnapshot(connection, group).ApprovalPolicy; got != "auto" {
		t.Fatalf("override=%q", got)
	}
	group.ApprovalPolicy = ""
	if got := resolvePromptSnapshot(Connection{}, group).ApprovalPolicy; got != "auto" {
		t.Fatalf("legacy default=%q", got)
	}
	for _, invalid := range []string{"full_access", "manual", "never"} {
		if _, err := NormalizeApprovalPolicy(invalid); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}

func TestChannelApprovalPolicyMigratesVersionOne(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.PutConnection(context.Background(), Connection{ID: "legacy", Provider: ProviderFeishu}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec(`ALTER TABLE channel_connections DROP COLUMN approval_policy;
 ALTER TABLE channel_conversations DROP COLUMN approval_policy; PRAGMA user_version = 1;`); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	connection, err := store.GetConnection(context.Background(), "legacy")
	if err != nil || connection.ApprovalPolicy != "auto" {
		t.Fatalf("migrated=%+v err=%v", connection, err)
	}
}
