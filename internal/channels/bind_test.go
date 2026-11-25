package channels

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// replyingAdapter records command replies.
type replyingAdapter struct {
	*fakeAdapter
	replyMu sync.Mutex
	replies []string
}

func (a *replyingAdapter) ReplyCommand(_ context.Context, _ InboundMessage, text string) error {
	a.replyMu.Lock()
	defer a.replyMu.Unlock()
	a.replies = append(a.replies, text)
	return nil
}

func (a *replyingAdapter) waitReply(t *testing.T, count int) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a.replyMu.Lock()
		if len(a.replies) >= count {
			reply := a.replies[count-1]
			a.replyMu.Unlock()
			return reply
		}
		a.replyMu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no reply %d", count)
	return ""
}

func (a *replyingAdapter) replyCount() int {
	a.replyMu.Lock()
	defer a.replyMu.Unlock()
	return len(a.replies)
}

type replyingFactory struct{ adapter *replyingAdapter }

func (replyingFactory) Capabilities() AdapterCapabilities { return fakeFactory{}.Capabilities() }
func (f replyingFactory) New(connection Connection, secret string, callbacks AdapterCallbacks) (Adapter, error) {
	if _, err := (fakeFactory{f.adapter.fakeAdapter}).New(connection, secret, callbacks); err != nil {
		return nil, err
	}
	return f.adapter, nil
}

// bindingDispatcher resolves the test Workspace and Session, and records
// model switches.
type bindingDispatcher struct {
	fakeDispatcher
	validated chan Conversation
	switched  chan string
}

func (bindingDispatcher) ChannelSessionRuntime(context.Context, string) (SessionRuntimeConfig, error) {
	return SessionRuntimeConfig{Agent: SessionAgentZotigo, ProfileName: "fast"}, nil
}

func (bindingDispatcher) ChannelRuntimeChoices(context.Context, string, string) ([]string, error) {
	return []string{"fast (model-a)", "deep (model-b)"}, nil
}

func (bindingDispatcher) ResolveChannelRuntime(_ context.Context, _ string, current SessionRuntimeConfig, choice []string) (SessionRuntimeConfig, error) {
	switch choice[0] {
	case "missing":
		return current, NewChatError(nil, `profile "missing" not found`)
	case "broken":
		return current, errors.New("open /home/owner/.zotigo/config.yaml: permission denied")
	}
	return SessionRuntimeConfig{Agent: current.Agent, ProfileName: choice[0]}, nil
}

func (d bindingDispatcher) SwitchChannelSessionRuntime(_ context.Context, sessionID string, choice []string) (SessionRuntimeConfig, bool, error) {
	d.switched <- sessionID + "=" + strings.Join(choice, " ")
	return SessionRuntimeConfig{Agent: SessionAgentZotigo, ProfileName: choice[0]}, false, nil
}

func (bindingDispatcher) ResolveChannelBindTarget(_ context.Context, id string) (ChannelBindTarget, error) {
	switch id {
	case "workspace_oncall":
		return ChannelBindTarget{WorkspaceID: id}, nil
	case "sess_shared":
		return ChannelBindTarget{SessionID: id, WorkspaceID: "workspace_oncall", Runtime: SessionRuntimeConfig{Agent: SessionAgentZotigo, ProfileName: "fast"}}, nil
	}
	return ChannelBindTarget{}, NewChatError(nil, "%s was not found", id)
}

func (d bindingDispatcher) ValidateChannelBinding(_ context.Context, conversation Conversation, prompt SessionPromptConfig, runtime SessionRuntimeConfig) (SessionPromptConfig, SessionRuntimeConfig, error) {
	d.validated <- conversation
	return prompt, runtime, nil
}

func startBindingService(t *testing.T, aliases []BindAlias) (*Service, *Store, *replyingAdapter, bindingDispatcher, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secrets, _ := NewSecretStore(t.TempDir())
	adapter := &replyingAdapter{fakeAdapter: &fakeAdapter{}}
	service := NewService(store, secrets, log.New(io.Discard, "", 0), map[string]AdapterFactory{ProviderFeishu: replyingFactory{adapter}})
	dispatcher := bindingDispatcher{fakeDispatcher: fakeDispatcher{called: make(chan Task, 4)}, validated: make(chan Conversation, 4), switched: make(chan string, 4)}
	service.SetDispatcher(dispatcher)
	secret := "secret"
	owners := []string{"owner-1"}
	if _, err = service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, OwnerSenderIDs: &owners, BindAliases: &aliases}); err != nil {
		t.Fatal(err)
	}
	if err = service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return service, store, adapter, dispatcher, ctx
}

func groupMessage(id, sender, text string, mentioned bool) InboundMessage {
	return InboundMessage{MessageID: id, ChatID: "chat-1", ChatType: "group", ChatName: "Oncall room", Sender: Sender{ID: sender}, Text: text, MentionedBot: mentioned, CreatedAt: time.Now()}
}

func TestOwnerBindsUnboundGroupFromChat(t *testing.T) {
	service, store, adapter, dispatcher, ctx := startBindingService(t, []BindAlias{{Name: "Oncall", Target: "workspace_oncall"}})

	// Members and unmentioned commands stay ignored in an unbound group.
	for _, in := range []InboundMessage{
		groupMessage("member", "member-1", "/bind oncall", true),
		groupMessage("unmentioned", "owner-1", "/bind oncall", false),
	} {
		if err := adapter.callbacks.Inbound(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if adapter.replyCount() != 0 {
		t.Fatalf("ignored commands replied: %v", adapter.replies)
	}
	if _, err := store.GetMessageByProviderID(ctx, "connection-1", "member"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("member command was stored: %v", err)
	}

	if err := adapter.callbacks.Inbound(ctx, groupMessage("status", "owner-1", "/bind", true)); err != nil {
		t.Fatal(err)
	}
	if reply := adapter.waitReply(t, 1); !strings.Contains(reply, "not bound") || !strings.Contains(reply, "Oncall") {
		t.Fatalf("status reply = %q", reply)
	}

	// Aliases match without regard to case.
	if err := adapter.callbacks.Inbound(ctx, groupMessage("bind", "owner-1", "/bind ONCALL", true)); err != nil {
		t.Fatal(err)
	}
	if reply := adapter.waitReply(t, 2); !strings.Contains(reply, "Workspace workspace_oncall") {
		t.Fatalf("bind reply = %q", reply)
	}
	if validated := <-dispatcher.validated; validated.WorkspaceID != "workspace_oncall" {
		t.Fatalf("validated %+v", validated)
	}
	group, err := store.GetConversationByScope(ctx, "connection-1", "chat-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if !group.Enabled || group.WorkspaceID != "workspace_oncall" || group.SessionStrategy != SessionStrategyTopic || group.SenderPolicy != SenderPolicyOwners || group.ChatName != "Oncall room" {
		t.Fatalf("group = %+v", group)
	}

	// The bound group now admits ordinary owner messages.
	if err := adapter.callbacks.Inbound(ctx, groupMessage("question", "owner-1", "what broke?", true)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.store.GetMessageByProviderID(ctx, "connection-1", "question"); err != nil {
		t.Fatalf("bound group message was not stored: %v", err)
	}

	if err := adapter.callbacks.Inbound(ctx, groupMessage("unbind", "owner-1", "/unbind", true)); err != nil {
		t.Fatal(err)
	}
	if reply := adapter.waitReply(t, 3); !strings.Contains(reply, "unbound") {
		t.Fatalf("unbind reply = %q", reply)
	}
	group, err = store.GetConversationByScope(ctx, "connection-1", "chat-1", "")
	if err != nil || group.Enabled || group.WorkspaceID != "" {
		t.Fatalf("unbound group = %+v err=%v", group, err)
	}
}

func TestOwnerBindsGroupToSharedSession(t *testing.T) {
	_, store, adapter, dispatcher, ctx := startBindingService(t, nil)
	if err := adapter.callbacks.Inbound(ctx, groupMessage("unknown", "owner-1", "/bind nowhere", true)); err != nil {
		t.Fatal(err)
	}
	if reply := adapter.waitReply(t, 1); !strings.Contains(reply, "not a Session ID, Workspace ID or alias") {
		t.Fatalf("unknown target reply = %q", reply)
	}
	if err := adapter.callbacks.Inbound(ctx, groupMessage("shared", "owner-1", "/bind sess_shared", true)); err != nil {
		t.Fatal(err)
	}
	if reply := adapter.waitReply(t, 2); !strings.Contains(reply, "shares Session sess_shared") {
		t.Fatalf("shared reply = %q", reply)
	}
	<-dispatcher.validated
	group, err := store.GetConversationByScope(ctx, "connection-1", "chat-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if group.SessionStrategy != SessionStrategyShared || group.SessionID != "sess_shared" || group.WorkspaceID != "workspace_oncall" || group.ProfileName != "fast" {
		t.Fatalf("group = %+v", group)
	}
}

func TestBindAliasesAndProgressDetailAreValidated(t *testing.T) {
	service, _, _, _, ctx := startBindingService(t, nil)
	secret := "secret"
	for _, aliases := range [][]BindAlias{
		{{Name: "on call", Target: "workspace_a"}},
		{{Name: "oncall", Target: "somewhere"}},
		{{Name: "sess_x", Target: "workspace_a"}},
		{{Name: "oncall", Target: "workspace_a"}, {Name: "OnCall", Target: "sess_b"}},
	} {
		if _, err := service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, BindAliases: &aliases}); err == nil {
			t.Fatalf("accepted aliases %+v", aliases)
		}
	}
	if _, err := service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, ProgressDetail: "verbose"}); err == nil {
		t.Fatal("accepted an unknown progress detail")
	}
	connection, err := service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, ProgressDetail: ProgressDetailFull})
	if err != nil || connection.ProgressDetail != ProgressDetailFull {
		t.Fatalf("connection=%+v err=%v", connection, err)
	}
	// Omitted fields keep their saved values.
	connection, err = service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true})
	if err != nil || connection.ProgressDetail != ProgressDetailFull || len(connection.BindAliases) != 0 {
		t.Fatalf("connection=%+v err=%v", connection, err)
	}
}

func TestGroupProgressDetailOverridesConnection(t *testing.T) {
	service, store, adapter, dispatcher, ctx := startBindingService(t, nil)
	secret := "secret"
	if _, err := service.PutConnection(ctx, "connection-1", ConnectionInput{Provider: ProviderFeishu, Name: "test", AppID: "app", AppSecret: &secret, Enabled: true, ProgressDetail: ProgressDetailDetailed}); err != nil {
		t.Fatal(err)
	}
	group := bindTestGroup(t, store, "chat-1", "owner-1")
	conversation, err := store.EnsureConversationRoot(ctx, "connection-1", "chat-1", "root-1", "", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	conversation.SessionID, conversation.WorkspaceID, conversation.Enabled = "session-1", "workspace-1", true
	if _, err = store.PutConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	send := func(id string) Task {
		t.Helper()
		in := InboundMessage{MessageID: id, ChatID: "chat-1", ChatType: "group", RootID: "root-1", Sender: Sender{ID: "owner-1"}, Text: "hi", MentionedBot: true, CreatedAt: time.Now()}
		if err := adapter.callbacks.Inbound(ctx, in); err != nil {
			t.Fatal(err)
		}
		select {
		case task := <-dispatcher.called:
			return task
		case <-time.After(5 * time.Second):
			t.Fatal("task was not dispatched")
			return Task{}
		}
	}
	if task := send("first"); task.ProgressDetail != ProgressDetailDetailed {
		t.Fatalf("inherited detail = %q", task.ProgressDetail)
	}
	full := ProgressDetailFull
	input := conversationInputFrom(group)
	input.ProgressDetail = &full
	if _, err = service.PutConversation(ctx, group.ID, input); err != nil {
		t.Fatal(err)
	}
	if task := send("second"); task.ProgressDetail != ProgressDetailFull {
		t.Fatalf("overridden detail = %q", task.ProgressDetail)
	}
	invalid := "verbose"
	input.ProgressDetail = &invalid
	if _, err = service.PutConversation(ctx, group.ID, input); err == nil {
		t.Fatal("accepted an unknown group progress detail")
	}
}

func TestGroupCommandsRunOnceInArrivalOrder(t *testing.T) {
	_, store, adapter, dispatcher, ctx := startBindingService(t, nil)
	for _, in := range []InboundMessage{
		groupMessage("bind", "owner-1", "/bind workspace_oncall", true),
		groupMessage("bind", "owner-1", "/bind workspace_oncall", true),
		groupMessage("unbind", "owner-1", "/unbind", true),
	} {
		if err := adapter.callbacks.Inbound(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	<-dispatcher.validated
	if reply := adapter.waitReply(t, 2); !strings.Contains(reply, "unbound") {
		t.Fatalf("second reply = %q", reply)
	}
	time.Sleep(50 * time.Millisecond)
	if count := adapter.replyCount(); count != 2 {
		t.Fatalf("a redelivered command ran again: %d replies %v", count, adapter.replies)
	}
	group, err := store.GetConversationByScope(ctx, "connection-1", "chat-1", "")
	if err != nil || group.Enabled {
		t.Fatalf("the later /unbind did not win: %+v err=%v", group, err)
	}
}

func TestModelCommandTargetsTheMessageScope(t *testing.T) {
	_, store, adapter, dispatcher, ctx := startBindingService(t, nil)
	send := func(in InboundMessage, count int) string {
		t.Helper()
		if err := adapter.callbacks.Inbound(ctx, in); err != nil {
			t.Fatal(err)
		}
		return adapter.waitReply(t, count)
	}
	if reply := send(groupMessage("unbound", "owner-1", "/model", true), 1); !strings.Contains(reply, "not bound") {
		t.Fatalf("unbound reply = %q", reply)
	}
	send(groupMessage("bind", "owner-1", "/bind workspace_oncall", true), 2)
	<-dispatcher.validated

	// A top-level message lists choices and then changes the model for new topics.
	if reply := send(groupMessage("list", "owner-1", "/model", true), 3); !strings.Contains(reply, "new topics") || !strings.Contains(reply, "deep (model-b)") {
		t.Fatalf("list reply = %q", reply)
	}
	if reply := send(groupMessage("default", "owner-1", "/model deep", true), 4); !strings.Contains(reply, "profile deep") {
		t.Fatalf("default reply = %q", reply)
	}
	<-dispatcher.validated
	group, err := store.GetConversationByScope(ctx, "connection-1", "chat-1", "")
	if err != nil || group.ProfileName != "deep" {
		t.Fatalf("group = %+v err=%v", group, err)
	}
	if reply := send(groupMessage("missing", "owner-1", "/model missing", true), 5); !strings.Contains(reply, "not found") {
		t.Fatalf("missing reply = %q", reply)
	}
	// Other members read the replies, so internal failures stay in the log.
	if reply := send(groupMessage("broken", "owner-1", "/model broken", true), 6); strings.Contains(reply, "config.yaml") || !strings.Contains(reply, "daemon log") {
		t.Fatalf("broken reply = %q", reply)
	}

	// A reply in a topic changes that topic's Session.
	topic, err := store.EnsureConversationRoot(ctx, "connection-1", "chat-1", "root-1", "", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	topic.SessionID, topic.WorkspaceID, topic.Enabled = "sess_topic", "workspace_oncall", true
	if _, err = store.PutConversation(ctx, topic); err != nil {
		t.Fatal(err)
	}
	reply := groupMessage("topic", "owner-1", "/model fast", true)
	reply.RootID = "root-1"
	if text := send(reply, 7); !strings.Contains(text, "This topic's session now uses profile fast") {
		t.Fatalf("topic reply = %q", text)
	}
	if switched := <-dispatcher.switched; switched != "sess_topic=fast" {
		t.Fatalf("switched %q", switched)
	}
	orphan := groupMessage("orphan", "owner-1", "/model fast", true)
	orphan.RootID = "root-2"
	if text := send(orphan, 8); !strings.Contains(text, "no session yet") {
		t.Fatalf("orphan reply = %q", text)
	}

	// A member's /model is an ordinary message.
	if err := adapter.callbacks.Inbound(ctx, groupMessage("member", "member-1", "/model fast", true)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if adapter.replyCount() != 8 {
		t.Fatalf("member command replied: %v", adapter.replies)
	}
}

func TestModelCommandSwitchesSharedSession(t *testing.T) {
	_, _, adapter, dispatcher, ctx := startBindingService(t, nil)
	if err := adapter.callbacks.Inbound(ctx, groupMessage("bind", "owner-1", "/bind sess_shared", true)); err != nil {
		t.Fatal(err)
	}
	adapter.waitReply(t, 1)
	<-dispatcher.validated
	if err := adapter.callbacks.Inbound(ctx, groupMessage("model", "owner-1", "/model deep", true)); err != nil {
		t.Fatal(err)
	}
	if reply := adapter.waitReply(t, 2); !strings.Contains(reply, "shared session now uses profile deep") {
		t.Fatalf("reply = %q", reply)
	}
	if switched := <-dispatcher.switched; switched != "sess_shared=deep" {
		t.Fatalf("switched %q", switched)
	}
}

// A shared group whose Session is not created yet keeps the choice for it.
func TestModelCommandConfiguresSharedGroupWithoutSession(t *testing.T) {
	_, store, adapter, dispatcher, ctx := startBindingService(t, nil)
	if err := adapter.callbacks.Inbound(ctx, groupMessage("bind", "owner-1", "/bind workspace_oncall", true)); err != nil {
		t.Fatal(err)
	}
	adapter.waitReply(t, 1)
	<-dispatcher.validated
	group, err := store.GetConversationByScope(ctx, "connection-1", "chat-1", "")
	if err != nil {
		t.Fatal(err)
	}
	group.SessionStrategy = SessionStrategyShared
	if _, err = store.PutConversation(ctx, group); err != nil {
		t.Fatal(err)
	}
	if err := adapter.callbacks.Inbound(ctx, groupMessage("list", "owner-1", "/model", true)); err != nil {
		t.Fatal(err)
	}
	if reply := adapter.waitReply(t, 2); !strings.Contains(reply, "created on the next mention") {
		t.Fatalf("list reply = %q", reply)
	}
	if err := adapter.callbacks.Inbound(ctx, groupMessage("model", "owner-1", "/model deep", true)); err != nil {
		t.Fatal(err)
	}
	if reply := adapter.waitReply(t, 3); !strings.Contains(reply, "shared session will use profile deep when the next mention creates it") {
		t.Fatalf("reply = %q", reply)
	}
	<-dispatcher.validated
	if group, err = store.GetConversationByScope(ctx, "connection-1", "chat-1", ""); err != nil || group.ProfileName != "deep" || group.SessionID != "" {
		t.Fatalf("group = %+v err=%v", group, err)
	}
	select {
	case switched := <-dispatcher.switched:
		t.Fatalf("switched a session: %q", switched)
	default:
	}
}

// Other members read command replies: they see a ChatError's public part only,
// and the log keeps every cause.
func TestCommandFailureShowsOnlyPublicMessages(t *testing.T) {
	var logged strings.Builder
	s := &Service{logger: log.New(&logged, "", 0)}
	cause := errors.New("open /home/owner/.zotigo/config.yaml: permission denied")
	for _, test := range []struct {
		err       error
		reply     string
		logsCause bool
	}{
		{NewChatError(nil, "session sess_1 was not found"), "Zotigo could not bind this group: session sess_1 was not found.", false},
		{fmt.Errorf("bind: %w", NewChatError(cause, "the Workspace has no usable default profile")), "Zotigo could not bind this group: the Workspace has no usable default profile.", true},
		{cause, "Zotigo could not bind this group. The daemon log has the details.", true},
	} {
		logged.Reset()
		if reply := s.commandFailure("connection-1", "bind this group", test.err); reply != test.reply {
			t.Errorf("reply = %q, want %q", reply, test.reply)
		}
		if strings.Contains(logged.String(), "config.yaml") != test.logsCause {
			t.Errorf("%v: log = %q", test.err, logged.String())
		}
	}
}
