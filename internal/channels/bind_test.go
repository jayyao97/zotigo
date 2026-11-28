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
	// Hold the next model or bind target resolution, the slow step a command
	// takes between reading the group and writing it; switchGate holds a
	// Session switch after the binding is held.
	modelGate, bindGate, switchGate *pauseGate
}

// pauseGate holds one call once armed, until the test releases it.
type pauseGate struct {
	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
}

func newPauseGate() *pauseGate {
	return &pauseGate{entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *pauseGate) arm() { g.mu.Lock(); g.armed = true; g.mu.Unlock() }

func (g *pauseGate) wait() {
	g.mu.Lock()
	armed := g.armed
	g.armed = false
	g.mu.Unlock()
	if armed {
		g.entered <- struct{}{}
		<-g.release
	}
}

func (bindingDispatcher) ChannelSessionRuntime(context.Context, string) (SessionRuntimeConfig, error) {
	return SessionRuntimeConfig{Agent: SessionAgentZotigo, ProfileName: "fast"}, nil
}

func (bindingDispatcher) ChannelRuntimeChoices(context.Context, string, string) ([]string, error) {
	return []string{"fast (model-a)", "deep (model-b)"}, nil
}

func (d bindingDispatcher) ResolveChannelRuntime(_ context.Context, _ string, current SessionRuntimeConfig, choice []string) (SessionRuntimeConfig, error) {
	d.modelGate.wait()
	switch choice[0] {
	case "missing":
		return current, NewChatError(nil, `profile "missing" not found`)
	case "broken":
		return current, errors.New("open /home/owner/.zotigo/config.yaml: permission denied")
	}
	return SessionRuntimeConfig{Agent: current.Agent, ProfileName: choice[0]}, nil
}

func (d bindingDispatcher) SwitchChannelSessionRuntime(_ context.Context, sessionID string, choice []string, hold func() (func(), error)) (SessionRuntimeConfig, bool, error) {
	d.modelGate.wait()
	release, err := hold()
	if err != nil {
		return SessionRuntimeConfig{}, false, err
	}
	defer release()
	d.switchGate.wait()
	d.switched <- sessionID + "=" + strings.Join(choice, " ")
	return SessionRuntimeConfig{Agent: SessionAgentZotigo, ProfileName: choice[0]}, false, nil
}

func (d bindingDispatcher) ResolveChannelBindTarget(_ context.Context, id string) (ChannelBindTarget, error) {
	d.bindGate.wait()
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
	dispatcher := bindingDispatcher{fakeDispatcher: fakeDispatcher{called: make(chan Task, 4)}, validated: make(chan Conversation, 4), switched: make(chan string, 4), modelGate: newPauseGate(), bindGate: newPauseGate(), switchGate: newPauseGate()}
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

func TestConnectionCOTDomain(t *testing.T) {
	service, _, _, _, ctx := startBindingService(t, nil)
	domain := "cot.example.org"
	input := ConnectionInput{Provider: ProviderFeishu, Name: "Bot", AppID: "app", COTDomain: &domain}
	for _, step := range []struct {
		value *string
		want  string
	}{{&domain, domain}, {nil, domain}, {new(string), ""}} {
		input.COTDomain = step.value
		saved, err := service.PutConnection(ctx, "connection-1", input)
		if err != nil || saved.COTDomain != step.want {
			t.Fatalf("domain %q: got %q, err %v", step.want, saved.COTDomain, err)
		}
	}
	domain = "https://cot.example.org/path"
	input.COTDomain = &domain
	if _, err := service.PutConnection(ctx, "connection-1", input); err == nil {
		t.Fatal("accepted URL instead of hostname")
	}
}

func TestBindAliasesAndProgressDetailAreValidated(t *testing.T) {
	service, _, _, _, ctx := startBindingService(t, nil)
	secret := "secret"
	for _, aliases := range [][]BindAlias{
		{{Name: "on call", Target: "workspace_a"}},
		// /bind splits words at any Unicode space, so these could never match.
		{{Name: "值班\u3000助手", Target: "workspace_a"}},
		{{Name: "on\u00a0call", Target: "workspace_a"}},
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
		// A public message without a cause has nothing more to log.
		if (logged.Len() != 0) != test.logsCause || test.logsCause && !strings.Contains(logged.String(), "config.yaml") {
			t.Errorf("%v: log = %q", test.err, logged.String())
		}
	}
}

// Every alias name that can be saved must reach /bind as one word.
func TestSavedAliasNamesAreReachableFromBind(t *testing.T) {
	aliases, err := normalizeBindAliases([]BindAlias{{Name: "值班助手", Target: "workspace_a"}, {Name: "on-call_2", Target: "sess_b"}})
	if err != nil {
		t.Fatal(err)
	}
	connection := Connection{OwnerSenderIDs: []string{"owner-1"}, BindAliases: aliases}
	for _, alias := range aliases {
		in := InboundMessage{ChatType: "group", MentionedBot: true, Sender: Sender{ID: "owner-1"}, Text: "/bind " + alias.Name}
		command, argument, ok := parseGroupCommand(connection, in)
		if !ok || command != commandBind || argument != alias.Name {
			t.Fatalf("%q parsed as %q %q %v", alias.Name, command, argument, ok)
		}
		if target, found := resolveBindTarget(connection, argument); !found || target != alias.Target {
			t.Fatalf("%q resolved to %q %v", alias.Name, target, found)
		}
	}
}

// desktopEdit changes the group as the Channel page would while a command waits.
func desktopEdit(t *testing.T, service *Service, store *Store, edit func(*ConversationInput)) {
	t.Helper()
	group, err := store.GetConversationByScope(context.Background(), "connection-1", "chat-1", "")
	if err != nil {
		t.Fatal(err)
	}
	input := conversationInputFrom(group)
	edit(&input)
	if _, err := service.PutConversation(context.Background(), group.ID, input); err != nil {
		t.Fatal(err)
	}
}

// A command reads the group, waits on a slow step, then writes. Edits made on
// the Channel page meanwhile must survive: a disabled group stays disabled and
// tightened send permissions stay tightened.
func TestGroupCommandsKeepEditsMadeWhileTheyRun(t *testing.T) {
	selectedOwner := func(input *ConversationInput) {
		policy := SenderPolicySelected
		input.SenderPolicy, input.AllowedSenderIDs = &policy, []string{"owner-1"}
	}
	disable := func(input *ConversationInput) { input.Enabled = false }
	for _, test := range []struct {
		name    string
		bound   bool // whether the group is bound before the command
		command string
		gate    func(bindingDispatcher) *pauseGate
		edit    func(*ConversationInput)
		reply   string
		check   func(Conversation) bool
	}{
		{"model after disable", true, "/model deep", func(d bindingDispatcher) *pauseGate { return d.modelGate }, disable,
			"unbound or disabled", func(g Conversation) bool { return !g.Enabled && g.ProfileName == "" }},
		{"model after tightening", true, "/model deep", func(d bindingDispatcher) *pauseGate { return d.modelGate }, selectedOwner,
			"now use profile deep", func(g Conversation) bool {
				return g.Enabled && g.ProfileName == "deep" && g.SenderPolicy == SenderPolicySelected && strings.Join(g.AllowedSenderIDs, ",") == "owner-1"
			}},
		{"rebind after disable", true, "/bind sess_shared", func(d bindingDispatcher) *pauseGate { return d.bindGate }, disable,
			"unbound or disabled", func(g Conversation) bool {
				return !g.Enabled && g.SessionID == "" && g.WorkspaceID == "workspace_oncall"
			}},
		{"bind after tightening", false, "/bind workspace_oncall", func(d bindingDispatcher) *pauseGate { return d.bindGate }, selectedOwner,
			"bound to Workspace workspace_oncall", func(g Conversation) bool {
				return g.Enabled && g.SenderPolicy == SenderPolicySelected && strings.Join(g.AllowedSenderIDs, ",") == "owner-1"
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, store, adapter, dispatcher, ctx := startBindingService(t, nil)
			replies := 0
			send := func(id, text string) string {
				t.Helper()
				if err := adapter.callbacks.Inbound(ctx, groupMessage(id, "owner-1", text, true)); err != nil {
					t.Fatal(err)
				}
				replies++
				return adapter.waitReply(t, replies)
			}
			if test.bound {
				send("bind", "/bind workspace_oncall")
				<-dispatcher.validated
				// Everyone may write before the edit, so tightening is visible.
				desktopEdit(t, service, store, func(input *ConversationInput) {
					policy := SenderPolicyAll
					input.SenderPolicy = &policy
				})
			} else {
				send("show", "/bind") // records the group
				desktopEdit(t, service, store, func(input *ConversationInput) {
					policy := SenderPolicyAll
					input.SenderPolicy = &policy
				})
			}

			gate := test.gate(dispatcher)
			gate.arm()
			if err := adapter.callbacks.Inbound(ctx, groupMessage("command", "owner-1", test.command, true)); err != nil {
				t.Fatal(err)
			}
			<-gate.entered
			desktopEdit(t, service, store, test.edit)
			close(gate.release)
			replies++
			if reply := adapter.waitReply(t, replies); !strings.Contains(reply, test.reply) {
				t.Fatalf("reply = %q", reply)
			}
			group, err := store.GetConversationByScope(ctx, "connection-1", "chat-1", "")
			if err != nil || !test.check(group) {
				t.Fatalf("group = %+v err=%v", group, err)
			}
		})
	}
}

// /model on a Session rechecks the binding once the choice is resolved and
// holds it through the switch, as the group-runtime path does through its write.
func TestModelOnSessionKeepsEditsMadeWhileItRuns(t *testing.T) {
	type setup struct {
		bind    string // command that binds the group
		message func() InboundMessage
		session string
	}
	shared := setup{"/bind sess_shared", func() InboundMessage { return groupMessage("model", "owner-1", "/model deep", true) }, "sess_shared"}
	topic := setup{"/bind workspace_oncall", func() InboundMessage {
		in := groupMessage("model", "owner-1", "/model deep", true)
		in.RootID = "root-1"
		return in
	}, "sess_topic"}
	for _, test := range []struct {
		name  string
		setup setup
		edit  func(*ConversationInput)
		reply string
	}{
		{"shared session after disable", shared, func(input *ConversationInput) { input.Enabled = false }, "unbound or disabled"},
		{"shared session after rebinding to another", shared, func(input *ConversationInput) { input.SessionID = "sess_other" }, "binding changed"},
		{"topic session after the group is disabled", topic, func(input *ConversationInput) { input.Enabled = false }, "unbound or disabled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, store, adapter, dispatcher, ctx := startBindingService(t, nil)
			if err := adapter.callbacks.Inbound(ctx, groupMessage("bind", "owner-1", test.setup.bind, true)); err != nil {
				t.Fatal(err)
			}
			adapter.waitReply(t, 1)
			<-dispatcher.validated
			if test.setup.session == "sess_topic" {
				bindTopicSession(t, store)
			}
			dispatcher.modelGate.arm()
			if err := adapter.callbacks.Inbound(ctx, test.setup.message()); err != nil {
				t.Fatal(err)
			}
			<-dispatcher.modelGate.entered
			desktopEdit(t, service, store, test.edit)
			close(dispatcher.modelGate.release)
			if reply := adapter.waitReply(t, 2); !strings.Contains(reply, test.reply) {
				t.Fatalf("reply = %q", reply)
			}
			select {
			case switched := <-dispatcher.switched:
				t.Fatalf("switched %q after the edit", switched)
			default:
			}
		})
	}

	// An edit that arrives while the switch runs waits for it, then applies.
	service, store, adapter, dispatcher, ctx := startBindingService(t, nil)
	if err := adapter.callbacks.Inbound(ctx, groupMessage("bind", "owner-1", "/bind sess_shared", true)); err != nil {
		t.Fatal(err)
	}
	adapter.waitReply(t, 1)
	<-dispatcher.validated
	dispatcher.switchGate.arm()
	if err := adapter.callbacks.Inbound(ctx, shared.message()); err != nil {
		t.Fatal(err)
	}
	<-dispatcher.switchGate.entered
	if service.configMu.TryLock() {
		service.configMu.Unlock()
		t.Fatal("the binding is not held during the switch")
	}
	group, err := store.GetConversationByScope(ctx, "connection-1", "chat-1", "")
	if err != nil {
		t.Fatal(err)
	}
	disabled := conversationInputFrom(group)
	disabled.Enabled = false
	edited := make(chan error, 1)
	go func() {
		_, err := service.PutConversation(context.Background(), group.ID, disabled)
		edited <- err
	}()
	close(dispatcher.switchGate.release)
	if switched := <-dispatcher.switched; switched != "sess_shared=deep" {
		t.Fatalf("switched %q", switched)
	}
	if err := <-edited; err != nil {
		t.Fatal(err)
	}
	if group, err := store.GetConversationByScope(ctx, "connection-1", "chat-1", ""); err != nil || group.Enabled {
		t.Fatalf("group = %+v err=%v", group, err)
	}
}

func bindTopicSession(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	topic, err := store.EnsureConversationRoot(ctx, "connection-1", "chat-1", "root-1", "", "group", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	topic.SessionID, topic.WorkspaceID, topic.Enabled = "sess_topic", "workspace_oncall", true
	if _, err = store.PutConversation(ctx, topic); err != nil {
		t.Fatal(err)
	}
}

// /unbind clears what it read, including a group disabled or a shared Session
// archived meanwhile, but keeps a binding that was replaced meanwhile. A shared
// Session the group gained meanwhile is kept whether an owner chose it or a
// mention created it; sending /unbind again then clears it.
func TestUnbindKeepsARebindMadeWhileItRuns(t *testing.T) {
	sharedWith := func(session string) func(*ConversationInput) {
		return func(input *ConversationInput) {
			input.SessionStrategy, input.SessionID = SessionStrategyShared, session
		}
	}
	// The Channel page saves a chosen Session with the binding validation.
	desktopBinds := func(session string) func(*testing.T, *Service, *Store, bindingDispatcher) {
		return func(t *testing.T, service *Service, store *Store, dispatcher bindingDispatcher) {
			group, err := store.GetConversationByScope(context.Background(), "connection-1", "chat-1", "")
			if err != nil {
				t.Fatal(err)
			}
			input := conversationInputFrom(group)
			sharedWith(session)(&input)
			if _, err := service.PutConversationValidated(context.Background(), group.ID, input, dispatcher.ValidateChannelBinding); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A mention records the Session it created on the group, as provisioning does.
	mentionCreates := func(session string) func(*testing.T, *Service, *Store, bindingDispatcher) {
		return func(t *testing.T, service *Service, store *Store, _ bindingDispatcher) {
			service.configMu.Lock()
			defer service.configMu.Unlock()
			group, err := store.GetConversationByScope(context.Background(), "connection-1", "chat-1", "")
			if err != nil {
				t.Fatal(err)
			}
			group.SessionID = session
			if _, err := store.PutConversation(context.Background(), group); err != nil {
				t.Fatal(err)
			}
		}
	}
	type change = func(*testing.T, *Service, *Store, bindingDispatcher)
	edit := func(apply func(*ConversationInput)) change {
		return func(t *testing.T, service *Service, store *Store, _ bindingDispatcher) {
			desktopEdit(t, service, store, apply)
		}
	}
	for _, test := range []struct {
		name    string
		before  func(*ConversationInput) // the group /unbind reads
		change  change                   // applied while it runs
		session string                   // the group's Session afterwards
		cleared bool
	}{
		{"topic group turned into one sharing a chosen Session", nil, edit(sharedWith("sess_b")), "sess_b", false},
		{"shared group switched to another Session", sharedWith("sess_a"), edit(sharedWith("sess_b")), "sess_b", false},
		{"shared group moved to another Workspace", sharedWith(""), edit(func(input *ConversationInput) { input.WorkspaceID = "workspace_other" }), "", false},
		{"shared group given a chosen Session on the Channel page", sharedWith(""), desktopBinds("sess_b"), "sess_b", false},
		{"shared Session created by a mention", sharedWith(""), mentionCreates("sess_b"), "sess_b", false},
		{"shared Session archived", sharedWith("sess_a"), edit(sharedWith("")), "", true},
		{"group disabled", nil, edit(func(input *ConversationInput) { input.Enabled = false }), "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, store, adapter, dispatcher, ctx := startBindingService(t, nil)
			if err := adapter.callbacks.Inbound(ctx, groupMessage("bind", "owner-1", "/bind workspace_oncall", true)); err != nil {
				t.Fatal(err)
			}
			adapter.waitReply(t, 1)
			<-dispatcher.validated
			if test.before != nil {
				desktopEdit(t, service, store, test.before)
			}
			gate := newPauseGate()
			gate.arm()
			service.beforeUnbindWrite = gate.wait
			if err := adapter.callbacks.Inbound(ctx, groupMessage("unbind", "owner-1", "/unbind", true)); err != nil {
				t.Fatal(err)
			}
			<-gate.entered
			test.change(t, service, store, dispatcher)
			close(gate.release)
			want := "is unbound"
			if !test.cleared {
				want = "binding changed while the command ran; send it again"
			}
			if reply := adapter.waitReply(t, 2); !strings.Contains(reply, want) {
				t.Fatalf("reply = %q", reply)
			}
			group, err := store.GetConversationByScope(ctx, "connection-1", "chat-1", "")
			if err != nil || (group.WorkspaceID == "" && !group.Enabled) != test.cleared || !test.cleared && (!group.Enabled || group.SessionID != test.session) {
				t.Fatalf("group = %+v err=%v", group, err)
			}
			if test.cleared {
				return
			}
			// Sent again, /unbind sees the new binding and clears it.
			if err := adapter.callbacks.Inbound(ctx, groupMessage("unbind-again", "owner-1", "/unbind", true)); err != nil {
				t.Fatal(err)
			}
			if reply := adapter.waitReply(t, 3); !strings.Contains(reply, "is unbound") {
				t.Fatalf("retry reply = %q", reply)
			}
			if group, err = store.GetConversationByScope(ctx, "connection-1", "chat-1", ""); err != nil || group.Enabled || group.WorkspaceID != "" || group.SessionID != "" {
				t.Fatalf("after retry group = %+v err=%v", group, err)
			}
		})
	}
}
