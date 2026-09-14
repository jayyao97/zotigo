package zotigod

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
	"github.com/jayyao97/zotigo/internal/channels"
	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

type noopChannelFactory struct{}

func (noopChannelFactory) Capabilities() channels.AdapterCapabilities {
	return channels.AdapterCapabilities{ProgressModes: []string{channels.ProgressModeInteractiveCard}, DefaultProgressMode: channels.ProgressModeInteractiveCard}
}

func (noopChannelFactory) New(channels.Connection, string, channels.AdapterCallbacks) (channels.Adapter, error) {
	return noopChannelAdapter{}, nil
}

type groupChannelFactory struct{}

func (groupChannelFactory) Capabilities() channels.AdapterCapabilities {
	return channels.AdapterCapabilities{ProgressModes: []string{channels.ProgressModeInteractiveCard}, DefaultProgressMode: channels.ProgressModeInteractiveCard}
}

func (groupChannelFactory) New(channels.Connection, string, channels.AdapterCallbacks) (channels.Adapter, error) {
	return groupChannelAdapter{}, nil
}

type groupChannelAdapter struct{ noopChannelAdapter }

func (groupChannelAdapter) ListGroups(context.Context) ([]channels.Group, error) {
	return []channels.Group{{ChatID: "chat-shadow", Name: "Shadow Test", Available: true}}, nil
}

type noopChannelAdapter struct{}

func (noopChannelAdapter) Start(context.Context) error { return nil }
func (noopChannelAdapter) Stop(context.Context) error  { return nil }
func (noopChannelAdapter) OpenProgress(context.Context, channels.InboundMessage) (channels.ProgressHandle, error) {
	return noopProgress{}, nil
}
func (noopChannelAdapter) ResumeProgress(channels.DeliveryReceipt) channels.ProgressHandle {
	return noopProgress{}
}

type noopProgress struct{}

func (noopProgress) Update(context.Context, channels.Progress) error { return nil }
func (noopProgress) Complete(context.Context, channels.TaskResult, func(string) error) error {
	return nil
}
func (noopProgress) Fail(context.Context, channels.Progress) error { return nil }
func (noopProgress) Recover(context.Context, bool) error           { return nil }
func (noopProgress) Receipt() channels.DeliveryReceipt {
	return channels.DeliveryReceipt{Mode: channels.ProgressModeInteractiveCard, MessageID: "reply"}
}

func TestProvisionChannelSessionCommitsBindingBeforePublishingSession(t *testing.T) {
	handler, store, catalog, workspace := newChannelProvisionFixture(t)
	var sessionID string
	prompt := channels.SessionPromptConfig{AgentInstructions: "channel agent", ApprovalInstructions: "channel approval", ReviewAllTools: true}
	err := handler.ProvisionChannelSession(context.Background(), workspace.ID, "Review README", prompt, func(id string) error {
		sessionID = id
		if _, visible := handler.registry.Get(id); visible {
			t.Fatal("session was published before its channel binding committed")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, visible := handler.registry.Get(sessionID); !visible {
		t.Fatal("committed session is absent from registry")
	}
	if stored, err := store.Get(context.Background(), sessionID); err != nil || stored == nil {
		t.Fatalf("stored session=%+v err=%v", stored, err)
	} else if stored.PromptConfig.AgentInstructions != prompt.AgentInstructions || stored.PromptConfig.ApprovalInstructions != prompt.ApprovalInstructions || !stored.PromptConfig.ReviewAllTools || stored.PromptConfig.Revision != 1 {
		t.Fatalf("stored prompt snapshot=%+v", stored.PromptConfig)
	}
	organization, err := catalog.GetSessionOrganization(context.Background(), sessionID)
	if err != nil || organization.WorkspaceID == nil || *organization.WorkspaceID != workspace.ID || organization.Title == nil || *organization.Title != "Review README" {
		t.Fatalf("organization=%+v err=%v", organization, err)
	}
}

func TestProvisionChannelSessionRollsBackFailedOrCanceledBinding(t *testing.T) {
	for _, test := range []struct {
		name string
		bind func(context.CancelFunc) func(string) error
	}{
		{name: "binding failure", bind: func(context.CancelFunc) func(string) error {
			return func(string) error { return errors.New("binding failed") }
		}},
		{name: "context canceled", bind: func(cancel context.CancelFunc) func(string) error {
			return func(string) error { cancel(); return context.Canceled }
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, store, catalog, workspace := newChannelProvisionFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var sessionID string
			err := handler.ProvisionChannelSession(ctx, workspace.ID, "Failed session", channels.SessionPromptConfig{}, func(id string) error {
				sessionID = id
				return test.bind(cancel)(id)
			})
			if err == nil {
				t.Fatal("provisioning unexpectedly succeeded")
			}
			if stored, getErr := store.Get(context.Background(), sessionID); getErr != nil || stored != nil {
				t.Fatalf("orphan stored session=%+v err=%v", stored, getErr)
			}
			if _, getErr := catalog.GetSessionOrganization(context.Background(), sessionID); !errors.Is(getErr, zotigoworkspace.ErrNotFound) {
				t.Fatalf("orphan organization err=%v", getErr)
			}
			if _, visible := handler.registry.Get(sessionID); visible {
				t.Fatal("rolled-back session remains in registry")
			}
		})
	}
}

func TestEnsureLegacyChannelSessionPromptInitializesOnce(t *testing.T) {
	handler, store, _, _ := newChannelProvisionFixture(t)
	session := newSession(t.TempDir(), "default")
	if err := handler.persistSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	prompt := channels.SessionPromptConfig{AgentInstructions: "legacy agent", ApprovalInstructions: "legacy approval", ReviewAllTools: true}
	if _, err := handler.EnsureChannelSessionPrompt(context.Background(), session.ID, prompt); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(context.Background(), session.ID)
	if err != nil || stored == nil {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	if stored.PromptConfig.Revision != 1 || stored.PromptConfig.AgentInstructions != prompt.AgentInstructions || stored.PromptConfig.ApprovalInstructions != prompt.ApprovalInstructions || !stored.PromptConfig.ReviewAllTools {
		t.Fatalf("prompt snapshot=%+v", stored.PromptConfig)
	}
	if _, err := handler.EnsureChannelSessionPrompt(context.Background(), session.ID, prompt); err != nil {
		t.Fatalf("matching snapshot was not idempotent: %v", err)
	}
	prompt.AgentInstructions = "changed"
	canonical, err := handler.EnsureChannelSessionPrompt(context.Background(), session.ID, prompt)
	if err != nil || canonical.AgentInstructions != "legacy agent" {
		t.Fatalf("initialized snapshot was not retained: prompt=%+v err=%v", canonical, err)
	}
}

func newChannelProvisionFixture(t *testing.T) (*handler, zotigosession.Store, *zotigoworkspace.Store, zotigoworkspace.Workspace) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	catalog, err := zotigoworkspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	project, err := catalog.CreateProject(context.Background(), "Channels")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := catalog.CreateWorkspacePlan(context.Background(), project.ID, "Shadow Test", nil)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = catalog.ProvisionWorkspace(context.Background(), workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	projectConfig := "default_profile: channel-default\nprofiles:\n  channel-default:\n    provider: openai\n    model: gpt-default\n"
	if err := os.WriteFile(filepath.Join(workspace.RootPath, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatal(err)
	}
	registry := newSessionRegistry()
	handler := &handler{registry: registry, store: store, catalog: catalog, sessionOps: newSessionOperationLocks(), workspaceOps: newSessionOperationLocks()}
	return handler, store, catalog, workspace
}

func TestChannelConnectionAPIKeepsSecretsOutOfResponses(t *testing.T) {
	channelStore, err := channels.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secretStore, err := channels.NewSecretStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := channels.NewService(channelStore, secretStore, log.New(io.Discard, "", 0), map[string]channels.AdapterFactory{channels.ProviderFeishu: noopChannelFactory{}})
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}, handlerOptions{channels: service})
	body := `{"provider":"feishu","name":"Test","app_id":"cli_test","app_secret":"super-secret","enabled":false,"allow_chat_ids":["oc_allowed"],"agent_instructions":"","approval_instructions":"review","review_all_tools":true}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/channels/connections", strings.NewReader(body)))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if bytes.Contains(recorder.Body.Bytes(), []byte("super-secret")) {
		t.Fatal("secret leaked in API response")
	}
	var created channels.Connection
	if err := decodeAPIData(t, recorder.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !created.HasSecret || created.ID == "" {
		t.Fatalf("created=%+v", created)
	}
	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/channels/connections", nil))
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("status=%d", listRecorder.Code)
	}
	if bytes.Contains(listRecorder.Body.Bytes(), []byte("super-secret")) {
		t.Fatal("secret leaked in list response")
	}
}

func TestEnabledChannelAllowsGroupDiscoveryWithoutLegacyAllowlist(t *testing.T) {
	channelStore, err := channels.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secretStore, _ := channels.NewSecretStore(t.TempDir())
	service := channels.NewService(channelStore, secretStore, log.New(io.Discard, "", 0), map[string]channels.AdapterFactory{channels.ProviderFeishu: noopChannelFactory{}})
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}, handlerOptions{channels: service})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/channels/connections", strings.NewReader(`{"provider":"feishu","name":"Test","app_id":"cli_test","app_secret":"secret","enabled":true,"allow_chat_ids":[],"agent_instructions":"","approval_instructions":"","review_all_tools":true}`)))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestChannelGroupsAPIListsBotGroups(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	channelStore, err := channels.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer channelStore.Close()
	secretStore, err := channels.NewSecretStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := channels.NewService(channelStore, secretStore, log.New(io.Discard, "", 0), map[string]channels.AdapterFactory{channels.ProviderFeishu: groupChannelFactory{}})
	secret := "secret"
	owners := []string{"owner-1"}
	connection, err := service.PutConnection(ctx, "connection-1", channels.ConnectionInput{Provider: channels.ProviderFeishu, Name: "Test", AppID: "cli_test", AppSecret: &secret, Enabled: true, OwnerSenderIDs: &owners})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}, handlerOptions{channels: service})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/channels/connections/"+connection.ID+"/groups", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "Shadow Test") || !strings.Contains(recorder.Body.String(), "chat-shadow") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestChannelGroupBindingValidatesWorkspaceAndClearsSession(t *testing.T) {
	provisionHandler, sessionStore, catalog, workspace := newChannelProvisionFixture(t)
	ctx := context.Background()
	channelStore, err := channels.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer channelStore.Close()
	if _, err := channelStore.PutConnection(ctx, channels.Connection{ID: "connection-1", Provider: channels.ProviderFeishu, Name: "test", AppID: "app"}); err != nil {
		t.Fatal(err)
	}
	conversation, err := channelStore.EnsureConversation(ctx, "connection-1", "chat-shadow", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	secretStore, err := channels.NewSecretStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := channels.NewService(channelStore, secretStore, log.New(io.Discard, "", 0), map[string]channels.AdapterFactory{channels.ProviderFeishu: noopChannelFactory{}})
	handler := newHandler(provisionHandler.registry, &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}, handlerOptions{store: sessionStore, catalog: catalog, channels: service})
	body := fmt.Sprintf(`{"display_name":"Shadow Test","session_id":"must-be-cleared","workspace_id":%q,"enabled":true,"allowed_sender_ids":["owner-1"],"agent_instructions_mode":"inherit","approval_instructions_mode":"inherit"}`, workspace.ID)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/channels/conversations/"+conversation.ID, strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	stored, err := channelStore.GetConversation(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SessionID != "" || stored.WorkspaceID != workspace.ID || !stored.Enabled {
		t.Fatalf("stored=%+v", stored)
	}
}

func TestChannelBindingRejectsBypassSession(t *testing.T) {
	ctx := context.Background()
	sessionStore, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session := &zotigosession.Session{Metadata: zotigosession.Metadata{ID: "session-bypass", Agent: "zotigo", ApprovalPolicy: agent.ApprovalPolicyBypass, CreatedAt: time.Now(), UpdatedAt: time.Now()}}
	if err := sessionStore.Put(ctx, session); err != nil {
		t.Fatal(err)
	}
	channelStore, _ := channels.Open(t.TempDir())
	secretStore, _ := channels.NewSecretStore(t.TempDir())
	_, err = channelStore.PutConnection(ctx, channels.Connection{ID: "connection-1", Provider: channels.ProviderFeishu, Name: "test", AppID: "app"})
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := channelStore.EnsureConversationRoot(ctx, "connection-1", "allowed", "root-bypass", "", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	service := channels.NewService(channelStore, secretStore, log.New(io.Discard, "", 0), map[string]channels.AdapterFactory{channels.ProviderFeishu: noopChannelFactory{}})
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: sessionStore}, handlerOptions{store: sessionStore, channels: service})
	body := `{"display_name":"Shadow Test","session_id":"session-bypass","workspace_id":"","enabled":true,"allowed_sender_ids":["user-1"],"agent_instructions_mode":"inherit","agent_instructions":"","approval_instructions_mode":"inherit","approval_instructions":""}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/channels/conversations/"+conversation.ID, strings.NewReader(body)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestChannelBindingRejectsSessionWithHistory(t *testing.T) {
	ctx := context.Background()
	sessionStore, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session := &zotigosession.Session{Metadata: zotigosession.Metadata{ID: "session-private", Agent: "zotigo", ApprovalPolicy: agent.ApprovalPolicyAuto, CreatedAt: time.Now(), UpdatedAt: time.Now()}}
	if err := sessionStore.Put(ctx, session); err != nil {
		t.Fatal(err)
	}
	channelStore, _ := channels.Open(t.TempDir())
	secretStore, _ := channels.NewSecretStore(t.TempDir())
	if _, err := channelStore.PutConnection(ctx, channels.Connection{ID: "connection-1", Provider: channels.ProviderFeishu, Name: "test", AppID: "app"}); err != nil {
		t.Fatal(err)
	}
	conversation, err := channelStore.EnsureConversationRoot(ctx, "connection-1", "allowed", "root-private", "", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	items := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{"session-private": {{ID: "private-message", Type: zotigosession.DisplayItemUserMessage}}}}
	service := channels.NewService(channelStore, secretStore, log.New(io.Discard, "", 0), map[string]channels.AdapterFactory{channels.ProviderFeishu: noopChannelFactory{}})
	handler := newHandler(newSessionRegistry(), items, handlerOptions{store: sessionStore, channels: service})
	body := `{"display_name":"Shadow Test","session_id":"session-private","workspace_id":"","enabled":true,"allowed_sender_ids":["user-1"],"agent_instructions_mode":"inherit","agent_instructions":"","approval_instructions_mode":"inherit","approval_instructions":""}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/channels/conversations/"+conversation.ID, strings.NewReader(body)))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "no existing history") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	conversation.SessionID = session.ID
	conversation.Enabled = true
	conversation.AllowedSenderIDs = []string{"user-1"}
	if _, err := channelStore.PutConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/channels/conversations/"+conversation.ID, strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("updating the existing binding with history failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestChannelBindingPersistsImmutablePromptSnapshot(t *testing.T) {
	ctx := context.Background()
	sessionStore, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessionStore.Close() })
	session := &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID:             "session-snapshot",
		Agent:          "zotigo",
		ApprovalPolicy: agent.ApprovalPolicyAuto,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}}
	if err := sessionStore.Put(ctx, session); err != nil {
		t.Fatal(err)
	}

	channelStore, err := channels.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = channelStore.Close() })
	connection := channels.Connection{
		ID:                   "connection-1",
		Provider:             channels.ProviderFeishu,
		Name:                 "test",
		AppID:                "app",
		AgentInstructions:    "agent A",
		ApprovalInstructions: "approval A",
		ReviewAllTools:       true,
	}
	if _, err := channelStore.PutConnection(ctx, connection); err != nil {
		t.Fatal(err)
	}
	conversation, err := channelStore.EnsureConversationRoot(ctx, connection.ID, "chat-1", "root-1", "", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	secretStore, err := channels.NewSecretStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := channels.NewService(channelStore, secretStore, log.New(io.Discard, "", 0), map[string]channels.AdapterFactory{channels.ProviderFeishu: noopChannelFactory{}})
	items := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{session.ID: {}}}
	handler := newHandler(newSessionRegistry(), items, handlerOptions{store: sessionStore, channels: service})
	body := `{"display_name":"Root","session_id":"session-snapshot","enabled":true,"allowed_sender_ids":["owner-1"],"agent_instructions_mode":"inherit","approval_instructions_mode":"inherit"}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/channels/conversations/"+conversation.ID, strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"agent_instructions_mode":"replace"`) ||
		!strings.Contains(recorder.Body.String(), `"agent_instructions":"agent A"`) ||
		!strings.Contains(recorder.Body.String(), `"approval_instructions":"approval A"`) {
		t.Fatalf("binding response does not expose the prompt snapshot: %s", recorder.Body.String())
	}
	storedConversation, err := channelStore.GetConversation(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedConversation.AgentInstructionsMode != channels.OverrideReplace || storedConversation.AgentInstructions != "agent A" ||
		storedConversation.ApprovalInstructionsMode != channels.OverrideReplace || storedConversation.ApprovalInstructions != "approval A" ||
		storedConversation.ReviewAllTools == nil || !*storedConversation.ReviewAllTools {
		t.Fatalf("conversation prompt snapshot=%+v", storedConversation)
	}
	storedSession, err := sessionStore.Get(ctx, session.ID)
	if err != nil || storedSession == nil {
		t.Fatalf("session=%+v err=%v", storedSession, err)
	}
	if storedSession.PromptConfig.AgentInstructions != "agent A" || storedSession.PromptConfig.ApprovalInstructions != "approval A" ||
		!storedSession.PromptConfig.ReviewAllTools || storedSession.PromptConfig.Revision != 1 {
		t.Fatalf("session prompt snapshot=%+v", storedSession.PromptConfig)
	}

	connection.AgentInstructions = "agent B"
	connection.ApprovalInstructions = "approval B"
	if _, err := channelStore.PutConnection(ctx, connection); err != nil {
		t.Fatal(err)
	}
	body = `{"display_name":"Root","session_id":"session-snapshot","enabled":true,"allowed_sender_ids":["owner-1"],"agent_instructions_mode":"replace","agent_instructions":"agent B","approval_instructions_mode":"replace","approval_instructions":"approval B","review_all_tools":true}`
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/channels/conversations/"+conversation.ID, strings.NewReader(body)))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "prompt snapshot cannot be changed") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	storedConversation, err = channelStore.GetConversation(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedConversation.AgentInstructions != "agent A" || storedConversation.ApprovalInstructions != "approval A" {
		t.Fatalf("rejected update changed prompt snapshot: %+v", storedConversation)
	}
}

func TestLegacyChannelBindingUsesSessionSnapshotBeforeAPIUpdate(t *testing.T) {
	ctx := context.Background()
	sessionStore, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessionStore.Close() })
	session := &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:             "legacy-session",
			Agent:          "zotigo",
			ApprovalPolicy: agent.ApprovalPolicyAuto,
			CreatedAt:      time.Now(),
			UpdatedAt:      time.Now(),
			PromptConfig: zotigosession.PromptConfig{
				AgentInstructions:    "agent A",
				ApprovalInstructions: "approval A",
				ReviewAllTools:       true,
				Revision:             1,
			},
		},
	}
	if err := sessionStore.Put(ctx, session); err != nil {
		t.Fatal(err)
	}

	channelStore, err := channels.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = channelStore.Close() })
	connection := channels.Connection{
		ID:                   "connection-1",
		Provider:             channels.ProviderFeishu,
		Name:                 "test",
		AppID:                "app",
		AgentInstructions:    "agent B",
		ApprovalInstructions: "approval B",
		ReviewAllTools:       true,
	}
	if _, err := channelStore.PutConnection(ctx, connection); err != nil {
		t.Fatal(err)
	}
	conversation, err := channelStore.EnsureConversationRoot(ctx, connection.ID, "chat-1", "legacy-root", "", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	conversation.SessionID = session.ID
	conversation.Enabled = true
	conversation.AllowedSenderIDs = []string{"owner-1"}
	if _, err := channelStore.PutConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	secretStore, err := channels.NewSecretStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := channels.NewService(channelStore, secretStore, log.New(io.Discard, "", 0), map[string]channels.AdapterFactory{channels.ProviderFeishu: noopChannelFactory{}})
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{session.ID: {}}}, handlerOptions{store: sessionStore, channels: service})
	body := `{"display_name":"Legacy root","session_id":"legacy-session","enabled":true,"allowed_sender_ids":["owner-1"],"agent_instructions_mode":"replace","agent_instructions":"agent B","approval_instructions_mode":"replace","approval_instructions":"approval B","review_all_tools":true}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/channels/conversations/"+conversation.ID, strings.NewReader(body)))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "prompt snapshot cannot be changed") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	stored, err := channelStore.GetConversation(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AgentInstructionsMode != channels.OverrideReplace || stored.AgentInstructions != "agent A" ||
		stored.ApprovalInstructionsMode != channels.OverrideReplace || stored.ApprovalInstructions != "approval A" ||
		stored.ReviewAllTools == nil || !*stored.ReviewAllTools {
		t.Fatalf("legacy conversation was not migrated to the Session snapshot: %+v", stored)
	}
}

func TestDuplicateChannelBindingRejectsBeforeSessionValidation(t *testing.T) {
	ctx := context.Background()
	sessionStore, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session := &zotigosession.Session{Metadata: zotigosession.Metadata{ID: "session-bound", Agent: "zotigo", ApprovalPolicy: agent.ApprovalPolicyAuto, CreatedAt: time.Now(), UpdatedAt: time.Now()}}
	if err := sessionStore.Put(ctx, session); err != nil {
		t.Fatal(err)
	}
	channelStore, _ := channels.Open(t.TempDir())
	secretStore, _ := channels.NewSecretStore(t.TempDir())
	if _, err := channelStore.PutConnection(ctx, channels.Connection{ID: "connection-1", Provider: channels.ProviderFeishu, Name: "test", AppID: "app"}); err != nil {
		t.Fatal(err)
	}
	first, _ := channelStore.EnsureConversationRoot(ctx, "connection-1", "chat-1", "root-1", "", "group", time.Now())
	second, _ := channelStore.EnsureConversationRoot(ctx, "connection-1", "chat-2", "root-2", "", "group", time.Now())
	first.SessionID = session.ID
	first.Enabled = true
	first.AllowedSenderIDs = []string{"user-1"}
	if _, err := channelStore.PutConversation(ctx, first); err != nil {
		t.Fatal(err)
	}
	items := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{session.ID: {}}}
	service := channels.NewService(channelStore, secretStore, log.New(io.Discard, "", 0), map[string]channels.AdapterFactory{channels.ProviderFeishu: noopChannelFactory{}})
	handler := newHandler(newSessionRegistry(), items, handlerOptions{store: sessionStore, channels: service})
	body := `{"display_name":"Second","session_id":"session-bound","enabled":true,"allowed_sender_ids":["user-2"],"agent_instructions_mode":"inherit","approval_instructions_mode":"inherit"}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/channels/conversations/"+second.ID, strings.NewReader(body)))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "already bound") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if calls := items.loadCalls.Load(); calls != 0 {
		t.Fatalf("duplicate binding reached session validation %d times", calls)
	}
}

func TestChannelTaskResultIgnoresTurnBeforeAcceptedCommand(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{"session-1": {{Sequence: 2, Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "private"}}, {Sequence: 3, Type: zotigosession.DisplayItemTurnCompleted, Turn: &zotigosession.DisplayTurn{ID: "private", LastAgentMessage: "private answer"}}, {Sequence: 5, Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "channel"}}, {Sequence: 6, Type: zotigosession.DisplayItemAssistantMessage, Turn: &zotigosession.DisplayTurn{ID: "channel"}, Content: []zotigosession.DisplayContentPart{{Type: "text", Text: "channel answer"}}}, {Sequence: 7, Type: zotigosession.DisplayItemTurnCompleted, Turn: &zotigosession.DisplayTurn{ID: "channel"}}}}}
	handler := &handler{items: source}
	turnID := ""
	approvalReported := false
	projected := uint64(4)
	result, done, err := handler.channelTaskResult(context.Background(), "session-1", 4, &projected, &turnID, &approvalReported, func(channels.Progress) {})
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if result.Text != "channel answer" || turnID != "channel" {
		t.Fatalf("turn=%q result=%+v", turnID, result)
	}
}

func TestProjectChannelItemExposesOnlySafeToolMetadata(t *testing.T) {
	item := zotigosession.DisplayItem{
		ID:       "item-1",
		Sequence: 8,
		Type:     zotigosession.DisplayItemAssistantMessage,
		Content: []zotigosession.DisplayContentPart{{Type: "tool_result", ToolResult: &zotigosession.DisplayToolResult{
			ToolCallID: "call-1", ToolName: "shell", Text: "secret output", Metadata: map[string]any{"path": "/private/path"},
		}}},
	}
	events := projectChannelItem(item, "turn-1")
	if len(events) != 1 || events[0].Type != channels.ExecutionToolFinished || events[0].Tool == nil {
		t.Fatalf("events=%+v", events)
	}
	if events[0].Tool.CallID != "call-1" || events[0].Tool.Kind != "shell" || events[0].Tool.DisplayName != "Run command" {
		t.Fatalf("tool=%+v", events[0].Tool)
	}
	encoded := fmt.Sprintf("%+v", events)
	if strings.Contains(encoded, "secret output") || strings.Contains(encoded, "/private/path") {
		t.Fatalf("private tool data leaked: %s", encoded)
	}
}

func TestProjectChannelApprovalUsesStableApprovalID(t *testing.T) {
	for _, itemType := range []zotigosession.DisplayItemType{zotigosession.DisplayItemApprovalRequest, zotigosession.DisplayItemApprovalDecision} {
		item := zotigosession.DisplayItem{ID: string(itemType), Type: itemType, Approval: &zotigosession.DisplayApproval{ID: "approval-2", TurnID: "turn-1"}}
		events := projectChannelItem(item, "turn-1")
		if len(events) != 1 || events[0].CorrelationID != "approval-2" {
			t.Fatalf("type=%s events=%+v", itemType, events)
		}
	}
}

func TestChannelStopKeepsAdmissionClosedUntilDurablePauseIsAccepted(t *testing.T) {
	const sessionID = "session-stop"
	appendStarted := make(chan struct{})
	releaseAppend := make(chan struct{})
	source := &fakeDisplayItemSource{
		items: map[string][]zotigosession.DisplayItem{sessionID: {{
			ID: "turn-start", Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
		}}},
		appendHook: func(_ string, item zotigosession.DisplayItem) {
			if item.Command != nil && item.Command.Type == sessionCommandPause {
				close(appendStarted)
				<-releaseAppend
			}
		},
	}
	registry := newSessionRegistry()
	registry.Add(Session{ID: sessionID, State: SessionStateRunning, Agent: string(zotigoruntime.AgentZotigo)})
	workers := newWorkerRegistry()
	worker := newWorkerConnection(sessionID, "generation-1", nil, workers)
	workers.mu.Lock()
	workers.workers[sessionID] = worker
	workers.mu.Unlock()
	handler := &handler{registry: registry, items: source, workers: workers, inputStopTimeout: time.Second, sessionOps: newSessionOperationLocks()}

	admitted := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := handler.StopChannelTask(context.Background(), channels.Task{SessionID: sessionID, MessageID: "message-stop"}, func(channels.Progress) {
			select {
			case <-admitted:
			default:
				t.Error("progress was emitted before stop admission completed")
			}
		}, func() { close(admitted) })
		done <- err
	}()
	<-appendStarted
	select {
	case <-admitted:
		t.Fatal("stop released channel admission before the durable pause boundary")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseAppend)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-admitted:
	default:
		t.Fatal("stop did not complete admission")
	}
	select {
	case message := <-worker.sendCh:
		if message.Command == nil || message.Command.Type != sessionCommandPause || message.Command.Pause == nil || message.Command.Pause.TurnID != "turn-1" {
			t.Fatalf("worker message=%+v", message)
		}
	default:
		t.Fatal("durable pause command was not sent to the worker")
	}
}
