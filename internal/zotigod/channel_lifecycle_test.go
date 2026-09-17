package zotigod

import (
	"context"
	"database/sql"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/channels"
)

func TestArchiveUnbindsChannelSessionAndClearsPromptOnNextTurn(t *testing.T) {
	for _, root := range []string{"", "topic-root"} {
		t.Run("root="+root, func(t *testing.T) {
			ctx := context.Background()
			h, sessions, catalog, workspace := newChannelProvisionFixture(t)
			session := newSession(workspace.RootPath, "channel-default")
			if err := h.persistSession(ctx, session); err != nil {
				t.Fatal(err)
			}
			if _, err := catalog.AssignSession(ctx, session.ID, workspace.ID); err != nil {
				t.Fatal(err)
			}
			stored, err := sessions.Get(ctx, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			stored.PromptConfig = zotigosession.PromptConfig{AgentInstructions: "channel-only", ApprovalInstructions: "review", ReviewAllTools: true, Revision: 3}
			if err = sessions.Put(ctx, stored); err != nil {
				t.Fatal(err)
			}
			h.items.(*fakeDisplayItemSource).items[session.ID] = nil
			channelRoot := t.TempDir()
			store, err := channels.Open(channelRoot)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			if _, err = store.PutConnection(ctx, channels.Connection{ID: "bot", Provider: channels.ProviderFeishu}); err != nil {
				t.Fatal(err)
			}
			conversation, err := store.EnsureConversationRoot(ctx, "bot", "chat", root, "", "group", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			conversation.SessionID, conversation.WorkspaceID, conversation.Enabled = session.ID, workspace.ID, true
			if root == "" {
				conversation.SessionStrategy = channels.SessionStrategyShared
			}
			if _, err = store.PutConversation(ctx, conversation); err != nil {
				t.Fatal(err)
			}
			secrets, err := channels.NewSecretStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			h.channels = channels.NewService(store, secrets, log.New(io.Discard, "", 0), nil)
			deleteResponse := httptest.NewRecorder()
			h.handleChannelConnection(deleteResponse, httptest.NewRequest(http.MethodDelete, "/channels/connections/bot", nil), "bot")
			if deleteResponse.Code != http.StatusConflict {
				t.Fatalf("delete bound connection %d: %s", deleteResponse.Code, deleteResponse.Body.String())
			}
			h.items.(*fakeDisplayItemSource).items[session.ID] = []zotigosession.DisplayItem{{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "active"}}}
			blocked := httptest.NewRecorder()
			h.handleSessionOrganizationArchive(blocked, httptest.NewRequest(http.MethodPost, "/sessions/"+session.ID+"/archive", nil), session.ID, true)
			if blocked.Code != http.StatusConflict {
				t.Fatalf("archive active: %d %s", blocked.Code, blocked.Body.String())
			}
			if binding, err := store.GetConversationBySession(ctx, session.ID); err != nil || binding.ID != conversation.ID {
				t.Fatalf("active binding lost: %+v %v", binding, err)
			}
			h.items.(*fakeDisplayItemSource).items[session.ID] = nil
			// Fail the actual Channel write after the catalog archive succeeds.
			faultDB, err := sql.Open("sqlite", filepath.Join(channelRoot, "channels.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = faultDB.Close() })
			if _, err = faultDB.Exec(`CREATE TRIGGER fail_unbind BEFORE UPDATE OF session_id ON channel_conversations WHEN NEW.session_id = '' BEGIN SELECT RAISE(ABORT, 'unbind write failed'); END`); err != nil {
				t.Fatal(err)
			}
			failed := httptest.NewRecorder()
			h.handleSessionOrganizationArchive(failed, httptest.NewRequest(http.MethodPost, "/sessions/"+session.ID+"/archive", nil), session.ID, true)
			if failed.Code != http.StatusInternalServerError {
				t.Fatalf("failed unbind: %d %s", failed.Code, failed.Body.String())
			}
			previous, err := catalog.GetSessionOrganization(ctx, session.ID)
			if err != nil || previous.SelfArchivedAt != nil {
				t.Fatalf("archive rollback failed: %+v %v", previous, err)
			}
			if _, err = store.GetConversationBySession(ctx, session.ID); err != nil {
				t.Fatalf("failed unbind lost binding: %v", err)
			}
			if _, err = faultDB.Exec(`DROP TRIGGER fail_unbind`); err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			h.handleSessionOrganizationArchive(response, httptest.NewRequest(http.MethodPost, "/sessions/"+session.ID+"/archive", nil), session.ID, true)
			if response.Code != http.StatusOK {
				t.Fatalf("archive %d: %s", response.Code, response.Body.String())
			}
			conversation, err = store.GetConversation(ctx, conversation.ID)
			if err != nil || conversation.SessionID != "" || conversation.Enabled != (root == "") {
				t.Fatalf("binding %+v: %v", conversation, err)
			}
			organization, err := catalog.GetSessionOrganization(ctx, session.ID)
			if err != nil || organization.SelfArchivedAt == nil {
				t.Fatalf("archive %+v: %v", organization, err)
			}
			if _, err = catalog.SetSessionArchived(ctx, session.ID, false); err != nil {
				t.Fatal(err)
			}
			admitted := 0
			if err = h.withRefreshedBoundChannelPrompt(ctx, session.ID, func(steeringOnly bool) error {
				if steeringOnly {
					t.Fatal("idle unbound session admitted as steering")
				}
				admitted++
				stored, err := sessions.Get(ctx, session.ID)
				if err != nil || stored.PromptConfig != (zotigosession.PromptConfig{}) {
					t.Fatalf("prompt not cleared before admission: %+v %v", stored, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if admitted != 1 {
				t.Fatalf("admitted %d times", admitted)
			}
			if err = h.channels.DeleteConnection(ctx, "bot"); err != nil {
				t.Fatal(err)
			}
			if stored, err = sessions.Get(ctx, session.ID); err != nil || stored == nil {
				t.Fatalf("session history lost: %v", err)
			}
		})
	}
}

func TestResumeUnboundCodexClearsChannelOverrides(t *testing.T) {
	rpc := &codexWorkerRPC{}
	err := resumeCodexThread(context.Background(), rpc, codexWorkerConfig{
		ThreadID: "thread", WorkingDirectory: t.TempDir(), Model: "model", ChannelToolsVersion: channels.RuntimeToolsVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := rpc.requests["thread/resume"]
	if value, present := request["developerInstructions"]; !present || value != "" {
		t.Fatalf("developer instructions not explicitly cleared: %#v", request)
	}
	if request["approvalsReviewer"] != "user" {
		t.Fatalf("reviewer not cleared: %#v", request)
	}
}
