package channels

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeFactory struct{ adapter *fakeAdapter }

func (fakeFactory) Capabilities() AdapterCapabilities {
	return AdapterCapabilities{ProgressModes: []string{ProgressModeAuto, ProgressModeCOT, ProgressModeInteractiveCard}, DefaultProgressMode: ProgressModeInteractiveCard}
}

func (f fakeFactory) New(_ Connection, _ string, callbacks AdapterCallbacks) (Adapter, error) {
	f.adapter.callbacks = callbacks
	originalInbound := callbacks.Inbound
	f.adapter.callbacks.Inbound = func(ctx context.Context, in InboundMessage) error {
		if in.ConversationKey == "" {
			rootID := in.RootID
			if rootID == "" {
				rootID = in.MessageID
			}
			in.ConversationKey = in.ChatID + "\x00" + rootID
			in.StartsConversation = in.RootID == "" || in.RootID == in.MessageID
			in.TriggerAllowed = !in.StartsConversation || in.MentionedBot
		}
		return originalInbound(ctx, in)
	}
	return f.adapter, nil
}

type credentialFactory struct {
	mu       sync.Mutex
	adapters map[string]*fakeAdapter
}

type recoveryRequiredAdapter struct {
	fakeAdapter
	prepareErr error
	prepared   atomic.Bool
	stops      atomic.Int32
}

func (a *recoveryRequiredAdapter) PrepareRecovery(context.Context) error {
	if a.prepareErr != nil {
		return a.prepareErr
	}
	a.prepared.Store(true)
	return nil
}

func (a *recoveryRequiredAdapter) ResumeProgress(receipt DeliveryReceipt) ProgressHandle {
	a.mu.Lock()
	a.receipts = append(a.receipts, receipt)
	a.mu.Unlock()
	return a
}

func (a *recoveryRequiredAdapter) ClearProcessingMarker(context.Context) error {
	if !a.prepared.Load() {
		return errors.New("bot identity is not ready")
	}
	return nil
}

func (a *recoveryRequiredAdapter) Stop(context.Context) error {
	a.stops.Add(1)
	return nil
}

type recoveryCredentialFactory struct {
	old *recoveryRequiredAdapter
	new *recoveryRequiredAdapter
}

func (*recoveryCredentialFactory) Capabilities() AdapterCapabilities {
	return fakeFactory{}.Capabilities()
}

func (f *recoveryCredentialFactory) New(_ Connection, secret string, callbacks AdapterCallbacks) (Adapter, error) {
	var adapter *recoveryRequiredAdapter
	switch secret {
	case "expired-secret":
		adapter = f.old
	case "working-secret":
		adapter = f.new
	default:
		return nil, fmt.Errorf("unexpected credential %q", secret)
	}
	adapter.callbacks = callbacks
	return adapter, nil
}

func (*credentialFactory) Capabilities() AdapterCapabilities { return fakeFactory{}.Capabilities() }

func (f *credentialFactory) New(_ Connection, secret string, callbacks AdapterCallbacks) (Adapter, error) {
	f.mu.Lock()
	adapter := f.adapters[secret]
	f.mu.Unlock()
	if adapter == nil {
		return nil, fmt.Errorf("unexpected credential %q", secret)
	}
	adapter.callbacks = callbacks
	return adapter, nil
}

type fakeAdapter struct {
	callbacks          AdapterCallbacks
	mu                 sync.Mutex
	opens              int
	opened             []InboundMessage
	updates            []Progress
	receipts           []DeliveryReceipt
	afterOpen          func()
	updateErr          error
	updateFunc         func(context.Context) error
	openErr            error
	processingMarkerID string
	markerClears       int
	clearMarker        func(context.Context) error
	stopFunc           func(context.Context) error
	groups             []Group
	ownerIDs           []string
	groupsErr          error
	listGroupsFunc     func(context.Context) ([]Group, error)
	resolveImagesFunc  func(context.Context, []InboundImage) ([]InboundImage, error)
	resolveImageCalls  int
	resolveReferenced  func(context.Context, string, string) (ReferencedMessage, error)
}

func (f *fakeAdapter) ResolveReferencedMessage(ctx context.Context, chatID, messageID string) (ReferencedMessage, error) {
	if f.resolveReferenced == nil {
		return ReferencedMessage{}, errors.New("referenced message unavailable")
	}
	return f.resolveReferenced(ctx, chatID, messageID)
}

func (f *fakeAdapter) ResolveInboundImages(ctx context.Context, images []InboundImage) ([]InboundImage, error) {
	f.mu.Lock()
	f.resolveImageCalls++
	resolve := f.resolveImagesFunc
	f.mu.Unlock()
	if resolve != nil {
		return resolve(ctx, images)
	}
	return images, nil
}

func (f *fakeAdapter) ListGroups(ctx context.Context) ([]Group, error) {
	if f.listGroupsFunc != nil {
		return f.listGroupsFunc(ctx)
	}
	return append([]Group(nil), f.groups...), f.groupsErr
}

func (f *fakeAdapter) Start(ctx context.Context) error {
	if len(f.ownerIDs) > 0 {
		f.callbacks.Owners(f.ownerIDs)
	}
	f.callbacks.Ready("bot-1", "Test Bot")
	<-ctx.Done()
	return ctx.Err()
}
func (f *fakeAdapter) Stop(ctx context.Context) error {
	if f.stopFunc != nil {
		return f.stopFunc(ctx)
	}
	return nil
}
func (f *fakeAdapter) OpenProgress(_ context.Context, in InboundMessage) (ProgressHandle, error) {
	f.mu.Lock()
	f.opens++
	f.opened = append(f.opened, in)
	f.mu.Unlock()
	if f.afterOpen != nil {
		f.afterOpen()
	}
	if f.openErr != nil {
		return nil, f.openErr
	}
	return f, nil
}
func (f *fakeAdapter) ResumeProgress(receipt DeliveryReceipt) ProgressHandle {
	f.mu.Lock()
	f.receipts = append(f.receipts, receipt)
	f.mu.Unlock()
	return f
}
func (f *fakeAdapter) Receipt() DeliveryReceipt {
	return DeliveryReceipt{Mode: ProgressModeInteractiveCard, MessageID: "reply-1", ProcessingMarkerID: f.processingMarkerID}
}
func (f *fakeAdapter) ClearProcessingMarker(ctx context.Context) error {
	f.mu.Lock()
	clearMarker := f.clearMarker
	if f.processingMarkerID != "" {
		f.markerClears++
	}
	f.mu.Unlock()
	if clearMarker != nil {
		return clearMarker(ctx)
	}
	return nil
}
func (f *fakeAdapter) Update(ctx context.Context, p Progress) error {
	f.mu.Lock()
	f.updates = append(f.updates, p)
	updateFunc := f.updateFunc
	f.mu.Unlock()
	if updateFunc != nil {
		return updateFunc(ctx)
	}
	return f.updateErr
}
func (f *fakeAdapter) Complete(ctx context.Context, result TaskResult, persistFinal func(string) error) error {
	if err := f.Update(ctx, Progress{State: "completed", Text: result.Text}); err != nil {
		return err
	}
	return persistFinal("reply-1")
}
func (f *fakeAdapter) Fail(ctx context.Context, progress Progress) error {
	return f.Update(ctx, progress)
}
func (f *fakeAdapter) Recover(ctx context.Context, completed bool) error {
	state := "failed"
	if completed {
		state = "completed"
	}
	return f.Update(ctx, Progress{State: state})
}

type fakeDispatcher struct{ called chan Task }

func (fakeDispatcher) EnsureChannelSessionPrompt(_ context.Context, _ string, prompt SessionPromptConfig) (SessionPromptConfig, error) {
	return prompt, nil
}

func (f fakeDispatcher) DispatchChannelTask(_ context.Context, task Task, progress func(Progress), admissionComplete func()) (TaskResult, error) {
	admissionComplete()
	f.called <- task
	return TaskResult{Text: "done"}, nil
}

type provisioningDispatcher struct {
	called       chan Task
	workspaceIDs chan string
	prompts      chan SessionPromptConfig
	runtimes     chan SessionRuntimeConfig
	sessionIDs   chan string
}

func (provisioningDispatcher) EnsureChannelSessionPrompt(_ context.Context, _ string, prompt SessionPromptConfig) (SessionPromptConfig, error) {
	return prompt, nil
}

func (f provisioningDispatcher) ProvisionChannelSession(_ context.Context, workspaceID, _ string, prompt SessionPromptConfig, runtime SessionRuntimeConfig, bind func(string, SessionRuntimeConfig) error) error {
	f.workspaceIDs <- workspaceID
	if f.prompts != nil {
		f.prompts <- prompt
	}
	if f.runtimes != nil {
		f.runtimes <- runtime
	}
	sessionID := "session-new"
	if f.sessionIDs != nil {
		sessionID = <-f.sessionIDs
	}
	if runtime.Agent == "" {
		runtime.Agent = SessionAgentZotigo
	}
	return bind(sessionID, runtime)
}

func (f provisioningDispatcher) DispatchChannelTask(_ context.Context, task Task, _ func(Progress), admissionComplete func()) (TaskResult, error) {
	admissionComplete()
	f.called <- task
	return TaskResult{Text: "done"}, nil
}

type controlDispatcher struct {
	tasks chan Task
	stops chan Task
}

func (controlDispatcher) EnsureChannelSessionPrompt(_ context.Context, _ string, prompt SessionPromptConfig) (SessionPromptConfig, error) {
	return prompt, nil
}

func (f controlDispatcher) DispatchChannelTask(_ context.Context, task Task, _ func(Progress), admissionComplete func()) (TaskResult, error) {
	admissionComplete()
	f.tasks <- task
	return TaskResult{Text: "ordinary"}, nil
}

func (f controlDispatcher) StopChannelTask(_ context.Context, task Task, progress func(Progress), admissionComplete func()) (TaskResult, error) {
	admissionComplete()
	progress(Progress{State: "running", Events: []PublicExecutionEvent{{Type: ExecutionRunStarted}}})
	f.stops <- task
	return TaskResult{Text: "stopped"}, nil
}

type progressDispatcher struct{}

func (progressDispatcher) EnsureChannelSessionPrompt(_ context.Context, _ string, prompt SessionPromptConfig) (SessionPromptConfig, error) {
	return prompt, nil
}

func (progressDispatcher) DispatchChannelTask(_ context.Context, _ Task, progress func(Progress), admissionComplete func()) (TaskResult, error) {
	admissionComplete()
	progress(Progress{State: "running", Sequence: 4, Events: []PublicExecutionEvent{{Type: ExecutionRunStarted}}})
	progress(Progress{State: "running", Sequence: 5, Events: []PublicExecutionEvent{{Type: ExecutionToolStarted}}})
	return TaskResult{Text: "done"}, nil
}

type blockingAdmissionDispatcher struct {
	entered chan struct{}
	release chan struct{}
}

func (blockingAdmissionDispatcher) EnsureChannelSessionPrompt(_ context.Context, _ string, prompt SessionPromptConfig) (SessionPromptConfig, error) {
	return prompt, nil
}

type crossConnectionDispatcher struct {
	firstEntered  chan struct{}
	firstRelease  chan struct{}
	secondEntered chan struct{}
}

func (crossConnectionDispatcher) EnsureChannelSessionPrompt(_ context.Context, _ string, prompt SessionPromptConfig) (SessionPromptConfig, error) {
	return prompt, nil
}

func (f crossConnectionDispatcher) DispatchChannelTask(_ context.Context, task Task, _ func(Progress), admissionComplete func()) (TaskResult, error) {
	if task.ConnectionID == "connection-1" {
		close(f.firstEntered)
		<-f.firstRelease
	} else {
		close(f.secondEntered)
	}
	admissionComplete()
	return TaskResult{Text: "done"}, nil
}

type serialAdmissionDispatcher struct {
	mu            sync.Mutex
	count         int
	firstEntered  chan struct{}
	firstRelease  chan struct{}
	secondEntered chan struct{}
}

func (*serialAdmissionDispatcher) EnsureChannelSessionPrompt(_ context.Context, _ string, prompt SessionPromptConfig) (SessionPromptConfig, error) {
	return prompt, nil
}

func (f *serialAdmissionDispatcher) DispatchChannelTask(_ context.Context, _ Task, _ func(Progress), admissionComplete func()) (TaskResult, error) {
	f.mu.Lock()
	f.count++
	count := f.count
	f.mu.Unlock()
	if count == 1 {
		close(f.firstEntered)
		<-f.firstRelease
	} else {
		close(f.secondEntered)
	}
	admissionComplete()
	return TaskResult{Text: "done"}, nil
}

type failingDispatcher struct{ err error }

type cancelAwareDispatcher struct{ entered chan struct{} }

func (cancelAwareDispatcher) EnsureChannelSessionPrompt(_ context.Context, _ string, prompt SessionPromptConfig) (SessionPromptConfig, error) {
	return prompt, nil
}

func (f cancelAwareDispatcher) DispatchChannelTask(ctx context.Context, _ Task, _ func(Progress), admissionComplete func()) (TaskResult, error) {
	admissionComplete()
	close(f.entered)
	<-ctx.Done()
	return TaskResult{}, ctx.Err()
}

func (failingDispatcher) EnsureChannelSessionPrompt(_ context.Context, _ string, prompt SessionPromptConfig) (SessionPromptConfig, error) {
	return prompt, nil
}

type conversationNameResolverFunc func(context.Context, string) (string, error)

func (f conversationNameResolverFunc) ResolveConversationName(ctx context.Context, chatID string) (string, error) {
	return f(ctx, chatID)
}

func bindTestGroup(t *testing.T, store *Store, chatID string, senderIDs ...string) Conversation {
	t.Helper()
	ctx := context.Background()
	group, err := store.EnsureConversation(ctx, "connection-1", chatID, "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	group.WorkspaceID = "workspace-1"
	group.Enabled = true
	group.AllowedSenderIDs = append([]string(nil), senderIDs...)
	group, err = store.PutConversation(ctx, group)
	if err != nil {
		t.Fatal(err)
	}
	return group
}

func (f failingDispatcher) DispatchChannelTask(_ context.Context, _ Task, _ func(Progress), admissionComplete func()) (TaskResult, error) {
	admissionComplete()
	return TaskResult{}, f.err
}

func (f blockingAdmissionDispatcher) DispatchChannelTask(_ context.Context, _ Task, _ func(Progress), admissionComplete func()) (TaskResult, error) {
	close(f.entered)
	<-f.release
	admissionComplete()
	return TaskResult{Text: "done"}, nil
}

func TestServiceDropsUnboundGroupsBeforeStorage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secrets, _ := NewSecretStore(t.TempDir())
	adapter := &fakeAdapter{}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	service.SetDispatcher(fakeDispatcher{called: make(chan Task, 1)})
	secret := "secret"
	owners := []string{"user-1"}
	_, err = service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, AllowChatIDs: []string{"allowed"}, OwnerSenderIDs: &owners})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err = adapter.callbacks.Inbound(ctx, InboundMessage{MessageID: "outside-message", ChatID: "outside", ChatType: "group", Text: "private", Images: []InboundImage{{ProviderKey: "outside"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.GetMessageByProviderID(ctx, "connection-1", "outside-message"); err != ErrNotFound {
		t.Fatalf("outside message was retained: %v", err)
	}
	bindTestGroup(t, store, "allowed", "user-1")
	if err = adapter.callbacks.Inbound(ctx, InboundMessage{MessageID: "unapproved-sender", ChatID: "allowed", ChatType: "group", Sender: Sender{ID: "user-2"}, Text: "private", Images: []InboundImage{{ProviderKey: "unapproved"}}, MentionedBot: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.GetMessageByProviderID(ctx, "connection-1", "unapproved-sender"); err != ErrNotFound {
		t.Fatalf("unapproved sender message was retained: %v", err)
	}
	adapter.mu.Lock()
	resolveCalls := adapter.resolveImageCalls
	adapter.mu.Unlock()
	if resolveCalls != 0 {
		t.Fatalf("unauthorized images were resolved %d times", resolveCalls)
	}
}

func TestServiceDiscoversGroupsAndMergesWorkspaceBinding(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	secrets, err := NewSecretStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	adapter := &fakeAdapter{groups: []Group{{ChatID: "chat-2", ChatMode: ChatModeTopic, Name: "Zeta"}, {ChatID: "chat-1", ChatMode: ChatModeGroup, Name: "Alpha", Avatar: "avatar"}}}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	secret := "secret"
	owners := []string{"owner-1"}
	if _, err = service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "bot", AppID: "app", AppSecret: &secret, Enabled: true, OwnerSenderIDs: &owners}); err != nil {
		t.Fatal(err)
	}
	if err = service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	groups, err := service.ListGroups(ctx, "connection-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || groups[0].ChatID != "chat-1" || groups[0].ChatMode != ChatModeGroup || groups[0].Name != "Alpha" || groups[0].Avatar != "avatar" || !groups[0].Available || groups[0].ConversationID == "" {
		t.Fatalf("groups=%+v", groups)
	}
	conversation, err := store.GetConversation(ctx, groups[0].ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if conversation.ChatMode != ChatModeGroup {
		t.Fatalf("persisted chat mode=%q", conversation.ChatMode)
	}
	conversation.WorkspaceID = "workspace-1"
	conversation.Agent = SessionAgentCodex
	conversation.Model = "gpt-test"
	conversation.ReasoningEffort = "high"
	conversation.Enabled = true
	conversation.AllowedSenderIDs = []string{"owner-1"}
	if _, err = store.PutConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	groups, err = service.ListGroups(ctx, "connection-1")
	if err != nil {
		t.Fatal(err)
	}
	if groups[0].WorkspaceID != "workspace-1" || groups[0].Agent != SessionAgentCodex || groups[0].Model != "gpt-test" || groups[0].ReasoningEffort != "high" || !groups[0].Enabled || !slices.Equal(groups[0].AllowedSenderIDs, []string{"owner-1"}) {
		t.Fatalf("bound group=%+v", groups[0])
	}
}

func TestServiceDiscoversUnnamedGroupWithoutOverwritingKnownName(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	secrets, _ := NewSecretStore(t.TempDir())
	adapter := &fakeAdapter{groups: []Group{{ChatID: "known-chat"}, {ChatID: "unnamed-chat"}}}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	secret := "secret"
	if _, err = service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "bot", AppID: "app", AppSecret: &secret, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err = store.PutGroupConversationName(ctx, "connection-1", "known-chat", "Known name"); err != nil {
		t.Fatal(err)
	}
	if err = service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	groups, err := service.ListGroups(ctx, "connection-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || groups[0].ChatID != "known-chat" || groups[0].Name != "Known name" || groups[1].ChatID != "unnamed-chat" || groups[1].Name != "" {
		t.Fatalf("groups=%+v", groups)
	}
	unnamed, err := store.GetConversationByScope(ctx, "connection-1", "unnamed-chat", "")
	if err != nil || unnamed.ChatName != "" {
		t.Fatalf("unnamed group=%+v err=%v", unnamed, err)
	}
}

func TestServiceRequiresMentionBindingAndAllowedSender(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secrets, _ := NewSecretStore(t.TempDir())
	adapter := &fakeAdapter{resolveImagesFunc: func(_ context.Context, images []InboundImage) ([]InboundImage, error) {
		if len(images) != 1 || images[0].ProviderKey != "provider-image" {
			t.Fatalf("unresolved images=%+v", images)
		}
		return []InboundImage{{Data: []byte("image")}}, nil
	}}
	called := make(chan Task, 1)
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	service.SetDispatcher(fakeDispatcher{called: called})
	secret := "secret"
	owners := []string{"user-1"}
	_, err = service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, AllowChatIDs: []string{"allowed"}, OwnerSenderIDs: &owners})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	bindTestGroup(t, store, "allowed", "user-1")
	if err = adapter.callbacks.Inbound(ctx, InboundMessage{MessageID: "observe", ChatID: "allowed", ChatType: "group", Sender: Sender{ID: "user-1"}, Text: "hello", MentionedBot: true}); err != nil {
		t.Fatal(err)
	}
	conversations, err := service.ListConversations(ctx, "connection-1")
	if err != nil || len(conversations) != 2 {
		t.Fatalf("conversations=%v err=%v", conversations, err)
	}
	message, _ := store.GetMessageByProviderID(ctx, "connection-1", "observe")
	if message.TriggerStatus != "rejected" || message.StatusDetail != "conversation_not_bound" {
		t.Fatalf("unexpected observed message: %+v", message)
	}
	var conversation Conversation
	for _, candidate := range conversations {
		if candidate.RootID == "observe" {
			conversation = candidate
		}
	}
	if conversation.ID == "" {
		t.Fatalf("rooted conversation missing: %+v", conversations)
	}
	_, err = service.PutConversation(ctx, conversation.ID, ConversationInput{DisplayName: "Shadow Test", SessionID: "session-1", Enabled: true, AllowedSenderIDs: []string{"user-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = adapter.callbacks.Inbound(ctx, InboundMessage{MessageID: "run", ParentMessageID: "quoted", ChatID: "allowed", RootID: "observe", ThreadID: "thread-1", ChatType: "group", Sender: Sender{ID: "user-1", DisplayName: "Owner"}, Text: "hello", Images: []InboundImage{{ProviderKey: "provider-image"}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case task := <-called:
		if task.SessionID != "session-1" || task.Origin.ActorRole != "owner" || task.Origin.Sender.ID != "user-1" || task.Origin.Sender.DisplayName != "Owner" || task.Origin.ConversationName != "Shadow Test" || task.Origin.ExternalConversation != "allowed" || task.Origin.ExternalRootID != "observe" || task.Origin.ExternalMessageID != "run" || task.Origin.ExternalParentMessageID != "quoted" || len(task.Images) != 1 || string(task.Images[0].Data) != "image" {
			t.Fatalf("task=%+v", task)
		}
	case <-ctx.Done():
		t.Fatal("task not dispatched")
	}
}

func TestServiceDeliversWithWorkspaceGroupBindingWithoutConnectionAllowlist(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	secrets, _ := NewSecretStore(t.TempDir())
	adapter := &fakeAdapter{}
	called := make(chan Task, 1)
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	service.SetDispatcher(fakeDispatcher{called: called})
	secret := "secret"
	owners := []string{"user-1"}
	connection, err := service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, OwnerSenderIDs: &owners})
	if err != nil {
		t.Fatal(err)
	}
	if len(connection.AllowChatIDs) != 0 {
		t.Fatalf("connection allowlist unexpectedly populated: %v", connection.AllowChatIDs)
	}
	if err = service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	bindTestGroup(t, store, "allowed", "user-1")
	conversation, err := store.EnsureConversationRoot(ctx, "connection-1", "allowed", "topic-root", "thread-1", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.PutConversation(ctx, conversation.ID, ConversationInput{DisplayName: "Topic", SessionID: "session-1", Enabled: true, AllowedSenderIDs: []string{"user-1"}}); err != nil {
		t.Fatal(err)
	}
	if err = adapter.callbacks.Inbound(ctx, InboundMessage{MessageID: "reply", ChatID: "allowed", RootID: "topic-root", ThreadID: "thread-1", ChatType: "group", Sender: Sender{ID: "user-1"}, Text: "continue", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("task was not dispatched")
	}
	deadline := time.Now().Add(time.Second)
	for {
		message, getErr := store.GetMessageByProviderID(ctx, "connection-1", "reply")
		if getErr == nil && message.TriggerStatus == "processed" {
			if message.FinalMessageID != "reply-1" {
				t.Fatalf("final message ID = %q", message.FinalMessageID)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivery did not settle as processed: %+v err=%v", message, getErr)
		}
		time.Sleep(time.Millisecond)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if len(adapter.updates) != 1 || adapter.updates[0].State != "completed" || adapter.updates[0].Text != "done" {
		t.Fatalf("updates=%+v", adapter.updates)
	}
}

func TestServiceRechecksWorkspaceBindingBeforeProvisioning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	secrets, _ := NewSecretStore(t.TempDir())
	adapter := &fakeAdapter{}
	dispatcher := provisioningDispatcher{called: make(chan Task, 1), workspaceIDs: make(chan string, 1)}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	service.SetDispatcher(dispatcher)
	secret := "secret"
	owners := []string{"user-1"}
	if _, err = service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, OwnerSenderIDs: &owners}); err != nil {
		t.Fatal(err)
	}
	if err = service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	group := bindTestGroup(t, store, "allowed", "user-1")
	claimed := make(chan struct{})
	resume := make(chan struct{})
	service.afterMessageClaimed = func() {
		close(claimed)
		<-resume
	}
	inboundDone := make(chan error, 1)
	go func() {
		inboundDone <- adapter.callbacks.Inbound(ctx, InboundMessage{MessageID: "new-topic", ChatID: "allowed", ChatType: "group", Sender: Sender{ID: "user-1"}, Text: "run", MentionedBot: true, CreatedAt: time.Now()})
	}()
	select {
	case <-claimed:
	case <-time.After(time.Second):
		t.Fatal("message was not claimed")
	}
	group.WorkspaceID = "workspace-2"
	if _, err = service.PutConversation(ctx, group.ID, ConversationInput{DisplayName: group.DisplayName, WorkspaceID: group.WorkspaceID, Enabled: true, AllowedSenderIDs: group.AllowedSenderIDs}); err != nil {
		t.Fatal(err)
	}
	close(resume)
	if err = <-inboundDone; err != nil {
		t.Fatal(err)
	}
	select {
	case workspaceID := <-dispatcher.workspaceIDs:
		if workspaceID != "workspace-2" {
			t.Fatalf("session provisioned in stale workspace %q", workspaceID)
		}
	case <-time.After(time.Second):
		t.Fatal("session was not provisioned")
	}
}

func TestServiceRequiresMentionForNewTopLevelSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	secrets, _ := NewSecretStore(t.TempDir())
	adapter := &fakeAdapter{}
	dispatcher := provisioningDispatcher{called: make(chan Task, 1), workspaceIDs: make(chan string, 1)}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	service.SetDispatcher(dispatcher)
	secret := "secret"
	owners := []string{"user-1"}
	if _, err = service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, OwnerSenderIDs: &owners}); err != nil {
		t.Fatal(err)
	}
	if err = service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	bindTestGroup(t, store, "allowed", "user-1")
	if err = adapter.callbacks.Inbound(ctx, InboundMessage{MessageID: "unmentioned", ChatID: "allowed", RootID: "unmentioned", ChatType: "group", Sender: Sender{ID: "user-1"}, Text: "ambient"}); err != nil {
		t.Fatal(err)
	}
	message, err := store.GetMessageByProviderID(ctx, "connection-1", "unmentioned")
	if err != nil {
		t.Fatal(err)
	}
	if message.TriggerStatus != "rejected" || message.StatusDetail != "mention_required" {
		t.Fatalf("message=%+v", message)
	}
	select {
	case <-dispatcher.workspaceIDs:
		t.Fatal("unmentioned top-level message provisioned a session")
	default:
	}
}

func TestServiceDoesNotShareSessionBindingAcrossTopics(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secrets, _ := NewSecretStore(t.TempDir())
	adapter := &fakeAdapter{}
	called := make(chan Task, 1)
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	service.SetDispatcher(fakeDispatcher{called: called})
	secret := "secret"
	owners := []string{"user-1"}
	if _, err = service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, AllowChatIDs: []string{"allowed"}, OwnerSenderIDs: &owners}); err != nil {
		t.Fatal(err)
	}
	if err = service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	bindTestGroup(t, store, "allowed", "user-1")
	first := InboundMessage{MessageID: "topic-a-observe", ChatID: "allowed", ThreadID: "topic-a", ChatType: "group", Sender: Sender{ID: "user-1", DisplayName: "Owner"}, Text: "hello", MentionedBot: true}
	if err = adapter.callbacks.Inbound(ctx, first); err != nil {
		t.Fatal(err)
	}
	conversations, err := service.ListConversations(ctx, "connection-1")
	if err != nil || len(conversations) != 2 {
		t.Fatalf("conversations=%+v err=%v", conversations, err)
	}
	var topicA Conversation
	for _, candidate := range conversations {
		if candidate.RootID == first.MessageID {
			topicA = candidate
		}
	}
	if topicA.ID == "" {
		t.Fatalf("topic A missing: %+v", conversations)
	}
	_, err = service.PutConversation(ctx, topicA.ID, ConversationInput{DisplayName: "Topic A", SessionID: "session-a", Enabled: true, AllowedSenderIDs: []string{"user-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = adapter.callbacks.Inbound(ctx, InboundMessage{MessageID: "topic-b-run", ChatID: "allowed", ThreadID: "topic-b", ChatType: "group", Sender: Sender{ID: "user-1", DisplayName: "Owner"}, Text: "run", MentionedBot: true}); err != nil {
		t.Fatal(err)
	}
	message, err := store.GetMessageByProviderID(ctx, "connection-1", "topic-b-run")
	if err != nil {
		t.Fatal(err)
	}
	if message.TriggerStatus != "rejected" || message.StatusDetail != "conversation_not_bound" {
		t.Fatalf("topic B inherited topic A binding: %+v", message)
	}
	if err = adapter.callbacks.Inbound(ctx, InboundMessage{MessageID: "topic-a-run", ChatID: "allowed", RootID: "topic-a-observe", ThreadID: "topic-a", ChatType: "group", Sender: Sender{ID: "user-1", DisplayName: "Owner"}, Text: "run", MentionedBot: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case task := <-called:
		if task.SessionID != "session-a" || task.Origin.ExternalConversation != "allowed" || task.Origin.ExternalRootID != "topic-a-observe" || task.Origin.ExternalThreadID != "topic-a" {
			t.Fatalf("task=%+v", task)
		}
	case <-time.After(time.Second):
		t.Fatal("topic A was not dispatched")
	}
}

func TestServiceProvisionsDistinctSessionForNewTopLevelConversation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secrets, _ := NewSecretStore(t.TempDir())
	adapter := &fakeAdapter{}
	dispatcher := provisioningDispatcher{called: make(chan Task, 1), workspaceIDs: make(chan string, 1), runtimes: make(chan SessionRuntimeConfig, 1)}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	service.SetDispatcher(dispatcher)
	secret := "secret"
	owners := []string{"user-1"}
	if _, err = service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, AllowChatIDs: []string{"allowed"}, OwnerSenderIDs: &owners}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = store.db.Exec(`INSERT INTO channel_conversations(id,connection_id,chat_id,root_id,chat_type,chat_name,display_name,workspace_id,session_agent,model,reasoning_effort,enabled,allowed_sender_ids,last_activity_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, "group-default", "connection-1", "allowed", "", "group", "Shadow Test", "Shadow Test", "workspace-1", SessionAgentCodex, "gpt-test", "high", true, `["user-1"]`, now, now, now); err != nil {
		t.Fatal(err)
	}
	if err = service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err = adapter.callbacks.Inbound(ctx, InboundMessage{MessageID: "topic-new", ChatID: "allowed", ChatType: "group", ChatName: "Shadow Test", Sender: Sender{ID: "user-1", DisplayName: "Owner"}, Text: "hello", MentionedBot: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case workspaceID := <-dispatcher.workspaceIDs:
		if workspaceID != "workspace-1" {
			t.Fatalf("workspace = %q", workspaceID)
		}
	case <-time.After(time.Second):
		t.Fatal("session was not provisioned")
	}
	if runtime := <-dispatcher.runtimes; runtime.Agent != SessionAgentCodex || runtime.Model != "gpt-test" || runtime.ReasoningEffort != "high" {
		t.Fatalf("runtime=%+v", runtime)
	}
	select {
	case task := <-dispatcher.called:
		if task.SessionID != "session-new" {
			t.Fatalf("task session = %q", task.SessionID)
		}
	case <-time.After(time.Second):
		t.Fatal("task was not dispatched")
	}
	conversation, err := store.GetConversationByScope(ctx, "connection-1", "allowed", "topic-new")
	if err != nil {
		t.Fatal(err)
	}
	if conversation.SessionID != "session-new" || conversation.WorkspaceID != "workspace-1" || conversation.Agent != SessionAgentCodex || conversation.Model != "gpt-test" || conversation.ReasoningEffort != "high" || !conversation.Enabled || conversation.ChatName != "Shadow Test" || !slices.Contains(conversation.AllowedSenderIDs, "user-1") {
		t.Fatalf("conversation = %+v", conversation)
	}
}

func TestServiceSharedGroupUsesOneSessionAndRepliesInMainGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	secrets, _ := NewSecretStore(t.TempDir())
	adapter := &fakeAdapter{}
	dispatcher := provisioningDispatcher{called: make(chan Task, 2), workspaceIDs: make(chan string, 1), sessionIDs: make(chan string, 1)}
	dispatcher.sessionIDs <- "session-shared"
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	service.SetDispatcher(dispatcher)
	secret := "secret"
	owners := []string{"owner-1"}
	if _, err = service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "alias", AppID: "app", AppSecret: &secret, Enabled: true, OwnerSenderIDs: &owners}); err != nil {
		t.Fatal(err)
	}
	group := bindTestGroup(t, store, "group-1", "owner-1")
	group.ChatName = "Actual group"
	group.SessionStrategy = SessionStrategyShared
	if _, err = store.PutConversation(ctx, group); err != nil {
		t.Fatal(err)
	}
	oldTopic, err := store.EnsureConversationRoot(ctx, "connection-1", "group-1", "old-topic", "thread-1", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	oldTopic.SessionID = "session-topic"
	oldTopic.Enabled = true
	if _, err = store.PutConversation(ctx, oldTopic); err != nil {
		t.Fatal(err)
	}
	if err = service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	inputs := []InboundMessage{
		{MessageID: "message-0", ChatID: "group-1", ChatType: "group", ChatName: "Actual group", Sender: Sender{ID: "owner-1", DisplayName: "Owner"}, Text: "first", MentionedBot: true, CreatedAt: time.Now()},
		{MessageID: "message-1", ChatID: "group-1", ChatType: "group", ChatName: "Actual group", RootID: "old-topic", ThreadID: "thread-1", Sender: Sender{ID: "owner-1", DisplayName: "Owner"}, Text: "second", MentionedBot: true, CreatedAt: time.Now()},
	}
	for _, input := range inputs {
		if err = adapter.callbacks.Inbound(ctx, input); err != nil {
			t.Fatal(err)
		}
		var task Task
		select {
		case task = <-dispatcher.called:
		case <-time.After(time.Second):
			message, _ := store.GetMessageByProviderID(ctx, "connection-1", input.MessageID)
			currentGroup, groupErr := store.GetConversationByScope(ctx, "connection-1", "group-1", "")
			t.Fatalf("message was not dispatched: %+v group=%+v err=%v", message, currentGroup, groupErr)
		}
		if task.SessionID != "session-shared" || task.Origin.ChannelID != "bot-1" || task.Origin.ChannelName != "Test Bot" || task.Origin.ExternalConversationName != "Actual group" {
			t.Fatalf("task=%+v", task)
		}
	}
	select {
	case <-dispatcher.workspaceIDs:
	default:
		t.Fatal("shared session was not provisioned")
	}
	shared, err := store.GetConversationByScope(ctx, "connection-1", "group-1", "")
	if err != nil || shared.SessionID != "session-shared" {
		t.Fatalf("shared=%+v err=%v", shared, err)
	}
	preservedTopic, err := store.GetConversationByScope(ctx, "connection-1", "group-1", "old-topic")
	if err != nil || preservedTopic.SessionID != "session-topic" {
		t.Fatalf("preserved topic=%+v err=%v", preservedTopic, err)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if len(adapter.opened) != 2 || adapter.opened[0].ReplyMode != ReplyModeDirect || adapter.opened[1].ReplyMode != ReplyModeDirect {
		t.Fatalf("opened=%+v", adapter.opened)
	}
}

func TestExistingSessionUsesCurrentInheritedPrompt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := Open(t.TempDir())
	defer store.Close()
	secrets, _ := NewSecretStore(t.TempDir())
	adapter := &fakeAdapter{}
	dispatcher := provisioningDispatcher{
		called: make(chan Task, 3), workspaceIDs: make(chan string, 2), prompts: make(chan SessionPromptConfig, 2),
		sessionIDs: make(chan string, 2),
	}
	dispatcher.sessionIDs <- "session-old"
	dispatcher.sessionIDs <- "session-new"
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	service.SetDispatcher(dispatcher)
	secret := "secret"
	owners := []string{"owner-1"}
	input := ConnectionInput{Provider: ProviderFeishu, Name: "bot", AppID: "app", AppSecret: &secret, Enabled: true, OwnerSenderIDs: &owners, AgentInstructions: "connection-old", ApprovalInstructions: "approval-old", ReviewAllTools: true}
	if _, err := service.PutConnection(ctx, "connection-1", input); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	bindTestGroup(t, store, "allowed", "owner-1")
	if err := adapter.callbacks.Inbound(ctx, InboundMessage{MessageID: "old-root", ChatID: "allowed", ChatType: "group", Sender: Sender{ID: "owner-1"}, Text: "first", MentionedBot: true, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	first := <-dispatcher.called
	if first.AgentInstructions != "connection-old" || first.ApprovalInstructions != "approval-old" || !first.ReviewAllTools {
		t.Fatalf("first task prompt=%+v", first)
	}
	if prompt := <-dispatcher.prompts; prompt.AgentInstructions != "connection-old" || prompt.ApprovalInstructions != "approval-old" || !prompt.ReviewAllTools {
		t.Fatalf("first session snapshot=%+v", prompt)
	}
	input.AppSecret = nil
	input.OwnerSenderIDs = nil
	input.AgentInstructions = "connection-new"
	input.ApprovalInstructions = "approval-new"
	if _, err := service.PutConnection(ctx, "connection-1", input); err != nil {
		t.Fatal(err)
	}
	if err := adapter.callbacks.Inbound(ctx, InboundMessage{MessageID: "old-reply", ChatID: "allowed", RootID: "old-root", ChatType: "group", Sender: Sender{ID: "owner-1"}, Text: "again", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	oldReply := <-dispatcher.called
	if oldReply.AgentInstructions != "connection-new" || oldReply.ApprovalInstructions != "approval-new" {
		t.Fatalf("old session prompt was not refreshed=%+v", oldReply)
	}
	if err := adapter.callbacks.Inbound(ctx, InboundMessage{MessageID: "new-root", ChatID: "allowed", ChatType: "group", Sender: Sender{ID: "owner-1"}, Text: "new", MentionedBot: true, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	newTask := <-dispatcher.called
	if newTask.AgentInstructions != "connection-new" || newTask.ApprovalInstructions != "approval-new" {
		t.Fatalf("new session prompt=%+v", newTask)
	}
	if prompt := <-dispatcher.prompts; prompt.AgentInstructions != "connection-new" || prompt.ApprovalInstructions != "approval-new" {
		t.Fatalf("new session snapshot=%+v", prompt)
	}
}

func TestServiceRefreshesOfficialGroupNameAcrossConversations(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	connection := Connection{ID: "connection-1", Provider: ProviderFeishu, Name: "test", AppID: "app", AllowChatIDs: []string{"allowed"}}
	if _, err = store.PutConnection(ctx, connection); err != nil {
		t.Fatal(err)
	}
	group, err := store.EnsureConversation(ctx, connection.ID, "allowed", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	group.DisplayName = "hi"
	if _, err = store.PutConversation(ctx, group); err != nil {
		t.Fatal(err)
	}
	if _, err = store.EnsureConversationRoot(ctx, connection.ID, "allowed", "topic-1", "", "group", time.Now()); err != nil {
		t.Fatal(err)
	}
	service := NewService(store, nil, log.New(io.Discard, "", 0), nil)
	service.runs[connection.ID] = &adapterRun{generation: 1}
	service.refreshConversationNames(ctx, connection, 1, conversationNameResolverFunc(func(_ context.Context, chatID string) (string, error) {
		if chatID != "allowed" {
			t.Fatalf("chat id = %q", chatID)
		}
		return "Shadow Test", nil
	}))
	conversations, err := store.ListConversations(ctx, connection.ID)
	if err != nil || len(conversations) != 2 {
		t.Fatalf("conversations=%+v err=%v", conversations, err)
	}
	for _, conversation := range conversations {
		if conversation.ChatName != "Shadow Test" {
			t.Fatalf("conversation=%+v", conversation)
		}
	}
	refreshedGroup, err := store.GetConversation(ctx, group.ID)
	if err != nil || refreshedGroup.DisplayName != "hi" {
		t.Fatalf("group=%+v err=%v", refreshedGroup, err)
	}
}

func TestConnectionPersistsNormalizedOwnerSenderIDs(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	connection, err := store.PutConnection(context.Background(), Connection{ID: "connection-1", Provider: ProviderFeishu, Name: "test", AppID: "app", OwnerSenderIDs: []string{" owner-1 ", "owner-1", ""}})
	if err != nil {
		t.Fatal(err)
	}
	if len(connection.OwnerSenderIDs) != 1 || connection.OwnerSenderIDs[0] != "owner-1" {
		t.Fatalf("owners=%v", connection.OwnerSenderIDs)
	}
}

func TestOpenInitializesVersionedChannelSchemaAndReopens(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err = store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != channelSchemaVersion {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
	var indexes int
	if err = store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name IN ('channel_session_binding','channel_thread_binding','channel_messages_by_conversation')`).Scan(&indexes); err != nil || indexes != 3 {
		t.Fatalf("schema indexes=%d err=%v", indexes, err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRejectsUnversionedChannelDatabase(t *testing.T) {
	root := t.TempDir()
	database, err := sql.Open("sqlite", filepath.Join(root, "channels.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.Exec(`CREATE TABLE channel_connections (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err = database.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(root)
	if store != nil {
		_ = store.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "unsupported unversioned channels database") {
		t.Fatalf("Open() error=%v", err)
	}
}

func TestStoreScopesConversationsAndSendersByRootMessage(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err = store.PutConnection(ctx, Connection{ID: "connection-1", Provider: ProviderFeishu, Name: "test", AppID: "app"}); err != nil {
		t.Fatal(err)
	}
	for _, message := range []InboundMessage{
		{MessageID: "root-message", ChatID: "chat-1", ChatType: "group", Sender: Sender{ID: "user-1"}},
		{MessageID: "thread-a-root", ChatID: "chat-1", ThreadID: "thread-a", ChatType: "group", Sender: Sender{ID: "user-1"}},
		{MessageID: "thread-b-root", ChatID: "chat-1", ThreadID: "thread-b", ChatType: "group", Sender: Sender{ID: "user-1", DisplayName: "Owner"}},
		{MessageID: "thread-b-2", ChatID: "chat-1", RootID: "thread-b-root", ThreadID: "thread-b", ChatType: "group", Sender: Sender{ID: "user-1", DisplayName: "Owner"}},
	} {
		if _, err = store.RecordMessage(ctx, "connection-1", message); err != nil {
			t.Fatal(err)
		}
	}
	conversations, err := store.ListConversations(ctx, "connection-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 4 {
		t.Fatalf("conversations=%+v", conversations)
	}
	byRoot := make(map[string]Conversation, len(conversations))
	for _, conversation := range conversations {
		byRoot[conversation.RootID] = conversation
	}
	if byRoot["root-message"].ChatID != "chat-1" || byRoot["thread-a-root"].ID == byRoot["thread-b-root"].ID {
		t.Fatalf("conversations were not scoped by root message: %+v", conversations)
	}
	threadB := byRoot["thread-b-root"]
	if len(threadB.ObservedSenders) != 1 || threadB.ObservedSenders[0].DisplayName != "Owner" {
		t.Fatalf("observed senders=%+v", threadB.ObservedSenders)
	}
	messages, err := store.ListMessages(ctx, threadB.ID, 10)
	if err != nil || len(messages) != 2 {
		t.Fatalf("messages=%+v err=%v", messages, err)
	}
}

func TestSenderPolicyAuthorization(t *testing.T) {
	connection := Connection{OwnerSenderIDs: []string{"owner"}}
	for _, test := range []struct {
		name    string
		policy  string
		sender  string
		allowed bool
	}{
		{"owner accepted", SenderPolicyOwners, "owner", true},
		{"member rejected by owner policy", SenderPolicyOwners, "member", false},
		{"member accepted by all policy", SenderPolicyAll, "member", true},
		{"empty sender rejected by all policy", SenderPolicyAll, "", false},
		{"selected accepted", SenderPolicySelected, "member", true},
		{"selected rejected", SenderPolicySelected, "other", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			group := Conversation{SenderPolicy: test.policy, AllowedSenderIDs: []string{"member"}}
			if got := senderAllowed(connection, group, test.sender); got != test.allowed {
				t.Fatalf("senderAllowed()=%v want %v", got, test.allowed)
			}
		})
	}
}

func TestAdapterResolvedOwnerReplacesConfiguredOwner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := Open(t.TempDir())
	defer store.Close()
	secrets, _ := NewSecretStore(t.TempDir())
	adapter := &fakeAdapter{ownerIDs: []string{"provider-owner"}}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	secret := "secret"
	oldOwners := []string{"manual-owner"}
	if _, err := service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, OwnerSenderIDs: &oldOwners}); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		connection, err := service.GetConnection(ctx, "connection-1")
		if err != nil {
			t.Fatal(err)
		}
		if slices.Equal(connection.OwnerSenderIDs, []string{"provider-owner"}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("owner IDs=%v", connection.OwnerSenderIDs)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestConnectionUpdateDistinguishesOmittedAndEmptyOwnerSenderIDs(t *testing.T) {
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
	owners := []string{"owner-1"}
	created, err := service.PutConnection(context.Background(), "connection-1", ConnectionInput{
		Provider: ProviderFeishu, Name: "test", AppID: "app", OwnerSenderIDs: &owners,
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := service.PutConnection(context.Background(), created.ID, ConnectionInput{
		Provider: ProviderFeishu, Name: "renamed", AppID: "app",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.OwnerSenderIDs) != 1 || updated.OwnerSenderIDs[0] != "owner-1" {
		t.Fatalf("omitted owner_sender_ids=%v, want preserved owner", updated.OwnerSenderIDs)
	}
	empty := []string{}
	updated, err = service.PutConnection(context.Background(), created.ID, ConnectionInput{
		Provider: ProviderFeishu, Name: "renamed", AppID: "app", OwnerSenderIDs: &empty,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.OwnerSenderIDs) != 0 {
		t.Fatalf("explicit empty owner_sender_ids=%v, want cleared", updated.OwnerSenderIDs)
	}
}

func TestConversationUpdatePreservesOmittedRuntimeFields(t *testing.T) {
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
	if _, err = store.PutConnection(ctx, Connection{ID: "connection-1", Provider: ProviderFeishu, Name: "test", AppID: "app"}); err != nil {
		t.Fatal(err)
	}
	conversation, err := store.EnsureConversation(ctx, "connection-1", "chat-1", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	agentName, model, effort := SessionAgentCodex, "gpt-test", "high"
	conversation, err = service.PutConversation(ctx, conversation.ID, ConversationInput{Agent: &agentName, Model: &model, ReasoningEffort: &effort})
	if err != nil {
		t.Fatal(err)
	}
	conversation, err = service.PutConversation(ctx, conversation.ID, ConversationInput{DisplayName: "renamed"})
	if err != nil {
		t.Fatal(err)
	}
	if conversation.Agent != agentName || conversation.Model != model || conversation.ReasoningEffort != effort {
		t.Fatalf("runtime was cleared by omitted fields: %+v", conversation)
	}
}

func TestPutConversationCanceledWriteLeavesBindingUnchanged(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err = store.PutConnection(ctx, Connection{ID: "connection-1", Provider: ProviderFeishu, Name: "test", AppID: "app"}); err != nil {
		t.Fatal(err)
	}
	conversation, err := store.EnsureConversationRoot(ctx, "connection-1", "chat-1", "root-1", "thread-1", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	conversation.SessionID = "session-new"
	conversation.WorkspaceID = "workspace-1"
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = store.PutConversation(canceled, conversation); !errors.Is(err, context.Canceled) {
		t.Fatalf("PutConversation error = %v, want context.Canceled", err)
	}
	unchanged, err := store.GetConversation(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.SessionID != "" || unchanged.WorkspaceID != "" {
		t.Fatalf("canceled binding persisted: %+v", unchanged)
	}
}

func TestServiceRoutesExactStopCommandToSessionController(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	adapter := &fakeAdapter{}
	dispatcher := controlDispatcher{tasks: make(chan Task, 1), stops: make(chan Task, 1)}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	service.SetDispatcher(dispatcher)
	secret := "secret"
	if _, err := service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, AllowChatIDs: []string{"allowed"}}); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	bindTestGroup(t, store, "allowed", "user-1")
	conversation, _ := store.EnsureConversationRoot(ctx, "connection-1", "allowed", "stop", "", "group", time.Now())
	if _, err := service.PutConversation(ctx, conversation.ID, ConversationInput{SessionID: "session-1", Enabled: true, AllowedSenderIDs: []string{"user-1"}}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.callbacks.Inbound(ctx, InboundMessage{MessageID: "stop", ChatID: "allowed", ChatType: "group", Sender: Sender{ID: "user-1"}, Text: " /STOP ", MentionedBot: true, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	select {
	case task := <-dispatcher.stops:
		if task.SessionID != "session-1" || task.Text != " /STOP " {
			t.Fatalf("stop task=%+v", task)
		}
	case <-time.After(time.Second):
		t.Fatal("stop was not dispatched")
	}
	select {
	case task := <-dispatcher.tasks:
		t.Fatalf("stop started an ordinary turn: %+v", task)
	case <-time.After(20 * time.Millisecond):
	}
	deadline := time.Now().Add(time.Second)
	for {
		message, err := store.GetMessageByProviderID(ctx, "connection-1", "stop")
		if err == nil && message.TriggerStatus == "processed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stop did not settle: %+v err=%v", message, err)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestConversationSessionBindingIsUnique(t *testing.T) {
	ctx := context.Background()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{&fakeAdapter{}}})
	if _, err := store.PutConnection(ctx, Connection{ID: "connection-1", Provider: ProviderFeishu, Name: "test", AppID: "app"}); err != nil {
		t.Fatal(err)
	}
	first, _ := store.EnsureConversationRoot(ctx, "connection-1", "chat-1", "root-1", "", "group", time.Now())
	second, _ := store.EnsureConversationRoot(ctx, "connection-1", "chat-2", "root-2", "", "group", time.Now())
	input := ConversationInput{SessionID: "session-1", Enabled: true, AllowedSenderIDs: []string{"user-1"}}
	if _, err := service.PutConversation(ctx, first.ID, input); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutConversation(ctx, second.ID, input); err == nil || !strings.Contains(err.Error(), "already bound") {
		t.Fatalf("duplicate binding error=%v", err)
	}
}

func TestTopicGroupRejectsSharedSessionStrategy(t *testing.T) {
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
	if _, err := store.PutConnection(ctx, Connection{ID: "connection-1", Provider: ProviderFeishu, Name: "test", AppID: "app"}); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureGroupConversation(ctx, "connection-1", "topic-chat"); err != nil {
		t.Fatal(err)
	}
	if err := store.PutGroupConversationMode(ctx, "connection-1", "topic-chat", ChatModeTopic); err != nil {
		t.Fatal(err)
	}
	group, err := store.GetConversationByScope(ctx, "connection-1", "topic-chat", "")
	if err != nil {
		t.Fatal(err)
	}
	message, err := store.RecordMessage(ctx, "connection-1", InboundMessage{MessageID: "message-1", ChatID: "topic-chat", ChatType: "group", Sender: Sender{ID: "user-1"}})
	if err != nil {
		t.Fatal(err)
	}
	topic, err := store.GetConversation(ctx, message.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if topic.ChatMode != ChatModeTopic {
		t.Fatalf("topic chat mode=%q", topic.ChatMode)
	}
	_, err = service.PutConversation(ctx, group.ID, ConversationInput{SessionStrategy: SessionStrategyShared, WorkspaceID: "workspace-1", Enabled: true, AllowedSenderIDs: []string{"user-1"}})
	if err == nil || !strings.Contains(err.Error(), "topic groups require") {
		t.Fatalf("shared topic group error=%v", err)
	}
}

func TestConversationBindingValidationRunsInsideConfigWriteBarrier(t *testing.T) {
	ctx := context.Background()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{&fakeAdapter{}}})
	if _, err := store.PutConnection(ctx, Connection{ID: "connection-1", Provider: ProviderFeishu, Name: "test", AppID: "app"}); err != nil {
		t.Fatal(err)
	}
	conversation, _ := store.EnsureConversation(ctx, "connection-1", "chat-1", "group", time.Now())
	validationStarted := make(chan struct{})
	releaseValidation := make(chan struct{})
	putDone := make(chan error, 1)
	go func() {
		_, err := service.PutConversationValidated(ctx, conversation.ID, ConversationInput{WorkspaceID: "workspace-1", Enabled: true, AllowedSenderIDs: []string{"user-1"}}, func(_ context.Context, _ Conversation, prompt SessionPromptConfig, runtime SessionRuntimeConfig) (SessionPromptConfig, SessionRuntimeConfig, error) {
			close(validationStarted)
			<-releaseValidation
			return prompt, runtime, nil
		})
		putDone <- err
	}()
	<-validationStarted

	readDone := make(chan error, 1)
	go func() {
		_, err := service.ListConversations(ctx, "connection-1")
		readDone <- err
	}()
	select {
	case err := <-readDone:
		t.Fatalf("configuration read crossed binding validation barrier: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseValidation)
	if err := <-putDone; err != nil {
		t.Fatal(err)
	}
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
}

func TestServiceClaimsDuplicateEventOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	adapter := &fakeAdapter{}
	called := make(chan Task, 8)
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	service.SetDispatcher(fakeDispatcher{called: called})
	secret := "secret"
	_, err := service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, AllowChatIDs: []string{"allowed"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	bindTestGroup(t, store, "allowed", "user-1")
	conversation, err := store.EnsureConversationRoot(ctx, "connection-1", "allowed", "duplicate", "", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.PutConversation(ctx, conversation.ID, ConversationInput{SessionID: "session-1", Enabled: true, AllowedSenderIDs: []string{"user-1"}})
	if err != nil {
		t.Fatal(err)
	}
	in := InboundMessage{MessageID: "duplicate", ChatID: "allowed", ChatType: "group", Sender: Sender{ID: "user-1"}, Text: "hello", MentionedBot: true, CreatedAt: time.Now()}
	var wait sync.WaitGroup
	for index := 0; index < 8; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if callbackErr := adapter.callbacks.Inbound(ctx, in); callbackErr != nil {
				t.Errorf("inbound: %v", callbackErr)
			}
		}()
	}
	wait.Wait()
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("task was not dispatched")
	}
	select {
	case task := <-called:
		t.Fatalf("duplicate task dispatched: %+v", task)
	case <-time.After(50 * time.Millisecond):
	}
	adapter.mu.Lock()
	opens := adapter.opens
	adapter.mu.Unlock()
	if opens != 1 {
		t.Fatalf("progress cards opened=%d", opens)
	}
}

func TestConversationUpdateWaitsForInboundAdmission(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	adapter := &fakeAdapter{}
	dispatcher := blockingAdmissionDispatcher{entered: make(chan struct{}), release: make(chan struct{})}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	service.SetDispatcher(dispatcher)
	secret := "secret"
	if _, err := service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, AllowChatIDs: []string{"allowed"}}); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	bindTestGroup(t, store, "allowed", "user-1")
	conversation, err := store.EnsureConversationRoot(ctx, "connection-1", "allowed", "admission", "", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutConversation(ctx, conversation.ID, ConversationInput{SessionID: "session-1", Enabled: true, AllowedSenderIDs: []string{"user-1"}}); err != nil {
		t.Fatal(err)
	}
	inboundDone := make(chan error, 1)
	go func() {
		inboundDone <- adapter.callbacks.Inbound(ctx, InboundMessage{MessageID: "admission", ChatID: "allowed", ChatType: "group", Sender: Sender{ID: "user-1"}, Text: "hello", MentionedBot: true, CreatedAt: time.Now()})
	}()
	<-dispatcher.entered
	updateDone := make(chan error, 1)
	go func() {
		_, err := service.PutConversation(ctx, conversation.ID, ConversationInput{SessionID: "session-1", Enabled: false, AllowedSenderIDs: []string{"user-1"}})
		updateDone <- err
	}()
	select {
	case err := <-updateDone:
		t.Fatalf("conversation update crossed admission boundary: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(dispatcher.release)
	if err := <-inboundDone; err != nil {
		t.Fatal(err)
	}
	if err := <-updateDone; err != nil {
		t.Fatal(err)
	}
}

func TestDispatchFailureKeepsInternalDetailsOutOfProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	adapter := &fakeAdapter{}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	service.SetDispatcher(failingDispatcher{err: errors.New("provider failed while reading /private/workspace/config.yaml")})
	secret := "secret"
	if _, err := service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, AllowChatIDs: []string{"allowed"}}); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	bindTestGroup(t, store, "allowed", "user-1")
	conversation, _ := store.EnsureConversationRoot(ctx, "connection-1", "allowed", "failure", "", "group", time.Now())
	if _, err := service.PutConversation(ctx, conversation.ID, ConversationInput{SessionID: "session-1", Enabled: true, AllowedSenderIDs: []string{"user-1"}}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.callbacks.Inbound(ctx, InboundMessage{MessageID: "failure", ChatID: "allowed", ChatType: "group", Sender: Sender{ID: "user-1"}, MentionedBot: true, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		message, err := store.GetMessageByProviderID(ctx, "connection-1", "failure")
		if err == nil && message.TriggerStatus == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dispatch failure did not settle: %+v err=%v", message, err)
		}
		time.Sleep(time.Millisecond)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if len(adapter.updates) != 1 || strings.Contains(adapter.updates[0].Text, "/private/") || !strings.Contains(adapter.updates[0].Text, "Open the bound session") {
		t.Fatalf("public updates=%+v", adapter.updates)
	}
}

func TestProgressDeliveryFailureStopsFurtherExternalDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	adapter := &fakeAdapter{updateErr: errors.New("ambiguous transport failure")}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	service.SetDispatcher(progressDispatcher{})
	secret := "secret"
	if _, err := service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, AllowChatIDs: []string{"allowed"}}); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	bindTestGroup(t, store, "allowed", "user-1")
	conversation, _ := store.EnsureConversationRoot(ctx, "connection-1", "allowed", "delivery-failure", "", "group", time.Now())
	if _, err := service.PutConversation(ctx, conversation.ID, ConversationInput{SessionID: "session-1", Enabled: true, AllowedSenderIDs: []string{"user-1"}}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.callbacks.Inbound(ctx, InboundMessage{MessageID: "delivery-failure", ChatID: "allowed", ChatType: "group", Sender: Sender{ID: "user-1"}, MentionedBot: true, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		message, err := store.GetMessageByProviderID(ctx, "connection-1", "delivery-failure")
		if err == nil && message.TriggerStatus == "delivery_unknown" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("message did not become delivery_unknown: %+v err=%v", message, err)
		}
		time.Sleep(time.Millisecond)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if len(adapter.updates) != 1 {
		t.Fatalf("external delivery continued after ambiguity: %+v", adapter.updates)
	}
}

func TestProgressDeliveryHasBoundedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	adapter := &fakeAdapter{updateFunc: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	service.deliveryTimeout = 10 * time.Millisecond
	service.SetDispatcher(progressDispatcher{})
	secret := "secret"
	if _, err := service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, AllowChatIDs: []string{"allowed"}}); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	bindTestGroup(t, store, "allowed", "user-1")
	conversation, _ := store.EnsureConversationRoot(ctx, "connection-1", "allowed", "delivery-timeout", "", "group", time.Now())
	if _, err := service.PutConversation(ctx, conversation.ID, ConversationInput{SessionID: "session-1", Enabled: true, AllowedSenderIDs: []string{"user-1"}}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.callbacks.Inbound(ctx, InboundMessage{MessageID: "delivery-timeout", ChatID: "allowed", ChatType: "group", Sender: Sender{ID: "user-1"}, MentionedBot: true, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		message, err := store.GetMessageByProviderID(ctx, "connection-1", "delivery-timeout")
		if err == nil && message.TriggerStatus == "delivery_unknown" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("bounded delivery did not settle: %+v err=%v", message, err)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestUncertainProgressOpenIsVisibleForReconciliation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	adapter := &fakeAdapter{openErr: UncertainDelivery(errors.New("response lost after send"))}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	called := make(chan Task, 1)
	service.SetDispatcher(fakeDispatcher{called: called})
	secret := "secret"
	if _, err := service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, AllowChatIDs: []string{"allowed"}}); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	bindTestGroup(t, store, "allowed", "user-1")
	conversation, _ := store.EnsureConversationRoot(ctx, "connection-1", "allowed", "uncertain-open", "", "group", time.Now())
	if _, err := service.PutConversation(ctx, conversation.ID, ConversationInput{SessionID: "session-1", Enabled: true, AllowedSenderIDs: []string{"user-1"}}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.callbacks.Inbound(ctx, InboundMessage{MessageID: "uncertain-open", ChatID: "allowed", ChatType: "group", Sender: Sender{ID: "user-1"}, MentionedBot: true, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	message, err := store.GetMessageByProviderID(ctx, "connection-1", "uncertain-open")
	if err != nil {
		t.Fatal(err)
	}
	if message.TriggerStatus != "delivery_unknown" || !strings.Contains(message.StatusDetail, "progress_message_delivery_unknown") {
		t.Fatalf("message=%+v", message)
	}
	select {
	case task := <-called:
		t.Fatalf("uncertain carrier started agent: %+v", task)
	default:
	}
}

func TestReceiptPersistsAfterInboundContextCancellation(t *testing.T) {
	serviceCtx, cancelService := context.WithCancel(context.Background())
	defer cancelService()
	inboundCtx, cancelInbound := context.WithCancel(context.Background())
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	adapter := &fakeAdapter{afterOpen: cancelInbound, processingMarkerID: "reaction-1"}
	called := make(chan Task, 1)
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	service.SetDispatcher(fakeDispatcher{called: called})
	secret := "secret"
	if _, err := service.PutConnection(context.Background(), "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, AllowChatIDs: []string{"allowed"}}); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(serviceCtx); err != nil {
		t.Fatal(err)
	}
	bindTestGroup(t, store, "allowed", "user-1")
	conversation, _ := store.EnsureConversationRoot(context.Background(), "connection-1", "allowed", "canceled", "", "group", time.Now())
	if _, err := service.PutConversation(context.Background(), conversation.ID, ConversationInput{SessionID: "session-1", Enabled: true, AllowedSenderIDs: []string{"user-1"}}); err != nil {
		t.Fatal(err)
	}
	_ = adapter.callbacks.Inbound(inboundCtx, InboundMessage{MessageID: "canceled", ChatID: "allowed", ChatType: "group", Sender: Sender{ID: "user-1"}, MentionedBot: true, CreatedAt: time.Now()})
	message, err := store.GetMessageByProviderID(context.Background(), "connection-1", "canceled")
	if err != nil {
		t.Fatal(err)
	}
	if message.ReplyMessageID != "reply-1" || (message.TriggerStatus != "running" && message.TriggerStatus != "processed") {
		t.Fatalf("receipt was not persisted: %+v", message)
	}
	deadline := time.Now().Add(time.Second)
	for {
		message, err = store.GetMessageByProviderID(context.Background(), "connection-1", "canceled")
		if err == nil && message.TriggerStatus == "processed" && message.ProcessingMarkerID == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("processing marker was not cleared at completion: %+v err=%v", message, err)
		}
		time.Sleep(time.Millisecond)
	}
	adapter.mu.Lock()
	clears := adapter.markerClears
	adapter.mu.Unlock()
	if clears != 1 {
		t.Fatalf("processing marker clears=%d", clears)
	}
}

type collectingFactory struct {
	mu       sync.Mutex
	adapters []*fakeAdapter
}

func (*collectingFactory) Capabilities() AdapterCapabilities { return fakeFactory{}.Capabilities() }

type blockingStopAdapter struct {
	fakeAdapter
	stopEntered chan struct{}
	stopRelease chan struct{}
	stopOnce    sync.Once
}

func (a *blockingStopAdapter) Stop(context.Context) error {
	a.stopOnce.Do(func() { close(a.stopEntered) })
	<-a.stopRelease
	return nil
}

type blockingStopFactory struct{ adapter *blockingStopAdapter }

func (blockingStopFactory) Capabilities() AdapterCapabilities { return fakeFactory{}.Capabilities() }

func (f blockingStopFactory) New(_ Connection, _ string, callbacks AdapterCallbacks) (Adapter, error) {
	f.adapter.callbacks = callbacks
	return f.adapter, nil
}

func (f *collectingFactory) New(_ Connection, _ string, callbacks AdapterCallbacks) (Adapter, error) {
	adapter := &fakeAdapter{callbacks: callbacks}
	f.mu.Lock()
	f.adapters = append(f.adapters, adapter)
	f.mu.Unlock()
	return adapter, nil
}

type multiAdapterFactory struct {
	mu       sync.Mutex
	adapters map[string]*fakeAdapter
}

func (*multiAdapterFactory) Capabilities() AdapterCapabilities { return fakeFactory{}.Capabilities() }

func (f *multiAdapterFactory) New(connection Connection, _ string, callbacks AdapterCallbacks) (Adapter, error) {
	adapter := &fakeAdapter{callbacks: callbacks}
	f.mu.Lock()
	if f.adapters == nil {
		f.adapters = make(map[string]*fakeAdapter)
	}
	f.adapters[connection.ID] = adapter
	f.mu.Unlock()
	return adapter, nil
}

func (f *multiAdapterFactory) adapter(connectionID string) *fakeAdapter {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.adapters[connectionID]
}

func TestInboundAdmissionDoesNotBlockAnotherConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := Open(t.TempDir())
	defer store.Close()
	secrets, _ := NewSecretStore(t.TempDir())
	factory := &multiAdapterFactory{}
	dispatcher := crossConnectionDispatcher{firstEntered: make(chan struct{}), firstRelease: make(chan struct{}), secondEntered: make(chan struct{})}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: factory})
	service.SetDispatcher(dispatcher)
	secret := "secret"
	for index := 1; index <= 2; index++ {
		connectionID := fmt.Sprintf("connection-%d", index)
		appID := fmt.Sprintf("app-%d", index)
		if _, err := service.PutConnection(ctx, connectionID, ConnectionInput{Provider: ProviderFeishu, Name: connectionID, AppID: appID, AppSecret: &secret, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		group, err := store.EnsureConversation(ctx, connectionID, "chat", "group", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		group.WorkspaceID, group.Enabled, group.AllowedSenderIDs = "workspace", true, []string{"user"}
		if _, err = store.PutConversation(ctx, group); err != nil {
			t.Fatal(err)
		}
		conversation, err := store.EnsureConversationRoot(ctx, connectionID, "chat", "root", "", "group", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		review := false
		conversation.SessionID, conversation.WorkspaceID, conversation.Enabled = "session-"+connectionID, "workspace", true
		conversation.AllowedSenderIDs = []string{"user"}
		conversation.AgentInstructionsMode, conversation.ApprovalInstructionsMode, conversation.ReviewAllTools = OverrideReplace, OverrideReplace, &review
		if _, err = store.PutConversation(ctx, conversation); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- factory.adapter("connection-1").callbacks.Inbound(ctx, InboundMessage{MessageID: "first", ChatID: "chat", RootID: "root", ChatType: "group", ConversationKey: "chat\x00root", TriggerAllowed: true, Sender: Sender{ID: "user"}, CreatedAt: time.Now()})
	}()
	select {
	case <-dispatcher.firstEntered:
	case <-time.After(time.Second):
		t.Fatal("first connection did not enter admission")
	}
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- factory.adapter("connection-2").callbacks.Inbound(ctx, InboundMessage{MessageID: "second", ChatID: "chat", RootID: "root", ChatType: "group", ConversationKey: "chat\x00root", TriggerAllowed: true, Sender: Sender{ID: "user"}, CreatedAt: time.Now()})
	}()
	select {
	case <-dispatcher.secondEntered:
	case <-time.After(time.Second):
		t.Fatal("second connection was blocked by first connection admission")
	}
	close(dispatcher.firstRelease)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func TestInboundAdmissionSerializesTheSameConversationKey(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := Open(t.TempDir())
	defer store.Close()
	secrets, _ := NewSecretStore(t.TempDir())
	adapter := &fakeAdapter{}
	dispatcher := &serialAdmissionDispatcher{firstEntered: make(chan struct{}), firstRelease: make(chan struct{}), secondEntered: make(chan struct{})}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	service.SetDispatcher(dispatcher)
	secret := "secret"
	if _, err := service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "bot", AppID: "app", AppSecret: &secret, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	bindTestGroup(t, store, "chat", "user")
	conversation, err := store.EnsureConversationRoot(ctx, "connection-1", "chat", "root", "", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	review := false
	conversation.SessionID, conversation.WorkspaceID, conversation.Enabled = "session", "workspace-1", true
	conversation.AllowedSenderIDs = []string{"user"}
	conversation.AgentInstructionsMode, conversation.ApprovalInstructionsMode, conversation.ReviewAllTools = OverrideReplace, OverrideReplace, &review
	if _, err = store.PutConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	send := func(messageID string) <-chan error {
		done := make(chan error, 1)
		go func() {
			done <- adapter.callbacks.Inbound(ctx, InboundMessage{MessageID: messageID, ChatID: "chat", RootID: "root", ChatType: "group", ConversationKey: "chat\x00root", TriggerAllowed: true, Sender: Sender{ID: "user"}, CreatedAt: time.Now()})
		}()
		return done
	}
	firstDone := send("first")
	<-dispatcher.firstEntered
	secondDone := send("second")
	select {
	case <-dispatcher.secondEntered:
		t.Fatal("second message entered admission before the first released its conversation lock")
	case <-time.After(50 * time.Millisecond):
	}
	close(dispatcher.firstRelease)
	select {
	case <-dispatcher.secondEntered:
	case <-time.After(time.Second):
		t.Fatal("second message did not enter after the first admission completed")
	}
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	service.inboxLocksMu.Lock()
	defer service.inboxLocksMu.Unlock()
	if len(service.inboxLocks) != 0 {
		t.Fatalf("released conversation locks=%d, want 0", len(service.inboxLocks))
	}
}

func TestServiceOnlyRestartsAdapterForRuntimeConfigurationChanges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	factory := &collectingFactory{}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: factory})
	secret := "secret"
	owners := []string{"owner-1"}
	input := ConnectionInput{
		Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret,
		Enabled: true, AllowChatIDs: []string{"allowed"}, OwnerSenderIDs: &owners,
		ProgressMode: ProgressModeCOT, ReviewAllTools: true,
	}
	if _, err := service.PutConnection(ctx, "connection-1", input); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}

	factory.mu.Lock()
	adapterCount := len(factory.adapters)
	factory.mu.Unlock()
	if adapterCount != 1 {
		t.Fatalf("adapters after start=%d, want 1", adapterCount)
	}

	input.Name = "renamed"
	input.AppSecret = nil
	input.OwnerSenderIDs = nil
	input.AgentInstructions = "updated agent instructions"
	input.ApprovalInstructions = "updated approval instructions"
	input.AllowChatIDs = []string{" allowed ", "allowed"}
	if _, err := service.PutConnection(ctx, "connection-1", input); err != nil {
		t.Fatal(err)
	}
	factory.mu.Lock()
	adapterCount = len(factory.adapters)
	factory.mu.Unlock()
	if adapterCount != 1 {
		t.Fatalf("metadata update created %d adapters, want 1", adapterCount)
	}

	input.AllowChatIDs = []string{"allowed", "another"}
	if _, err := service.PutConnection(ctx, "connection-1", input); err != nil {
		t.Fatal(err)
	}
	factory.mu.Lock()
	adapterCount = len(factory.adapters)
	factory.mu.Unlock()
	if adapterCount != 2 {
		t.Fatalf("runtime update created %d adapters, want 2", adapterCount)
	}
}

func TestServiceRejectsCallbacksFromReplacedConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	factory := &collectingFactory{}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: factory})
	secret := "secret"
	input := ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "old-app", AppSecret: &secret, Enabled: true, AllowChatIDs: []string{"allowed"}}
	if _, err := service.PutConnection(ctx, "connection-1", input); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	factory.mu.Lock()
	old := factory.adapters[0]
	factory.mu.Unlock()
	input.AppID = "new-app"
	input.AppSecret = nil
	if _, err := service.PutConnection(ctx, "connection-1", input); err != nil {
		t.Fatal(err)
	}
	old.callbacks.Error(errors.New("late old error"))
	if err := old.callbacks.Inbound(ctx, InboundMessage{MessageID: "old", ChatID: "allowed", ChatType: "group", Text: "old", MentionedBot: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetMessageByProviderID(ctx, "connection-1", "old"); err != ErrNotFound {
		t.Fatalf("old callback retained: %v", err)
	}
	connection, _ := service.GetConnection(ctx, "connection-1")
	if connection.LastError == "late old error" {
		t.Fatal("old callback overwrote new connection status")
	}
}

func TestDeleteSerializesWithConnectionReplacement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	adapter := &blockingStopAdapter{stopEntered: make(chan struct{}), stopRelease: make(chan struct{})}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: blockingStopFactory{adapter}})
	secret := "secret"
	input := ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, AllowChatIDs: []string{"allowed"}}
	if _, err := service.PutConnection(ctx, "connection-1", input); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- service.DeleteConnection(ctx, "connection-1") }()
	<-adapter.stopEntered
	input.AppSecret = nil
	putDone := make(chan error, 1)
	go func() {
		_, err := service.PutConnection(ctx, "connection-1", input)
		putDone <- err
	}()
	select {
	case err := <-putDone:
		t.Fatalf("replacement crossed delete boundary: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(adapter.stopRelease)
	if err := <-deleteDone; err != nil {
		t.Fatal(err)
	}
	if err := <-putDone; err == nil {
		t.Fatal("replacement unexpectedly reused the deleted secret")
	}
	if _, err := store.GetConnection(ctx, "connection-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted connection reappeared: %v", err)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.runs["connection-1"] != nil {
		t.Fatal("adapter remained active after delete")
	}
}

func TestServiceFailsInterruptedDeliveryOnRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	store, _ := Open(root)
	secrets, _ := NewSecretStore(root)
	secret := "secret"
	if err := secrets.Set("connection-1", secret); err != nil {
		t.Fatal(err)
	}
	_, err := store.PutConnection(ctx, Connection{ID: "connection-1", Provider: ProviderFeishu, Name: "test", AppID: "app", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	bindTestGroup(t, store, "allowed", "user-1")
	message, err := store.RecordMessage(ctx, "connection-1", InboundMessage{MessageID: "pending", ChatID: "allowed", ChatType: "group", Sender: Sender{ID: "user-1"}, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimMessage(ctx, message.ID)
	if err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if err := store.SetMessageRunning(ctx, message.ID, DeliveryReceipt{Mode: ProgressModeCOT, MessageID: "reply-1", COTID: "cot-1", ProcessingMarkerID: "reaction-1"}); err != nil {
		t.Fatal(err)
	}
	conversation, err := store.GetConversation(ctx, message.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	conversation.Enabled = true
	conversation.SessionID = "session-1"
	conversation.AllowedSenderIDs = []string{"user-1"}
	if _, err := store.PutConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	factory := &collectingFactory{}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: factory})
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.GetMessageByProviderID(ctx, "connection-1", "pending")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.TriggerStatus != "failed" || recovered.StatusDetail != "daemon_restarted_before_completion" {
		t.Fatalf("message=%+v", recovered)
	}
	factory.mu.Lock()
	adapter := factory.adapters[0]
	factory.mu.Unlock()
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if len(adapter.updates) != 1 || adapter.updates[0].State != "failed" {
		t.Fatalf("updates=%+v", adapter.updates)
	}
	if len(adapter.receipts) == 0 || adapter.receipts[0].ProcessingMarkerID != "reaction-1" {
		t.Fatalf("receipts=%+v", adapter.receipts)
	}
	if recovered.ProcessingMarkerID != "" {
		t.Fatalf("processing marker was not cleared: %+v", recovered)
	}
	if adapter.receipts[0].Mode != ProgressModeCOT || adapter.receipts[0].COTID != "cot-1" {
		t.Fatalf("receipts=%+v", adapter.receipts)
	}
}

func TestPersistedProcessingMarkerIntentRecoversClaimedMessage(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.PutConnection(ctx, Connection{ID: "connection-1", Provider: ProviderFeishu, Name: "test", AppID: "app"}); err != nil {
		t.Fatal(err)
	}
	message, err := store.RecordMessage(ctx, "connection-1", InboundMessage{
		MessageID: "origin-pending", ChatID: "allowed", ChatType: "group",
		Sender: Sender{ID: "user-1"}, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimMessage(ctx, message.ID)
	if err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if err := store.SetMessageProcessingMarker(ctx, message.ID, "provider-marker-pending"); err != nil {
		t.Fatal(err)
	}
	active, err := store.ListMessagesWithProcessingMarkers(ctx, "connection-1")
	if err != nil || len(active) != 0 {
		t.Fatalf("active marker was eligible for reconciliation: %+v err=%v", active, err)
	}
	if _, err := store.RecoverPendingMessages(ctx); err != nil {
		t.Fatal(err)
	}
	adapter := &fakeAdapter{processingMarkerID: "provider-marker-pending"}
	service := &Service{store: store, logger: log.New(io.Discard, "", 0)}
	service.reconcileProcessingMarkers(ctx, "connection-1", adapter)
	recovered, err := store.GetMessageByProviderID(ctx, "connection-1", "origin-pending")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ProcessingMarkerID != "" {
		t.Fatalf("processing marker intent was not cleared: %+v", recovered)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.markerClears != 1 || len(adapter.receipts) != 1 || adapter.receipts[0].ProcessingMarkerID != "provider-marker-pending" {
		t.Fatalf("marker clears=%d receipts=%+v", adapter.markerClears, adapter.receipts)
	}
}

func TestProcessingMarkerReconciliationDrainsBeforeAdapterStop(t *testing.T) {
	ctx := context.Background()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	if _, err := store.PutConnection(ctx, Connection{ID: "connection-1", Provider: ProviderFeishu, Name: "test", AppID: "app"}); err != nil {
		t.Fatal(err)
	}
	message, err := store.RecordMessage(ctx, "connection-1", InboundMessage{MessageID: "terminal-marker", ChatID: "allowed", ChatType: "group", Sender: Sender{ID: "user-1"}, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimMessage(ctx, message.ID); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if err := store.SetMessageProcessingMarker(ctx, message.ID, "feishu:on_it:pending"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMessageStatus(ctx, message.ID, "failed", "test"); err != nil {
		t.Fatal(err)
	}
	cleanupEntered := make(chan struct{})
	cleanupRelease := make(chan struct{})
	var stopped atomic.Bool
	adapter := &fakeAdapter{
		clearMarker: func(context.Context) error {
			close(cleanupEntered)
			<-cleanupRelease
			if stopped.Load() {
				return errors.New("adapter stopped during marker cleanup")
			}
			return nil
		},
		stopFunc: func(context.Context) error {
			stopped.Store(true)
			return nil
		},
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	run := &adapterRun{adapter: adapter, cancel: cancelRun, generation: 1, ctx: runCtx}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), nil)
	service.ctx = ctx
	service.runs["connection-1"] = run
	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- service.reconcileRunProcessingMarkers(ctx, "connection-1") }()
	<-cleanupEntered
	stopCtx, cancelStop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancelStop()
	if _, err := service.stop(stopCtx, "connection-1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop error=%v, want deadline", err)
	}
	if stopped.Load() {
		t.Fatal("adapter stopped before marker reconciliation drained")
	}
	close(cleanupRelease)
	if err := <-reconcileDone; err != nil {
		t.Fatal(err)
	}
	if _, err := service.stop(ctx, "connection-1"); err != nil {
		t.Fatal(err)
	}
	if !stopped.Load() {
		t.Fatal("adapter was not stopped after reconciliation drained")
	}
}

func TestPutConnectionDoesNotPersistWhenMarkerCleanupFails(t *testing.T) {
	ctx := context.Background()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	secret := "original-secret"
	if err := secrets.Set("connection-1", secret); err != nil {
		t.Fatal(err)
	}
	original := Connection{ID: "connection-1", Provider: ProviderFeishu, Name: "original", AppID: "original-app", Enabled: true, ProgressMode: ProgressModeInteractiveCard}
	if _, err := store.PutConnection(ctx, original); err != nil {
		t.Fatal(err)
	}
	message, err := store.RecordMessage(ctx, "connection-1", InboundMessage{MessageID: "cleanup-fails", ChatID: "allowed", ChatType: "group", Sender: Sender{ID: "user-1"}, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimMessage(ctx, message.ID); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if err := store.SetMessageProcessingMarker(ctx, message.ID, "feishu:on_it:pending"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMessageStatus(ctx, message.ID, "failed", "test"); err != nil {
		t.Fatal(err)
	}
	adapter := &fakeAdapter{clearMarker: func(context.Context) error { return errors.New("provider unavailable") }}
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	service.ctx = ctx
	service.runs["connection-1"] = &adapterRun{adapter: adapter, cancel: cancelRun, generation: 1, ctx: runCtx}
	_, err = service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "changed", AppID: "changed-app", Enabled: true, ProgressMode: ProgressModeInteractiveCard})
	if err == nil || !strings.Contains(err.Error(), "clean channel processing markers") {
		t.Fatalf("PutConnection error=%v", err)
	}
	stored, err := store.GetConnection(ctx, "connection-1")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != original.Name || stored.AppID != original.AppID {
		t.Fatalf("connection changed despite cleanup failure: %+v", stored)
	}
	storedSecret, ok, err := secrets.Get("connection-1")
	if err != nil || !ok || storedSecret != secret {
		t.Fatalf("secret changed despite cleanup failure: value=%q ok=%v err=%v", storedSecret, ok, err)
	}
	service.mu.Lock()
	retained := service.runs["connection-1"] != nil
	service.mu.Unlock()
	if !retained {
		t.Fatal("active run was removed despite cleanup failure")
	}
}

func TestPutConnectionCanRecoverMarkersWithReplacementCredentialWithoutRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	if err := secrets.Set("connection-1", "expired-secret"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutConnection(ctx, Connection{ID: "connection-1", Provider: ProviderFeishu, Name: "test", AppID: "app", Enabled: true, ProgressMode: ProgressModeInteractiveCard}); err != nil {
		t.Fatal(err)
	}
	message, err := store.RecordMessage(ctx, "connection-1", InboundMessage{MessageID: "recover-with-new-secret", ChatID: "allowed", ChatType: "group", Sender: Sender{ID: "user-1"}, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimMessage(ctx, message.ID); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if err := store.SetMessageProcessingMarker(ctx, message.ID, "feishu:on_it:pending"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMessageStatus(ctx, message.ID, "failed", "test"); err != nil {
		t.Fatal(err)
	}
	oldAdapter := &recoveryRequiredAdapter{prepareErr: errors.New("credential expired")}
	newAdapter := &recoveryRequiredAdapter{}
	factory := &recoveryCredentialFactory{old: oldAdapter, new: newAdapter}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: factory})
	service.ctx = ctx
	newSecret := "working-secret"
	if _, err := service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &newSecret, Enabled: true, ProgressMode: ProgressModeInteractiveCard}); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.GetMessageByProviderID(ctx, "connection-1", "recover-with-new-secret")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ProcessingMarkerID != "" {
		t.Fatalf("replacement credential did not clear marker: %+v", recovered)
	}
	if !newAdapter.prepared.Load() || oldAdapter.stops.Load() != 1 || newAdapter.stops.Load() == 0 {
		t.Fatalf("recovery lifecycle incomplete: prepared=%v old stops=%d new stops=%d", newAdapter.prepared.Load(), oldAdapter.stops.Load(), newAdapter.stops.Load())
	}
	storedSecret, ok, err := secrets.Get("connection-1")
	if err != nil || !ok || storedSecret != newSecret {
		t.Fatalf("replacement credential was not saved: value=%q ok=%v err=%v", storedSecret, ok, err)
	}
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPutConnectionDoesNotUseNewPrincipalToCleanOldMarkers(t *testing.T) {
	ctx := context.Background()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	if err := secrets.Set("connection-1", "expired-secret"); err != nil {
		t.Fatal(err)
	}
	original := Connection{ID: "connection-1", Provider: ProviderFeishu, Name: "old", AppID: "old-app", Enabled: true, ProgressMode: ProgressModeInteractiveCard}
	if _, err := store.PutConnection(ctx, original); err != nil {
		t.Fatal(err)
	}
	message, _ := store.RecordMessage(ctx, "connection-1", InboundMessage{MessageID: "old-principal-marker", ChatID: "allowed", ChatType: "group", Sender: Sender{ID: "user-1"}, CreatedAt: time.Now()})
	if claimed, err := store.ClaimMessage(ctx, message.ID); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if err := store.SetMessageProcessingMarker(ctx, message.ID, "feishu:on_it:pending"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMessageStatus(ctx, message.ID, "failed", "test"); err != nil {
		t.Fatal(err)
	}
	oldAdapter := &recoveryRequiredAdapter{prepareErr: errors.New("old principal unavailable")}
	newAdapter := &recoveryRequiredAdapter{}
	factory := &recoveryCredentialFactory{old: oldAdapter, new: newAdapter}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: factory})
	service.ctx = ctx
	newSecret := "working-secret"
	_, err := service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "new", AppID: "new-app", AppSecret: &newSecret, Enabled: true, ProgressMode: ProgressModeInteractiveCard})
	if err == nil {
		t.Fatal("new principal was allowed to prove cleanup of old marker")
	}
	if newAdapter.prepared.Load() {
		t.Fatal("new principal was used for old marker cleanup")
	}
	stored, err := store.GetConnection(ctx, "connection-1")
	if err != nil || stored.AppID != original.AppID {
		t.Fatalf("old connection was not preserved: %+v err=%v", stored, err)
	}
}

func TestPutConnectionRetriesRevokedRunAfterStopFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	var stops atomic.Int32
	adapter := &fakeAdapter{stopFunc: func(context.Context) error {
		if stops.Add(1) == 1 {
			return errors.New("temporary stop failure")
		}
		return nil
	}}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	service.ctx = ctx
	secret := "secret"
	if _, err := service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "old", AppID: "old-app", AppSecret: &secret, Enabled: true, ProgressMode: ProgressModeInteractiveCard}); err != nil {
		t.Fatal(err)
	}
	updated := ConnectionInput{Provider: ProviderFeishu, Name: "new", AppID: "new-app", Enabled: true, ProgressMode: ProgressModeInteractiveCard}
	if _, err := service.PutConnection(ctx, "connection-1", updated); err == nil || !strings.Contains(err.Error(), "temporary stop failure") {
		t.Fatalf("first reconfiguration error=%v", err)
	}
	service.mu.Lock()
	failedRun := service.runs["connection-1"]
	service.mu.Unlock()
	if failedRun == nil {
		t.Fatal("failed Stop discarded the run needed for retry")
	}
	failedRun.sendMu.Lock()
	revoked := failedRun.revoked
	failedRun.sendMu.Unlock()
	if !revoked {
		t.Fatal("failed Stop run was not revoked")
	}
	if _, err := service.PutConnection(ctx, "connection-1", updated); err != nil {
		t.Fatalf("retry same configuration: %v", err)
	}
	service.mu.Lock()
	activeRun := service.runs["connection-1"]
	service.mu.Unlock()
	if activeRun == nil || activeRun == failedRun {
		t.Fatal("retry did not replace the revoked run")
	}
	activeRun.sendMu.Lock()
	stillRevoked := activeRun.revoked
	activeRun.sendMu.Unlock()
	if stillRevoked || stops.Load() < 2 {
		t.Fatalf("retry state: revoked=%v stops=%d", stillRevoked, stops.Load())
	}
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPutConnectionWaitsForRevokedRunToDrainBeforeMarkerCleanup(t *testing.T) {
	ctx := context.Background()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	if err := secrets.Set("connection-1", "secret"); err != nil {
		t.Fatal(err)
	}
	original := Connection{ID: "connection-1", Provider: ProviderFeishu, Name: "old", AppID: "old-app", Enabled: true, ProgressMode: ProgressModeInteractiveCard}
	if _, err := store.PutConnection(ctx, original); err != nil {
		t.Fatal(err)
	}
	message, _ := store.RecordMessage(ctx, "connection-1", InboundMessage{MessageID: "drain-before-cleanup", ChatID: "allowed", ChatType: "group", Sender: Sender{ID: "user-1"}, CreatedAt: time.Now()})
	if claimed, err := store.ClaimMessage(ctx, message.ID); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if err := store.SetMessageProcessingMarker(ctx, message.ID, "reaction-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMessageStatus(ctx, message.ID, "failed", "test"); err != nil {
		t.Fatal(err)
	}
	cleanupEntered := make(chan struct{})
	var cleanupOnce sync.Once
	var stops atomic.Int32
	adapter := &fakeAdapter{
		clearMarker: func(context.Context) error {
			cleanupOnce.Do(func() { close(cleanupEntered) })
			return nil
		},
		stopFunc: func(context.Context) error {
			stops.Add(1)
			return nil
		},
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	run := &adapterRun{adapter: adapter, cancel: cancelRun, generation: 1, ctx: runCtx}
	run.sendMu.Lock()
	run.deliveries.Add(1)
	run.sendMu.Unlock()
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	service.ctx = ctx
	service.runs["connection-1"] = run
	updated := ConnectionInput{Provider: ProviderFeishu, Name: "new", AppID: "new-app", Enabled: true, ProgressMode: ProgressModeInteractiveCard}
	firstCtx, cancelFirst := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancelFirst()
	if _, err := service.PutConnection(firstCtx, "connection-1", updated); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Put error=%v, want deadline", err)
	}
	stored, err := store.GetConnection(ctx, "connection-1")
	if err != nil || stored.AppID != original.AppID {
		t.Fatalf("timed-out Put changed connection: %+v err=%v", stored, err)
	}
	select {
	case <-cleanupEntered:
		t.Fatal("marker cleanup ran before delivery drained")
	default:
	}
	secondDone := make(chan error, 1)
	go func() {
		_, err := service.PutConnection(ctx, "connection-1", updated)
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("retry bypassed delivery drain: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if stops.Load() != 0 {
		t.Fatal("adapter stopped before delivery drained")
	}
	run.deliveries.Done()
	select {
	case <-cleanupEntered:
	case <-time.After(time.Second):
		t.Fatal("marker cleanup did not run after delivery drained")
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if stops.Load() == 0 {
		t.Fatal("adapter was not stopped after delivery drained")
	}
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteConnectionCleansMarkersWithoutActiveRun(t *testing.T) {
	ctx := context.Background()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	if err := secrets.Set("connection-1", "working-secret"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutConnection(ctx, Connection{ID: "connection-1", Provider: ProviderFeishu, Name: "test", AppID: "app", Enabled: false, ProgressMode: ProgressModeInteractiveCard}); err != nil {
		t.Fatal(err)
	}
	message, _ := store.RecordMessage(ctx, "connection-1", InboundMessage{MessageID: "delete-without-run", ChatID: "allowed", ChatType: "group", Sender: Sender{ID: "user-1"}, CreatedAt: time.Now()})
	if claimed, err := store.ClaimMessage(ctx, message.ID); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if err := store.SetMessageProcessingMarker(ctx, message.ID, "feishu:on_it:pending"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMessageStatus(ctx, message.ID, "failed", "test"); err != nil {
		t.Fatal(err)
	}
	adapter := &recoveryRequiredAdapter{}
	factory := &recoveryCredentialFactory{old: &recoveryRequiredAdapter{}, new: adapter}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: factory})
	if err := service.DeleteConnection(ctx, "connection-1"); err != nil {
		t.Fatal(err)
	}
	if !adapter.prepared.Load() || adapter.stops.Load() != 1 {
		t.Fatalf("cleanup adapter lifecycle: prepared=%v stops=%d", adapter.prepared.Load(), adapter.stops.Load())
	}
	if _, err := store.GetConnection(ctx, "connection-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("connection still exists: %v", err)
	}
}

func TestListGroupsDiscardsOldRunResultAcrossReconfiguration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	requestEntered := make(chan struct{})
	requestRelease := make(chan struct{})
	var calls atomic.Int32
	oldAdapter := &fakeAdapter{listGroupsFunc: func(context.Context) ([]Group, error) {
		if calls.Add(1) == 1 {
			return nil, nil // automatic discovery during startup
		}
		close(requestEntered)
		<-requestRelease
		return []Group{{ChatID: "old-bot-group", Name: "Old bot group"}}, nil
	}}
	newAdapter := &fakeAdapter{}
	factory := &credentialFactory{adapters: map[string]*fakeAdapter{"old-secret": oldAdapter, "new-secret": newAdapter}}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: factory})
	service.ctx = ctx
	oldSecret := "old-secret"
	if _, err := service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "old", AppID: "old-app", AppSecret: &oldSecret, Enabled: true, ProgressMode: ProgressModeInteractiveCard}); err != nil {
		t.Fatal(err)
	}
	groupsDone := make(chan error, 1)
	go func() {
		_, err := service.ListGroups(ctx, "connection-1")
		groupsDone <- err
	}()
	<-requestEntered
	newSecret := "new-secret"
	putDone := make(chan error, 1)
	go func() {
		_, err := service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "new", AppID: "new-app", AppSecret: &newSecret, Enabled: true, ProgressMode: ProgressModeInteractiveCard})
		putDone <- err
	}()
	time.Sleep(20 * time.Millisecond)
	stored, err := store.GetConnection(ctx, "connection-1")
	if err != nil || stored.AppID != "old-app" {
		t.Fatalf("reconfiguration persisted before old group discovery drained: %+v err=%v", stored, err)
	}
	close(requestRelease)
	if err := <-groupsDone; err != nil {
		t.Fatal(err)
	}
	if err := <-putDone; err != nil {
		t.Fatal(err)
	}
	stored, err = store.GetConnection(ctx, "connection-1")
	if err != nil || stored.AppID != "new-app" {
		t.Fatalf("reconfiguration was not persisted after drain: %+v err=%v", stored, err)
	}
	conversations, err := store.ListConversations(ctx, "connection-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, conversation := range conversations {
		if conversation.ChatID == "old-bot-group" {
			t.Fatalf("old adapter group leaked across reconfiguration: %+v", conversation)
		}
	}
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestConversationMetadataDiscardsOldRunResultAcrossReconfiguration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	if err := secrets.Set("connection-1", "secret"); err != nil {
		t.Fatal(err)
	}
	connection := Connection{ID: "connection-1", Provider: ProviderFeishu, Name: "old", AppID: "old-app", Enabled: true, AllowChatIDs: []string{"allowed"}, ProgressMode: ProgressModeInteractiveCard}
	if _, err := store.PutConnection(ctx, connection); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureGroupConversation(ctx, "connection-1", "allowed"); err != nil {
		t.Fatal(err)
	}
	resolverEntered := make(chan struct{})
	resolverRelease := make(chan struct{})
	resolver := conversationNameResolverFunc(func(context.Context, string) (string, error) {
		close(resolverEntered)
		<-resolverRelease
		return "Old bot name", nil
	})
	oldAdapter := &fakeAdapter{}
	runCtx, cancelRun := context.WithCancel(ctx)
	run := &adapterRun{adapter: oldAdapter, cancel: cancelRun, generation: 1, ctx: runCtx}
	newAdapter := &fakeAdapter{}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{newAdapter}})
	service.ctx = ctx
	service.runs["connection-1"] = run
	service.startRunOperation("connection-1", run, func(operationCtx context.Context) {
		service.refreshConversationNames(operationCtx, connection, 1, resolver)
	})
	<-resolverEntered
	putDone := make(chan error, 1)
	go func() {
		_, err := service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "new", AppID: "new-app", Enabled: true, AllowChatIDs: []string{"allowed"}, ProgressMode: ProgressModeInteractiveCard})
		putDone <- err
	}()
	time.Sleep(20 * time.Millisecond)
	stored, err := store.GetConnection(ctx, "connection-1")
	if err != nil || stored.AppID != "old-app" {
		t.Fatalf("reconfiguration persisted before metadata lookup drained: %+v err=%v", stored, err)
	}
	close(resolverRelease)
	if err := <-putDone; err != nil {
		t.Fatal(err)
	}
	stored, err = store.GetConnection(ctx, "connection-1")
	if err != nil || stored.AppID != "new-app" {
		t.Fatalf("reconfiguration was not persisted after drain: %+v err=%v", stored, err)
	}
	group, err := store.GetConversationByScope(ctx, "connection-1", "allowed", "")
	if err != nil {
		t.Fatal(err)
	}
	if group.ChatName == "Old bot name" {
		t.Fatalf("old adapter metadata leaked across reconfiguration: %+v", group)
	}
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRevokedRunCannotPersistCallbacksOrInboundMessage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	adapter := &fakeAdapter{}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	service.ctx = ctx
	secret := "secret"
	if _, err := service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, AllowChatIDs: []string{"allowed"}, ProgressMode: ProgressModeInteractiveCard}); err != nil {
		t.Fatal(err)
	}
	bindTestGroup(t, store, "allowed", "user-1")
	service.revokeCurrentRun("connection-1")
	if err := store.SetConnectionRuntime(ctx, "connection-1", "sentinel", "", "current-bot", "Current bot"); err != nil {
		t.Fatal(err)
	}
	adapter.callbacks.Ready("stale-bot", "Stale bot")
	adapter.callbacks.Error(errors.New("stale error"))
	if err := adapter.callbacks.Inbound(ctx, InboundMessage{MessageID: "stale-inbound", ChatID: "allowed", ChatType: "group", ConversationKey: "allowed\x00stale-inbound", StartsConversation: true, TriggerAllowed: true, Sender: Sender{ID: "user-1"}, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	connection, err := store.GetConnection(ctx, "connection-1")
	if err != nil {
		t.Fatal(err)
	}
	if connection.Status != "sentinel" || connection.BotOpenID != "current-bot" {
		t.Fatalf("revoked callbacks changed runtime state: %+v", connection)
	}
	if _, err := store.GetMessageByProviderID(ctx, "connection-1", "stale-inbound"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked inbound message was persisted: %v", err)
	}
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteConnectionWaitsForActiveProcessingMarkerCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	cleanupEntered := make(chan struct{})
	cleanupRelease := make(chan struct{})
	var cleanupOnce sync.Once
	var adapterStopped atomic.Bool
	adapter := &fakeAdapter{
		processingMarkerID: "reaction-active",
		clearMarker: func(context.Context) error {
			cleanupOnce.Do(func() { close(cleanupEntered) })
			<-cleanupRelease
			if adapterStopped.Load() {
				return errors.New("marker API unavailable after adapter stop")
			}
			return nil
		},
		stopFunc: func(context.Context) error {
			adapterStopped.Store(true)
			return nil
		},
	}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{adapter}})
	dispatchEntered := make(chan struct{})
	service.SetDispatcher(cancelAwareDispatcher{entered: dispatchEntered})
	secret := "secret"
	if _, err := service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, AllowChatIDs: []string{"allowed"}}); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	bindTestGroup(t, store, "allowed", "user-1")
	conversation, _ := store.EnsureConversationRoot(ctx, "connection-1", "allowed", "active-delete", "", "group", time.Now())
	if _, err := service.PutConversation(ctx, conversation.ID, ConversationInput{SessionID: "session-1", Enabled: true, AllowedSenderIDs: []string{"user-1"}}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.callbacks.Inbound(ctx, InboundMessage{MessageID: "active-delete", ChatID: "allowed", ChatType: "group", Sender: Sender{ID: "user-1"}, MentionedBot: true, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	<-dispatchEntered
	firstDeleteCtx, cancelFirstDelete := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancelFirstDelete()
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- service.DeleteConnection(firstDeleteCtx, "connection-1") }()
	<-cleanupEntered
	if err := <-deleteDone; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first delete error=%v, want deadline", err)
	}
	if _, err := store.GetConnection(ctx, "connection-1"); err != nil {
		t.Fatalf("timed-out delete removed connection: %v", err)
	}
	secondDeleteDone := make(chan error, 1)
	go func() { secondDeleteDone <- service.DeleteConnection(ctx, "connection-1") }()
	select {
	case err := <-secondDeleteDone:
		t.Fatalf("retry delete bypassed draining marker cleanup: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(cleanupRelease)
	if err := <-secondDeleteDone; err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetConnection(ctx, "connection-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("connection still exists after cleanup: %v", err)
	}
}

func TestDeleteConnectionRetriesAdapterStopBeforeDeletingState(t *testing.T) {
	ctx := context.Background()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	var stops atomic.Int32
	adapter := &fakeAdapter{stopFunc: func(context.Context) error {
		if stops.Add(1) == 1 {
			return errors.New("temporary stop failure")
		}
		return nil
	}}
	runCtx, cancelRun := context.WithCancel(ctx)
	service := NewService(store, secrets, log.New(io.Discard, "", 0), nil)
	service.ctx = ctx
	service.runs["connection-1"] = &adapterRun{adapter: adapter, cancel: cancelRun, ctx: runCtx, generation: 1}
	if err := service.DeleteConnection(ctx, "connection-1"); err == nil {
		t.Fatal("first delete succeeded despite adapter stop failure")
	}
	service.mu.Lock()
	retained := service.runs["connection-1"] != nil
	service.mu.Unlock()
	if !retained {
		t.Fatal("failed adapter stop discarded the draining run")
	}
	if err := service.DeleteConnection(ctx, "connection-1"); err != nil {
		t.Fatal(err)
	}
	if stops.Load() != 2 {
		t.Fatalf("adapter stop attempts=%d", stops.Load())
	}
}

func TestCloseRetriesAfterDeliveryDrainTimeout(t *testing.T) {
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	var stops atomic.Int32
	adapter := &fakeAdapter{stopFunc: func(context.Context) error {
		stops.Add(1)
		return nil
	}}
	runCtx, cancelRun := context.WithCancel(context.Background())
	run := &adapterRun{adapter: adapter, cancel: cancelRun, ctx: runCtx, generation: 1}
	run.deliveries.Add(1)
	service := NewService(store, secrets, log.New(io.Discard, "", 0), nil)
	service.ctx = context.Background()
	service.runs["connection-1"] = run
	firstCtx, cancelFirst := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelFirst()
	if err := service.Close(firstCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first close error=%v, want deadline", err)
	}
	if stops.Load() != 0 {
		t.Fatalf("adapter stopped before deliveries drained: %d", stops.Load())
	}
	run.deliveries.Done()
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stops.Load() != 1 {
		t.Fatalf("adapter stop attempts=%d", stops.Load())
	}
	if _, err := store.ListConnections(context.Background()); err == nil {
		t.Fatal("session store remained open after retry close")
	}
}

func TestApprovalInstructionsRequireReviewAllTools(t *testing.T) {
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{&fakeAdapter{}}})
	_, err := service.PutConnection(context.Background(), "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", ApprovalInstructions: "restrict", ReviewAllTools: false})
	if err == nil {
		t.Fatal("expected unsafe approval configuration to be rejected")
	}
}

func TestEnabledConnectionCannotClearItsSecret(t *testing.T) {
	ctx := context.Background()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{&fakeAdapter{}}})
	secret := "original"
	input := ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, AllowChatIDs: []string{"allowed"}}
	if _, err := service.PutConnection(ctx, "connection-1", input); err != nil {
		t.Fatal(err)
	}
	input.AppSecret = nil
	input.ClearSecret = true
	if _, err := service.PutConnection(ctx, "connection-1", input); err == nil {
		t.Fatal("expected enabled connection without candidate secret to be rejected")
	}
	if got, ok, err := secrets.Get("connection-1"); err != nil || !ok || got != secret {
		t.Fatalf("secret changed after rejected update: value=%q ok=%v err=%v", got, ok, err)
	}
	connection, err := store.GetConnection(ctx, "connection-1")
	if err != nil || !connection.Enabled {
		t.Fatalf("connection changed after rejected update: %+v err=%v", connection, err)
	}
}

func TestConnectionAppIDIsUniqueWithinDaemon(t *testing.T) {
	ctx := context.Background()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{&fakeAdapter{}}})
	input := ConnectionInput{Provider: ProviderFeishu, Name: "first", AppID: "app"}
	if _, err := service.PutConnection(ctx, "connection-1", input); err != nil {
		t.Fatal(err)
	}
	input.Name = "second"
	if _, err := service.PutConnection(ctx, "connection-2", input); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("duplicate app_id error=%v", err)
	}
}

func TestRecoverClaimedMessageMarksDeliveryUnknown(t *testing.T) {
	ctx := context.Background()
	store, _ := Open(t.TempDir())
	defer store.Close()
	if _, err := store.PutConnection(ctx, Connection{ID: "connection-1", Provider: ProviderFeishu, Name: "test", AppID: "app"}); err != nil {
		t.Fatal(err)
	}
	message, err := store.RecordMessage(ctx, "connection-1", InboundMessage{MessageID: "claimed", ChatID: "allowed", ChatType: "group", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimMessage(ctx, message.ID); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if _, err := store.RecoverPendingMessages(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.GetMessageByProviderID(ctx, "connection-1", "claimed")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.TriggerStatus != "delivery_unknown" || recovered.StatusDetail != "daemon_restarted_after_possible_delivery" {
		t.Fatalf("message=%+v", recovered)
	}
}

func TestRecordMessagePrunesConversationHistory(t *testing.T) {
	ctx := context.Background()
	store, _ := Open(t.TempDir())
	defer store.Close()
	if _, err := store.PutConnection(ctx, Connection{ID: "connection-1", Provider: ProviderFeishu, Name: "test", AppID: "app"}); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 501; index++ {
		_, err := store.RecordMessage(ctx, "connection-1", InboundMessage{MessageID: fmt.Sprintf("message-%03d", index), ChatID: "allowed", RootID: "root-message", ChatType: "group", CreatedAt: time.Now().Add(time.Duration(index) * time.Millisecond)})
		if err != nil {
			t.Fatal(err)
		}
	}
	conversation, err := store.EnsureConversationRoot(ctx, "connection-1", "allowed", "root-message", "", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM channel_messages WHERE conversation_id=?`, conversation.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 500 {
		t.Fatalf("retained messages=%d", count)
	}
	old := time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := store.db.ExecContext(ctx, `UPDATE channel_messages SET created_at=?`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListMessages(ctx, conversation.ID, 100); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM channel_messages WHERE conversation_id=?`, conversation.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expired messages retained=%d", count)
	}
}

func TestDeleteConnectionCleansOrphanSecret(t *testing.T) {
	ctx := context.Background()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	if err := secrets.Set("orphan", "secret"); err != nil {
		t.Fatal(err)
	}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{&fakeAdapter{}}})
	if err := service.DeleteConnection(ctx, "orphan"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := secrets.Get("orphan"); err != nil || ok {
		t.Fatalf("orphan secret remains: ok=%v err=%v", ok, err)
	}
}

func TestConversationApprovalOverrideCannotDisableEffectiveReview(t *testing.T) {
	ctx := context.Background()
	store, _ := Open(t.TempDir())
	secrets, _ := NewSecretStore(t.TempDir())
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: fakeFactory{&fakeAdapter{}}})
	_, err := service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", ApprovalInstructions: "restrict", ReviewAllTools: true})
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := store.EnsureConversation(ctx, "connection-1", "allowed", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	_, err = service.PutConversation(ctx, conversation.ID, ConversationInput{ApprovalInstructionsMode: OverrideInherit, ReviewAllTools: &disabled})
	if err == nil {
		t.Fatal("expected inherited approval instructions to require review")
	}
	_, err = service.PutConversation(ctx, conversation.ID, ConversationInput{ApprovalInstructionsMode: OverrideReplace, ApprovalInstructions: "", ReviewAllTools: &disabled})
	if err != nil {
		t.Fatalf("explicitly empty approval override should allow ordinary safe-tool policy: %v", err)
	}
}
