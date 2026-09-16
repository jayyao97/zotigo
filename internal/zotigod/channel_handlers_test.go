package zotigod

import (
	"bytes"
	"context"
	"encoding/base64"
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
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
	"github.com/jayyao97/zotigo/internal/channels"
	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

func TestChannelMessageImagesUseSessionImageValidation(t *testing.T) {
	data, err := base64.StdEncoding.DecodeString(tinyPNGBase64())
	if err != nil {
		t.Fatal(err)
	}
	images, err := channelMessageImages([]channels.InboundImage{{Data: data}})
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 || images[0].MimeType != "image/png" || images[0].Width == 0 || images[0].Height == 0 || !bytes.Equal(images[0].Data, data) {
		t.Fatalf("images=%+v", images)
	}
	if _, err := channelMessageImages([]channels.InboundImage{{Data: []byte("not an image")}}); err == nil {
		t.Fatal("expected invalid channel image to be rejected")
	}
}

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

func (groupChannelAdapter) ListGroupMembers(context.Context, string) ([]channels.Sender, error) {
	return []channels.Sender{{ID: "ou_owner", DisplayName: "Owner"}, {ID: "ou_member", DisplayName: "Member"}}, nil
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
	err := handler.ProvisionChannelSession(context.Background(), workspace.ID, "Review README", prompt, channels.SessionRuntimeConfig{Agent: channels.SessionAgentZotigo}, func(id string, _ channels.SessionRuntimeConfig) error {
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

func TestProvisionChannelSessionPersistsSelectedRuntime(t *testing.T) {
	handler, store, _, workspace := newChannelProvisionFixture(t)
	handler.runtimes = newRuntimeRegistry(nativeRuntimeAdapter{}, &fakeCodexRuntime{})
	var resolved channels.SessionRuntimeConfig
	err := handler.ProvisionChannelSession(context.Background(), workspace.ID, "Codex topic", channels.SessionPromptConfig{}, channels.SessionRuntimeConfig{
		Agent: channels.SessionAgentCodex, Model: "gpt-5.6-luna", ReasoningEffort: "medium",
	}, func(_ string, runtime channels.SessionRuntimeConfig) error {
		resolved = runtime
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Agent != channels.SessionAgentCodex || resolved.Model != "gpt-5.6-luna" || resolved.ReasoningEffort != "medium" {
		t.Fatalf("resolved runtime=%+v", resolved)
	}
	var created *zotigosession.Session
	for _, candidate := range handler.registry.List() {
		if candidate.Agent == channels.SessionAgentCodex {
			stored, loadErr := store.Get(context.Background(), candidate.ID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			created = stored
		}
	}
	if created == nil || created.Agent != channels.SessionAgentCodex || created.Model != "gpt-5.6-luna" || created.ReasoningEffort != "medium" || created.PromptConfig.Revision != 1 {
		t.Fatalf("created session=%+v", created)
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
			err := handler.ProvisionChannelSession(ctx, workspace.ID, "Failed session", channels.SessionPromptConfig{}, channels.SessionRuntimeConfig{Agent: channels.SessionAgentZotigo}, func(id string, _ channels.SessionRuntimeConfig) error {
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

func TestEnsureChannelSessionPromptInitializesOnce(t *testing.T) {
	handler, store, _, _ := newChannelProvisionFixture(t)
	session := newSession(t.TempDir(), "default")
	if err := handler.persistSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	prompt := channels.SessionPromptConfig{AgentInstructions: "channel agent", ApprovalInstructions: "channel approval", ReviewAllTools: true}
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
	if err != nil || canonical.AgentInstructions != "channel agent" {
		t.Fatalf("initialized snapshot was not retained: prompt=%+v err=%v", canonical, err)
	}
}

func TestEnsureChannelSessionPromptSupportsCodex(t *testing.T) {
	handler, store, _, _ := newChannelProvisionFixture(t)
	session := newSession(t.TempDir(), "")
	session.Agent = channels.SessionAgentCodex
	session.Model = "gpt-5.6-luna"
	session.ReasoningEffort = "medium"
	if err := handler.persistSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	prompt := channels.SessionPromptConfig{AgentInstructions: "channel context"}
	canonical, err := handler.EnsureChannelSessionPrompt(context.Background(), session.ID, prompt)
	if err != nil || canonical != prompt {
		t.Fatalf("canonical=%+v err=%v", canonical, err)
	}
	stored, err := store.Get(context.Background(), session.ID)
	if err != nil || stored == nil || stored.PromptConfig.AgentInstructions != "channel context" || stored.PromptConfig.Revision != 1 {
		t.Fatalf("stored=%+v err=%v", stored, err)
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

func TestChannelGroupMembersAPIListsProviderMembers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	channelStore, _ := channels.Open(t.TempDir())
	defer channelStore.Close()
	secretStore, _ := channels.NewSecretStore(t.TempDir())
	service := channels.NewService(channelStore, secretStore, log.New(io.Discard, "", 0), map[string]channels.AdapterFactory{channels.ProviderFeishu: groupChannelFactory{}})
	secret := "secret"
	connection, err := service.PutConnection(ctx, "connection-1", channels.ConnectionInput{Provider: channels.ProviderFeishu, Name: "Test", AppID: "cli_test", AppSecret: &secret, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}, handlerOptions{channels: service})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/channels/connections/"+connection.ID+"/groups/chat-shadow/members", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "ou_owner") || !strings.Contains(recorder.Body.String(), "Member") {
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
	body := fmt.Sprintf(`{"display_name":"Shadow Test","session_id":"must-be-cleared","workspace_id":%q,"agent":"zotigo","profile_name":"channel-default","enabled":true,"allowed_sender_ids":["owner-1"],"agent_instructions_mode":"inherit","approval_instructions_mode":"inherit"}`, workspace.ID)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/channels/conversations/"+conversation.ID, strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	stored, err := channelStore.GetConversation(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SessionID != "" || stored.WorkspaceID != workspace.ID || stored.Agent != channels.SessionAgentZotigo || stored.ProfileName != "channel-default" || !stored.Enabled {
		t.Fatalf("stored=%+v", stored)
	}
	badBody := fmt.Sprintf(`{"display_name":"Shadow Test","workspace_id":%q,"agent":"zotigo","profile_name":"missing","enabled":true,"allowed_sender_ids":["owner-1"],"agent_instructions_mode":"inherit","approval_instructions_mode":"inherit"}`, workspace.ID)
	badRecorder := httptest.NewRecorder()
	handler.ServeHTTP(badRecorder, httptest.NewRequest(http.MethodPut, "/channels/conversations/"+conversation.ID, strings.NewReader(badBody)))
	if badRecorder.Code != http.StatusBadRequest || !strings.Contains(badRecorder.Body.String(), "missing") {
		t.Fatalf("invalid profile status=%d body=%s", badRecorder.Code, badRecorder.Body.String())
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

func TestChannelBindingAllowsIdleRunningSessionWithHistory(t *testing.T) {
	ctx := context.Background()
	sessionStore, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session := &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "session-private", Agent: "zotigo", ApprovalPolicy: agent.ApprovalPolicyAuto, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		PromptConfig: zotigosession.PromptConfig{AgentInstructions: "session instructions", Revision: 3},
	}}
	if err := sessionStore.Put(ctx, session); err != nil {
		t.Fatal(err)
	}
	channelStore, _ := channels.Open(t.TempDir())
	secretStore, _ := channels.NewSecretStore(t.TempDir())
	if _, err := channelStore.PutConnection(ctx, channels.Connection{ID: "connection-1", Provider: channels.ProviderFeishu, Name: "test", AppID: "app", AgentInstructions: "group instructions"}); err != nil {
		t.Fatal(err)
	}
	conversation, err := channelStore.EnsureConversationRoot(ctx, "connection-1", "allowed", "root-private", "", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	items := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{"session-private": {
		{ID: "private-message", Type: zotigosession.DisplayItemUserMessage},
		{ID: "private-turn", Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
		{ID: "private-answer", Type: zotigosession.DisplayItemAssistantMessage, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
		{ID: "private-complete", Type: zotigosession.DisplayItemTurnCompleted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
	}}}
	service := channels.NewService(channelStore, secretStore, log.New(io.Discard, "", 0), map[string]channels.AdapterFactory{channels.ProviderFeishu: noopChannelFactory{}})
	registry := newSessionRegistry()
	registry.Add(Session{ID: session.ID, State: SessionStateRunning, Agent: "zotigo", Working: false})
	handler := newHandler(registry, items, handlerOptions{store: sessionStore, channels: service})
	body := `{"display_name":"Shadow Test","session_id":"session-private","workspace_id":"","enabled":true,"allowed_sender_ids":["user-1"],"agent_instructions_mode":"inherit","agent_instructions":"","approval_instructions_mode":"inherit","approval_instructions":""}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/channels/conversations/"+conversation.ID, strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"agent_instructions":"session instructions"`) {
		t.Fatalf("binding did not retain the Session prompt: %s", recorder.Body.String())
	}

	body = `{"display_name":"Shadow Test","session_id":"session-private","workspace_id":"","enabled":true,"allowed_sender_ids":["user-1"],"agent_instructions_mode":"replace","agent_instructions":"session instructions","approval_instructions_mode":"replace","approval_instructions":"","review_all_tools":false}`
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/channels/conversations/"+conversation.ID, strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("updating the existing binding with history failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestChannelBindingRejectsSessionWithOpenTurn(t *testing.T) {
	ctx := context.Background()
	sessionStore, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessionStore.Close() })
	session := &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "session-active", Agent: "zotigo", ApprovalPolicy: agent.ApprovalPolicyAuto, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}}
	if err := sessionStore.Put(ctx, session); err != nil {
		t.Fatal(err)
	}
	registry := newSessionRegistry()
	registry.Add(Session{ID: session.ID, State: SessionStateRunning, Agent: "zotigo"})
	items := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{session.ID: {
		{ID: "active-turn", Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
	}}}
	daemonHandler := &handler{registry: registry, items: items, store: sessionStore}
	_, err = daemonHandler.validateChannelSessionBinding(ctx, session.ID, channels.SessionRuntimeConfig{Agent: channels.SessionAgentZotigo})
	if err == nil || !strings.Contains(err.Error(), "idle session") {
		t.Fatalf("error=%v", err)
	}
}

func TestChannelBindingRejectsSessionWithPendingMessage(t *testing.T) {
	ctx := context.Background()
	sessionStore, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessionStore.Close() })
	session := &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "session-pending", Agent: "zotigo", ApprovalPolicy: agent.ApprovalPolicyAuto, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}}
	if err := sessionStore.Put(ctx, session); err != nil {
		t.Fatal(err)
	}
	registry := newSessionRegistry()
	registry.Add(Session{ID: session.ID, State: SessionStateRunning, Agent: "zotigo"})
	items := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{session.ID: {
		{ID: "pending-message", Type: zotigosession.DisplayItemUserMessage, Command: &zotigosession.DisplayCommand{Type: sessionCommandMessage, Text: "not started yet"}},
	}}}
	daemonHandler := &handler{registry: registry, items: items, store: sessionStore}
	_, err = daemonHandler.validateChannelSessionBinding(ctx, session.ID, channels.SessionRuntimeConfig{Agent: channels.SessionAgentZotigo})
	if err == nil || !strings.Contains(err.Error(), "idle session") {
		t.Fatalf("error=%v", err)
	}
}

func TestChannelBindingPreparesUnstartedCodexSessionTools(t *testing.T) {
	ctx := context.Background()
	sessionStore, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessionStore.Close() })
	session := &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "codex-unstarted", Agent: "codex", Model: "gpt-5.6-sol", ReasoningEffort: "medium",
		ApprovalPolicy: agent.ApprovalPolicyAuto, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}}
	if err := sessionStore.Put(ctx, session); err != nil {
		t.Fatal(err)
	}
	handler := &handler{registry: newSessionRegistry(), items: &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{session.ID: {}}}, store: sessionStore}
	if _, err := handler.validateChannelSessionBinding(ctx, session.ID, channels.SessionRuntimeConfig{Agent: "codex", Model: session.Model, ReasoningEffort: session.ReasoningEffort}); err != nil {
		t.Fatal(err)
	}
	stored, err := sessionStore.Get(ctx, session.ID)
	if err != nil || stored.Capabilities.ChannelToolsVersion != channels.RuntimeToolsVersion {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
}

func TestChannelBindingPreparesExistingZotigoSessionTools(t *testing.T) {
	ctx := context.Background()
	sessionStore, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessionStore.Close() })
	session := &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "zotigo-existing", Agent: "zotigo", ProfileName: "default", ConversationID: "provider-session",
		ApprovalPolicy: agent.ApprovalPolicyAuto, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}}
	if err := sessionStore.Put(ctx, session); err != nil {
		t.Fatal(err)
	}
	handler := &handler{registry: newSessionRegistry(), items: &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{session.ID: {}}}, store: sessionStore}
	if _, err := handler.validateChannelSessionBinding(ctx, session.ID, channels.SessionRuntimeConfig{Agent: "zotigo", ProfileName: "default"}); err != nil {
		t.Fatal(err)
	}
	stored, err := sessionStore.Get(ctx, session.ID)
	if err != nil || stored.Capabilities.ChannelToolsVersion != channels.RuntimeToolsVersion {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
}

func TestChannelBindingRestartsIdleWorkerOutsideDisconnectLock(t *testing.T) {
	ctx := context.Background()
	sessionStore, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessionStore.Close() })
	session := &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "zotigo-idle-worker", Agent: "zotigo", ProfileName: "default",
		ApprovalPolicy: agent.ApprovalPolicyAuto, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}}
	if err := sessionStore.Put(ctx, session); err != nil {
		t.Fatal(err)
	}
	operations := newSessionOperationLocks()
	workers := newWorkerRegistry()
	worker := newWorkerConnection(session.ID, "generation-1", nil, workers)
	workers.mu.Lock()
	workers.workers[session.ID] = worker
	workers.mu.Unlock()
	disconnected := make(chan struct{})
	workers.SetDisconnectHandler(func(sessionID, generation string) {
		unlock := operations.lock(sessionID)
		defer unlock()
		close(disconnected)
	})
	handler := &handler{
		registry: newSessionRegistry(), items: &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{session.ID: {}}},
		store: sessionStore, workers: workers,
	}
	done := make(chan error, 1)
	go func() {
		unlock := operations.lock(session.ID)
		_, validateErr := handler.validateChannelSessionBinding(ctx, session.ID, channels.SessionRuntimeConfig{Agent: "zotigo", ProfileName: "default"})
		unlock()
		done <- validateErr
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("binding deadlocked while the disconnect callback waited for the Session operation lock")
	}
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("worker disconnect callback did not complete")
	}
}

func TestChannelBindingRejectsStartedCodexSessionWithoutTools(t *testing.T) {
	ctx := context.Background()
	sessionStore, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessionStore.Close() })
	session := &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "codex-started", Agent: "codex", Model: "gpt-5.6-sol", ReasoningEffort: "medium", ConversationID: "thread-existing",
		ApprovalPolicy: agent.ApprovalPolicyAuto, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}}
	if err := sessionStore.Put(ctx, session); err != nil {
		t.Fatal(err)
	}
	handler := &handler{registry: newSessionRegistry(), items: &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{session.ID: {}}}, store: sessionStore}
	_, err = handler.validateChannelSessionBinding(ctx, session.ID, channels.SessionRuntimeConfig{Agent: "codex", Model: session.Model, ReasoningEffort: session.ReasoningEffort})
	if !errors.Is(err, errSessionMissingChannelTools) {
		t.Fatalf("error = %v", err)
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
		ProfileName:    "channel-default",
		ApprovalPolicy: agent.ApprovalPolicyAuto,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
		PromptConfig: zotigosession.PromptConfig{
			AgentInstructions:    "session agent",
			ApprovalInstructions: "session approval",
			ReviewAllTools:       true,
			Revision:             4,
		},
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
		!strings.Contains(recorder.Body.String(), `"agent_instructions":"session agent"`) ||
		!strings.Contains(recorder.Body.String(), `"approval_instructions":"session approval"`) ||
		!strings.Contains(recorder.Body.String(), `"profile_name":"channel-default"`) {
		t.Fatalf("binding response does not expose the prompt snapshot: %s", recorder.Body.String())
	}
	storedConversation, err := channelStore.GetConversation(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedConversation.AgentInstructionsMode != channels.OverrideReplace || storedConversation.AgentInstructions != "session agent" ||
		storedConversation.ApprovalInstructionsMode != channels.OverrideReplace || storedConversation.ApprovalInstructions != "session approval" ||
		storedConversation.ReviewAllTools == nil || !*storedConversation.ReviewAllTools {
		t.Fatalf("conversation prompt snapshot=%+v", storedConversation)
	}
	if storedConversation.Agent != channels.SessionAgentZotigo || storedConversation.ProfileName != "channel-default" {
		t.Fatalf("conversation runtime snapshot=%+v", storedConversation)
	}
	storedSession, err := sessionStore.Get(ctx, session.ID)
	if err != nil || storedSession == nil {
		t.Fatalf("session=%+v err=%v", storedSession, err)
	}
	if storedSession.PromptConfig.AgentInstructions != "session agent" || storedSession.PromptConfig.ApprovalInstructions != "session approval" ||
		!storedSession.PromptConfig.ReviewAllTools || storedSession.PromptConfig.Revision != 4 {
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
	if storedConversation.AgentInstructions != "session agent" || storedConversation.ApprovalInstructions != "session approval" {
		t.Fatalf("rejected update changed prompt snapshot: %+v", storedConversation)
	}
}

func TestChannelBoundSessionRejectsRuntimeMutationEndpoints(t *testing.T) {
	ctx := context.Background()
	sessionStore, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessionStore.Close() })
	now := time.Now()
	for _, session := range []*zotigosession.Session{
		{Metadata: zotigosession.Metadata{ID: "channel-zotigo", Agent: channels.SessionAgentZotigo, ProfileName: "old", ApprovalPolicy: agent.ApprovalPolicyAuto, CreatedAt: now, UpdatedAt: now}},
		{Metadata: zotigosession.Metadata{ID: "channel-codex", Agent: channels.SessionAgentCodex, Model: "gpt-old", ReasoningEffort: "medium", ApprovalPolicy: agent.ApprovalPolicyAuto, CreatedAt: now, UpdatedAt: now}},
	} {
		if err := sessionStore.Put(ctx, session); err != nil {
			t.Fatal(err)
		}
	}
	channelStore, err := channels.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = channelStore.Close() })
	if _, err := channelStore.PutConnection(ctx, channels.Connection{ID: "connection-1", Provider: channels.ProviderFeishu, Name: "test", AppID: "app"}); err != nil {
		t.Fatal(err)
	}
	for index, sessionID := range []string{"channel-zotigo", "channel-codex"} {
		conversation, err := channelStore.EnsureConversationRoot(ctx, "connection-1", "chat-1", fmt.Sprintf("root-%d", index), "", "group", now)
		if err != nil {
			t.Fatal(err)
		}
		conversation.SessionID = sessionID
		conversation.Enabled = true
		if _, err := channelStore.PutConversation(ctx, conversation); err != nil {
			t.Fatal(err)
		}
	}
	secretStore, err := channels.NewSecretStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := channels.NewService(channelStore, secretStore, log.New(io.Discard, "", 0), map[string]channels.AdapterFactory{channels.ProviderFeishu: noopChannelFactory{}})
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}, handlerOptions{store: sessionStore, channels: service})

	tests := []struct {
		path string
		body string
	}{
		{"/sessions/channel-zotigo/profile", `{"profile":"new"}`},
		{"/sessions/channel-codex/codex-settings", `{"model":"gpt-new","reasoning_effort":"high"}`},
	}
	for _, test := range tests {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, test.path, strings.NewReader(test.body)))
		if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "channel-bound session runtime cannot be changed") {
			t.Fatalf("%s status=%d body=%s", test.path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestManualCodexChannelBindingDoesNotExposeStoredProfileSentinel(t *testing.T) {
	ctx := context.Background()
	sessionStore, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessionStore.Close() })
	now := time.Now()
	session := &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "manual-codex", Agent: channels.SessionAgentCodex, ProfileName: "__zotigo_backend__:codex",
		Model: "gpt-channel", ReasoningEffort: "high", ApprovalPolicy: agent.ApprovalPolicyAuto, CreatedAt: now, UpdatedAt: now,
	}}
	if err := sessionStore.Put(ctx, session); err != nil {
		t.Fatal(err)
	}
	channelStore, err := channels.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = channelStore.Close() })
	if _, err := channelStore.PutConnection(ctx, channels.Connection{ID: "connection-1", Provider: channels.ProviderFeishu, Name: "test", AppID: "app"}); err != nil {
		t.Fatal(err)
	}
	conversation, err := channelStore.EnsureConversationRoot(ctx, "connection-1", "chat-1", "root-codex", "", "group", now)
	if err != nil {
		t.Fatal(err)
	}
	secretStore, err := channels.NewSecretStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := channels.NewService(channelStore, secretStore, log.New(io.Discard, "", 0), map[string]channels.AdapterFactory{channels.ProviderFeishu: noopChannelFactory{}})
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{session.ID: {}}}, handlerOptions{store: sessionStore, channels: service})
	body := `{"display_name":"Codex root","session_id":"manual-codex","agent":"codex","profile_name":"","model":"gpt-channel","reasoning_effort":"high","enabled":true,"allowed_sender_ids":["owner-1"]}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/channels/conversations/"+conversation.ID, strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "__zotigo_backend__") {
		t.Fatalf("binding response leaked internal profile sentinel: %s", recorder.Body.String())
	}
	stored, err := channelStore.GetConversation(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Agent != channels.SessionAgentCodex || stored.ProfileName != "" || stored.Model != "gpt-channel" || stored.ReasoningEffort != "high" {
		t.Fatalf("conversation runtime snapshot=%+v", stored)
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
	usage := protocol.Usage{InputTokens: 12, OutputTokens: 3, TotalTokens: 15}
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{"session-1": {{Sequence: 2, Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "private", Runtime: &zotigosession.DisplayRuntime{Agent: "zotigo", Model: "wrong"}}}, {Sequence: 3, Type: zotigosession.DisplayItemTurnCompleted, Turn: &zotigosession.DisplayTurn{ID: "private", LastAgentMessage: "private answer"}}, {Sequence: 5, Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "channel", Runtime: &zotigosession.DisplayRuntime{Agent: "codex", Model: "gpt-channel", ReasoningEffort: "high"}}}, {Sequence: 6, Type: zotigosession.DisplayItemAssistantMessage, Turn: &zotigosession.DisplayTurn{ID: "channel"}, Content: []zotigosession.DisplayContentPart{{Type: "text", Text: "channel answer"}}}, {Sequence: 7, Type: zotigosession.DisplayItemTurnCompleted, Turn: &zotigosession.DisplayTurn{ID: "channel", DurationMS: 2500, Usage: &usage}}}}}
	handler := &handler{items: source}
	turnID := ""
	approvalReported := false
	projected := uint64(4)
	result, done, err := handler.channelTaskResult(context.Background(), "session-1", 4, &projected, &turnID, &approvalReported, channels.RuntimeAttribution{}, func(channels.Progress) {})
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if result.Text != "channel answer" || turnID != "channel" {
		t.Fatalf("turn=%q result=%+v", turnID, result)
	}
	if result.Runtime.Agent != "codex" || result.Runtime.Model != "gpt-channel" || result.Runtime.ReasoningEffort != "high" {
		t.Fatalf("runtime=%+v", result.Runtime)
	}
	if result.DurationMS != 2500 || result.Usage == nil || result.Usage.TotalTokens != 15 {
		t.Fatalf("metrics=%+v", result)
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

func TestProjectChannelItemNamesMessageHistoryTool(t *testing.T) {
	item := zotigosession.DisplayItem{
		ID:   "internal-tool-start",
		Type: zotigosession.DisplayItemToolExecutionStarted,
		ToolExecution: &zotigosession.DisplayToolExecution{
			TurnID: "turn-1", ToolCallID: "exec-1", ToolName: "read_messages",
		},
	}
	events := projectChannelItem(item, "turn-1")
	if len(events) != 1 || events[0].Tool == nil || events[0].Tool.CallID != "exec-1" || events[0].Tool.Kind != "read_messages" || events[0].Tool.DisplayName != "Read group messages" {
		t.Fatalf("events=%+v", events)
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
