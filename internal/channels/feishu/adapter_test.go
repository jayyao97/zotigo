package feishu

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/internal/channels"
	channeltypes "github.com/larksuite/oapi-sdk-go/v3/channel/types"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func stringPointer(value string) *string { return &value }

func messageEvent(chatID, senderID, messageType, content string, mentions []*larkim.MentionEvent) *larkim.P2MessageReceiveV1 {
	return &larkim.P2MessageReceiveV1{
		EventV2Base: &larkevent.EventV2Base{Header: &larkevent.EventHeader{EventID: "event-1", CreateTime: "1700000000000"}},
		Event: &larkim.P2MessageReceiveV1Data{
			Sender:  &larkim.EventSender{SenderId: &larkim.UserId{OpenId: stringPointer(senderID)}},
			Message: &larkim.EventMessage{MessageId: stringPointer("message-1"), RootId: stringPointer("root-1"), ChatId: stringPointer(chatID), ChatType: stringPointer("group"), ThreadId: stringPointer("thread-1"), MessageType: stringPointer(messageType), Content: stringPointer(content), Mentions: mentions},
		},
	}
}

func TestGroupFromListChatExcludesPrivateChats(t *testing.T) {
	for _, test := range []struct {
		mode string
		want bool
	}{
		{mode: "group", want: true},
		{mode: "topic", want: true},
		{mode: "p2p", want: false},
		{mode: "unknown", want: false},
	} {
		item := &larkim.ListChat{ChatId: stringPointer("oc_test"), ChatMode: stringPointer(test.mode), Name: stringPointer("Test")}
		group, ok := groupFromListChat(item)
		if ok != test.want {
			t.Fatalf("mode %q accepted=%v", test.mode, ok)
		}
		if ok && (group.ChatID != "oc_test" || group.ChatMode != test.mode || group.Name != "Test") {
			t.Fatalf("mode %q group=%+v", test.mode, group)
		}
	}
	if _, ok := groupFromListChat(&larkim.ListChat{ChatId: stringPointer("oc_test")}); ok {
		t.Fatal("chat with missing mode was accepted")
	}
}

func TestReceiveForwardsGroupMetadataAndFiltersUnsupportedMessages(t *testing.T) {
	var received []channels.InboundMessage
	adapter := &Adapter{
		connection: channels.Connection{AllowChatIDs: []string{"allowed"}},
		callbacks: channels.AdapterCallbacks{Inbound: func(_ context.Context, message channels.InboundMessage) error {
			received = append(received, message)
			return nil
		}},
		bot: channeltypes.BotIdentity{OpenID: "bot-open-id"},
	}
	mention := &larkim.MentionEvent{Key: stringPointer("@_user_1"), Id: &larkim.UserId{OpenId: stringPointer("bot-open-id")}}
	if err := adapter.receive(context.Background(), messageEvent("allowed", "user-open-id", "text", `{"text":"@_user_1 run tests"}`, []*larkim.MentionEvent{mention})); err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 {
		t.Fatalf("received=%+v", received)
	}
	message := received[0]
	if message.ChatID != "allowed" || message.RootID != "root-1" || message.Sender.ID != "user-open-id" || message.ThreadID != "thread-1" || message.ConversationName != "run tests" || !message.MentionedBot || message.Text != "run tests" || message.EventID != "event-1" || message.ConversationKey != "allowed\x00root-1" || message.StartsConversation || !message.TriggerAllowed {
		t.Fatalf("message=%+v", message)
	}

	otherMention := &larkim.MentionEvent{Key: stringPointer("@_user_2"), Id: &larkim.UserId{OpenId: stringPointer("other-open-id")}}
	for _, event := range []*larkim.P2MessageReceiveV1{
		messageEvent("outside", "user-open-id", "text", `{"text":"private"}`, nil),
		messageEvent("allowed", "user-open-id", "file", `{"file_key":"file"}`, nil),
		messageEvent("allowed", "bot-open-id", "text", `{"text":"self"}`, nil),
	} {
		if err := adapter.receive(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	if len(received) != 2 || received[1].ChatID != "outside" || received[1].Text != "private" {
		t.Fatalf("filtered messages reached callback: %+v", received)
	}
	if err := adapter.receive(context.Background(), messageEvent("allowed", "user-open-id", "text", `{"text":"@_user_2 hello"}`, []*larkim.MentionEvent{otherMention})); err != nil {
		t.Fatal(err)
	}
	if len(received) != 3 || received[2].MentionedBot {
		t.Fatalf("other-bot mention was attributed to current bot: %+v", received)
	}
}

func TestReceiveForwardsStandaloneImageInExistingTopic(t *testing.T) {
	var received channels.InboundMessage
	adapter := &Adapter{
		callbacks: channels.AdapterCallbacks{Inbound: func(_ context.Context, message channels.InboundMessage) error {
			received = message
			return nil
		}},
		bot: channeltypes.BotIdentity{OpenID: "bot-open-id"},
		downloadMessageImage: func(_ context.Context, messageID, imageKey string) ([]byte, error) {
			if messageID != "message-1" || imageKey != "img-one" {
				t.Fatalf("message=%q image=%q", messageID, imageKey)
			}
			return []byte("image bytes"), nil
		},
	}
	event := messageEvent("allowed", "user-open-id", "image", `{"image_key":"img-one"}`, nil)
	if err := adapter.receive(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if received.Text != "" || len(received.Images) != 1 || received.Images[0].ProviderKey != "message-1\x00img-one" || received.StartsConversation || !received.TriggerAllowed {
		t.Fatalf("received=%+v", received)
	}
}

func TestReceiveParsesPostTextAndImages(t *testing.T) {
	var received channels.InboundMessage
	var downloaded []string
	adapter := &Adapter{
		callbacks: channels.AdapterCallbacks{Inbound: func(_ context.Context, message channels.InboundMessage) error {
			received = message
			return nil
		}},
		bot: channeltypes.BotIdentity{OpenID: "bot-open-id"},
		downloadMessageImage: func(_ context.Context, messageID, imageKey string) ([]byte, error) {
			downloaded = append(downloaded, messageID+":"+imageKey)
			return []byte("image bytes"), nil
		},
	}
	content := `{"title":"Question","content":[[{"tag":"at","user_id":"bot-open-id","user_name":"Shadow"},{"tag":"text","text":" 里面是什么？"},{"tag":"img","image_key":"img-one"}],[{"tag":"text","text":"第二行"}]]}`
	event := messageEvent("allowed", "user-open-id", "post", content, nil)
	if err := adapter.receive(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if received.Text != "Question\n里面是什么？\n第二行" || !received.MentionedBot {
		t.Fatalf("received=%+v", received)
	}
	if len(received.Images) != 1 || received.Images[0].ProviderKey != "message-1\x00img-one" || len(received.Images[0].Data) != 0 || len(downloaded) != 0 {
		t.Fatalf("downloads=%v images=%+v", downloaded, received.Images)
	}
	resolved, err := adapter.ResolveInboundImages(context.Background(), received.Images)
	if err != nil || !slices.Equal(downloaded, []string{"message-1:img-one"}) || len(resolved) != 1 || string(resolved[0].Data) != "image bytes" || resolved[0].ProviderKey != "" {
		t.Fatalf("resolved=%+v downloads=%v err=%v", resolved, downloaded, err)
	}
}

func TestReceiveParsesContentV2Markdown(t *testing.T) {
	var received channels.InboundMessage
	adapter := &Adapter{
		callbacks: channels.AdapterCallbacks{Inbound: func(_ context.Context, message channels.InboundMessage) error {
			received = message
			return nil
		}},
		bot: channeltypes.BotIdentity{OpenID: "bot-open-id"},
	}
	content := `{"zh_cn":{"title":"","content":[[{"tag":"text","text":"legacy"}]],"content_v2":[[{"tag":"md","text":"<at user_id=\"bot-open-id\">Shadow</at> 看这张图 ![image](img-v2)"}]]}}`
	if err := adapter.receive(context.Background(), messageEvent("topic-chat", "user-open-id", "post", content, nil)); err != nil {
		t.Fatal(err)
	}
	if received.Text != "看这张图" || !received.MentionedBot || len(received.Images) != 1 || received.Images[0].ProviderKey != "message-1\x00img-v2" {
		t.Fatalf("received=%+v", received)
	}
}

func TestReceiveParsesLocalizedPostInTopicGroup(t *testing.T) {
	var received channels.InboundMessage
	adapter := &Adapter{
		callbacks: channels.AdapterCallbacks{Inbound: func(_ context.Context, message channels.InboundMessage) error {
			received = message
			return nil
		}},
		bot: channeltypes.BotIdentity{OpenID: "bot-open-id"},
	}
	content := `{"zh_cn":{"title":"","content":[[{"tag":"at","user_id":"bot-open-id","user_name":"Shadow"},{"tag":"text","text":" 今天星期几了？"}]]}}`
	event := messageEvent("topic-chat", "user-open-id", "post", content, nil)
	event.Event.Message.MessageId = stringPointer("topic-root")
	event.Event.Message.RootId = nil
	event.Event.Message.ThreadId = stringPointer("thread-1")
	if err := adapter.receive(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if received.Text != "今天星期几了？" || received.RootID != "topic-root" || received.ConversationKey != "topic-chat\x00topic-root" || !received.StartsConversation || !received.TriggerAllowed || !received.MentionedBot {
		t.Fatalf("received=%+v", received)
	}
}

func TestTopicDisplayNameIsSingleLineAndBounded(t *testing.T) {
	name := topicDisplayName("  first\nsecond   " + strings.Repeat("界", 60))
	if !strings.HasPrefix(name, "first second ") || !strings.HasSuffix(name, "…") || len([]rune(name)) != 49 {
		t.Fatalf("name=%q runes=%d", name, len([]rune(name)))
	}
}

func TestReceiveResolvesAndCachesSenderDisplayName(t *testing.T) {
	var received []channels.InboundMessage
	lookups := 0
	adapter := &Adapter{
		connection: channels.Connection{AllowChatIDs: []string{"allowed"}},
		callbacks: channels.AdapterCallbacks{Inbound: func(_ context.Context, message channels.InboundMessage) error {
			received = append(received, message)
			return nil
		}},
		bot: channeltypes.BotIdentity{OpenID: "bot-open-id"},
		lookupMessageDetails: func(_ context.Context, _, senderID string) (messageDetails, error) {
			lookups++
			if senderID != "user-open-id" {
				t.Fatalf("sender=%q", senderID)
			}
			return messageDetails{senderName: "姚填佳", threadID: "thread-1"}, nil
		},
	}
	for range 2 {
		if err := adapter.receive(context.Background(), messageEvent("allowed", "user-open-id", "text", `{"text":"hello"}`, nil)); err != nil {
			t.Fatal(err)
		}
	}
	if lookups != 1 || len(received) != 2 {
		t.Fatalf("lookups=%d received=%d", lookups, len(received))
	}
	for _, message := range received {
		if message.Sender.DisplayName != "姚填佳" {
			t.Fatalf("sender=%+v", message.Sender)
		}
	}
}

func TestReceiveResolvesAndCachesContainingChatName(t *testing.T) {
	var received []channels.InboundMessage
	lookups := 0
	adapter := &Adapter{
		connection: channels.Connection{AllowChatIDs: []string{"allowed"}},
		callbacks: channels.AdapterCallbacks{Inbound: func(_ context.Context, message channels.InboundMessage) error {
			received = append(received, message)
			return nil
		}},
		bot: channeltypes.BotIdentity{OpenID: "bot-open-id"},
		lookupChatName: func(_ context.Context, chatID string) (string, error) {
			lookups++
			if chatID != "allowed" {
				t.Fatalf("chat=%q", chatID)
			}
			if lookups == 1 {
				return "Shadow Test", nil
			}
			return "Renamed Shadow Test", nil
		},
	}
	for range 2 {
		if err := adapter.receive(context.Background(), messageEvent("allowed", "user-open-id", "text", `{"text":"hello"}`, nil)); err != nil {
			t.Fatal(err)
		}
	}
	if lookups != 1 || len(received) != 2 {
		t.Fatalf("lookups=%d received=%d", lookups, len(received))
	}
	for _, message := range received {
		if message.ChatName != "Shadow Test" {
			t.Fatalf("message=%+v", message)
		}
	}
	adapter.mu.Lock()
	cached := adapter.chatNames["allowed"]
	cached.expiresAt = time.Now().Add(-time.Second)
	adapter.chatNames["allowed"] = cached
	adapter.mu.Unlock()
	if err := adapter.receive(context.Background(), messageEvent("allowed", "user-open-id", "text", `{"text":"hello"}`, nil)); err != nil {
		t.Fatal(err)
	}
	if lookups != 2 || received[2].ChatName != "Renamed Shadow Test" {
		t.Fatalf("lookups=%d message=%+v", lookups, received[2])
	}
}

func TestReceiveUsesTopLevelMessageAsConversationRoot(t *testing.T) {
	var received []channels.InboundMessage
	lookups := 0
	adapter := &Adapter{
		connection: channels.Connection{AllowChatIDs: []string{"allowed"}},
		callbacks: channels.AdapterCallbacks{Inbound: func(_ context.Context, message channels.InboundMessage) error {
			received = append(received, message)
			return nil
		}},
		bot: channeltypes.BotIdentity{OpenID: "bot-open-id"},
		lookupMessageDetails: func(_ context.Context, messageID, _ string) (messageDetails, error) {
			lookups++
			return messageDetails{senderName: "Owner"}, nil
		},
	}
	for index, messageID := range []string{"one", "two"} {
		event := messageEvent("allowed", "user-open-id", "text", `{"text":"hello"}`, nil)
		event.Event.Message.MessageId = stringPointer(messageID)
		event.Event.Message.RootId = nil
		event.Event.Message.ThreadId = nil
		if err := adapter.receive(context.Background(), event); err != nil {
			t.Fatal(err)
		}
		if received[index].RootID != messageID || received[index].ThreadID != "" || received[index].Sender.DisplayName != "Owner" || received[index].ConversationKey != "allowed\x00"+messageID || !received[index].StartsConversation || received[index].TriggerAllowed {
			t.Fatalf("received=%+v", received[index])
		}
	}
	if lookups != 1 {
		t.Fatalf("sender name lookup was not cached: %d", lookups)
	}
}

func TestReceiveUsesRootIDForTopicReply(t *testing.T) {
	var received channels.InboundMessage
	adapter := &Adapter{
		connection: channels.Connection{AllowChatIDs: []string{"allowed"}},
		callbacks: channels.AdapterCallbacks{Inbound: func(_ context.Context, message channels.InboundMessage) error {
			received = message
			return nil
		}},
		bot: channeltypes.BotIdentity{OpenID: "bot-open-id"},
		lookupMessageDetails: func(context.Context, string, string) (messageDetails, error) {
			return messageDetails{senderName: "Owner"}, nil
		},
	}
	event := messageEvent("allowed", "user-open-id", "text", `{"text":"follow-up"}`, nil)
	event.Event.Message.MessageId = stringPointer("reply-1")
	event.Event.Message.RootId = stringPointer("root-message")
	event.Event.Message.ThreadId = stringPointer("thread-1")
	if err := adapter.receive(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if received.RootID != "root-message" || received.ThreadID != "thread-1" || received.ConversationKey != "allowed\x00root-message" || received.StartsConversation || !received.TriggerAllowed {
		t.Fatalf("received=%+v", received)
	}
}

func TestReceiveFailsClosedWhenMissingThreadScopeCannotBeResolved(t *testing.T) {
	callbacks := 0
	adapter := &Adapter{
		connection: channels.Connection{AllowChatIDs: []string{"allowed"}},
		callbacks: channels.AdapterCallbacks{Inbound: func(context.Context, channels.InboundMessage) error {
			callbacks++
			return nil
		}},
		bot: channeltypes.BotIdentity{OpenID: "bot-open-id"},
		lookupMessageDetails: func(context.Context, string, string) (messageDetails, error) {
			return messageDetails{}, errors.New("unavailable")
		},
	}
	event := messageEvent("allowed", "user-open-id", "text", `{"text":"hello"}`, nil)
	event.Event.Message.RootId = nil
	event.Event.Message.ThreadId = nil
	event.Event.Message.ParentId = stringPointer("parent-1")
	if err := adapter.receive(context.Background(), event); err == nil {
		t.Fatal("expected ambiguous message scope to fail closed")
	}
	if callbacks != 0 {
		t.Fatalf("ambiguous message reached service callback %d times", callbacks)
	}
}

func TestReceiveFailsClosedWhenMessageDetailsOmitReplyRoot(t *testing.T) {
	callbacks := 0
	adapter := &Adapter{
		connection: channels.Connection{AllowChatIDs: []string{"allowed"}},
		callbacks: channels.AdapterCallbacks{Inbound: func(context.Context, channels.InboundMessage) error {
			callbacks++
			return nil
		}},
		bot: channeltypes.BotIdentity{OpenID: "bot-open-id"},
		lookupMessageDetails: func(context.Context, string, string) (messageDetails, error) {
			return messageDetails{}, nil
		},
	}
	event := messageEvent("allowed", "user-open-id", "text", `{"text":"hello"}`, nil)
	event.Event.Message.RootId = nil
	event.Event.Message.ThreadId = stringPointer("thread-1")
	event.Event.Message.ParentId = stringPointer("parent-1")
	if err := adapter.receive(context.Background(), event); err == nil {
		t.Fatal("expected reply without a resolvable root to fail closed")
	}
	if callbacks != 0 {
		t.Fatalf("ambiguous reply reached service callback %d times", callbacks)
	}
}

func TestReceiveAcceptsTopLevelMessageWithThreadIDWhenLookupFails(t *testing.T) {
	var received channels.InboundMessage
	adapter := &Adapter{
		connection: channels.Connection{AllowChatIDs: []string{"allowed"}},
		callbacks: channels.AdapterCallbacks{Inbound: func(_ context.Context, message channels.InboundMessage) error {
			received = message
			return nil
		}},
		bot: channeltypes.BotIdentity{OpenID: "bot-open-id"},
		lookupMessageDetails: func(context.Context, string, string) (messageDetails, error) {
			return messageDetails{}, errors.New("unavailable")
		},
	}
	event := messageEvent("allowed", "user-open-id", "text", `{"text":"hello"}`, nil)
	event.Event.Message.MessageId = stringPointer("root-message")
	event.Event.Message.RootId = nil
	event.Event.Message.ParentId = nil
	event.Event.Message.ThreadId = stringPointer("thread-1")
	if err := adapter.receive(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if received.RootID != "root-message" || received.ThreadID != "thread-1" {
		t.Fatalf("received=%+v", received)
	}
}

func TestReceiveFallsBackToSenderIDWhenNameLookupFails(t *testing.T) {
	var received channels.InboundMessage
	lookups := 0
	adapter := &Adapter{
		connection: channels.Connection{AllowChatIDs: []string{"allowed"}},
		callbacks: channels.AdapterCallbacks{Inbound: func(_ context.Context, message channels.InboundMessage) error {
			received = message
			return nil
		}},
		bot: channeltypes.BotIdentity{OpenID: "bot-open-id"},
		lookupMessageDetails: func(context.Context, string, string) (messageDetails, error) {
			lookups++
			return messageDetails{}, errors.New("permission denied")
		},
	}
	for range 2 {
		if err := adapter.receive(context.Background(), messageEvent("allowed", "user-open-id", "text", `{"text":"hello"}`, nil)); err != nil {
			t.Fatal(err)
		}
	}
	if lookups != 1 || received.Sender.ID != "user-open-id" || received.Sender.DisplayName != "" {
		t.Fatalf("sender=%+v", received.Sender)
	}
}

func TestSenderDisplayNameCoalescesConcurrentLookups(t *testing.T) {
	var lookups atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	adapter := &Adapter{lookupMessageDetails: func(context.Context, string, string) (messageDetails, error) {
		if lookups.Add(1) == 1 {
			close(started)
		}
		<-release
		return messageDetails{senderName: "Owner", threadID: "thread-1"}, nil
	}}
	const callers = 8
	results := make(chan string, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			details, _ := adapter.resolveMessageDetails(context.Background(), "message-1", "user-1", true)
			results <- details.senderName
		}()
	}
	<-started
	// Keep the first lookup in flight until all goroutines have had a chance to
	// join the same singleflight call.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wait.Wait()
	close(results)
	if lookups.Load() != 1 {
		t.Fatalf("lookups=%d", lookups.Load())
	}
	for name := range results {
		if name != "Owner" {
			t.Fatalf("name=%q", name)
		}
	}
}

func TestOpenProgressUsesStrictThreadReplyAndStableUUID(t *testing.T) {
	replies := 0
	patches := 0
	adapter := &Adapter{
		reply: func(_ context.Context, messageID string, body *larkim.ReplyMessageReqBody) (string, error) {
			replies++
			if messageID != "message-1" || body.MsgType == nil || *body.MsgType != "interactive" || body.ReplyInThread == nil || !*body.ReplyInThread || body.Uuid == nil || *body.Uuid != "zotigo-message-1" || body.Content == nil || !strings.Contains(*body.Content, "Zotigo is working") {
				t.Fatalf("unexpected reply: message=%q body=%+v", messageID, body)
			}
			return "reply-1", nil
		},
		patch: func(_ context.Context, messageID, card string) error {
			patches++
			if messageID != "reply-1" || !strings.Contains(card, "Completed") {
				t.Fatalf("unexpected patch: message=%q card=%s", messageID, card)
			}
			return nil
		},
	}
	handle, err := adapter.OpenProgress(context.Background(), channels.InboundMessage{MessageID: "message-1"})
	if err != nil {
		t.Fatal(err)
	}
	if handle.Receipt().MessageID != "reply-1" {
		t.Fatalf("receipt=%q", handle.Receipt().MessageID)
	}
	if err := handle.Update(context.Background(), channels.Progress{State: "completed", Text: "done"}); err != nil {
		t.Fatal(err)
	}
	if replies != 1 || patches != 1 {
		t.Fatalf("replies=%d patches=%d", replies, patches)
	}
}

func TestOpenProgressCanReplyDirectlyInGroup(t *testing.T) {
	adapter := &Adapter{reply: func(_ context.Context, _ string, body *larkim.ReplyMessageReqBody) (string, error) {
		if body.ReplyInThread == nil || *body.ReplyInThread {
			t.Fatalf("reply_in_thread=%v", body.ReplyInThread)
		}
		return "reply-1", nil
	}}
	handle, err := adapter.OpenProgress(context.Background(), channels.InboundMessage{MessageID: "message-1", ReplyMode: channels.ReplyModeDirect})
	if err != nil {
		t.Fatal(err)
	}
	if handle.Receipt().ReplyMode != channels.ReplyModeDirect {
		t.Fatalf("receipt=%+v", handle.Receipt())
	}
}

func TestOpenProgressDoesNotFallbackWhenReplyFails(t *testing.T) {
	calls := 0
	adapter := &Adapter{reply: func(context.Context, string, *larkim.ReplyMessageReqBody) (string, error) {
		calls++
		return "", errors.New("reply denied")
	}}
	if _, err := adapter.OpenProgress(context.Background(), channels.InboundMessage{MessageID: "message-1"}); err == nil {
		t.Fatal("expected strict reply failure")
	}
	if calls != 1 {
		t.Fatalf("reply calls=%d", calls)
	}
}

func TestFinalCardPreservesMarkdown(t *testing.T) {
	card, err := renderFinalCard(channels.TaskResult{Text: "### Result\n\n- **done**\n- `value`"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(card, `"tag":"markdown"`) || !strings.Contains(card, `### Result`) || strings.Contains(card, `"header"`) {
		t.Fatalf("card=%s", card)
	}
}

func TestFinalCardRendersRuntimeAttributionOutsideAnswer(t *testing.T) {
	card, err := renderFinalCard(channels.TaskResult{
		Text:       "answer",
		Runtime:    channels.RuntimeAttribution{Agent: "codex", Model: "gpt-6-astra", ReasoningEffort: "high"},
		DurationMS: 12_400,
		Usage:      &protocol.Usage{InputTokens: 4200, OutputTokens: 500, TotalTokens: 4700},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"answer", "Powered by Codex", "gpt-6-astra", "high", "12s", "4.7k tokens"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card missing %q: %s", want, card)
		}
	}
}

func TestProcessingReactionIsBestEffortAndHasExplicitCleanup(t *testing.T) {
	removed := ""
	adapter := &Adapter{
		addProcessingReaction: func(context.Context, string) (string, error) { return "reaction-1", nil },
		removeProcessingReaction: func(_ context.Context, messageID, reactionID string) error {
			removed = messageID + ":" + reactionID
			return nil
		},
	}
	base := &progressHandle{messageID: "progress-1", patch: func(context.Context, string, string) error { return nil }}
	handle := adapter.withProcessingReaction(context.Background(), channels.InboundMessage{MessageID: "origin-1"}, base)
	if receipt := handle.Receipt(); receipt.ProcessingMarkerID != "reaction-1" || receipt.InboundMessageID != "origin-1" {
		t.Fatalf("receipt=%+v", receipt)
	}
	marker, ok := handle.(channels.ProcessingMarkerHandle)
	if !ok {
		t.Fatal("processing reaction handle did not expose cleanup")
	}
	if err := marker.ClearProcessingMarker(context.Background()); err != nil {
		t.Fatal(err)
	}
	if removed != "origin-1:reaction-1" {
		t.Fatalf("removed=%q", removed)
	}

	adapter.addProcessingReaction = func(context.Context, string) (string, error) { return "", errors.New("missing scope") }
	handle = adapter.withProcessingReaction(context.Background(), channels.InboundMessage{MessageID: "origin-2"}, base)
	if handle.Receipt().ProcessingMarkerID != "" {
		t.Fatalf("failed best-effort reaction was persisted: %+v", handle.Receipt())
	}

	adapter.addProcessingReaction = func(context.Context, string) (string, error) {
		return "", channels.UncertainDelivery(errors.New("connection reset"))
	}
	handle = adapter.withProcessingReaction(context.Background(), channels.InboundMessage{MessageID: "origin-3"}, base)
	if handle.Receipt().ProcessingMarkerID != processingReactionPending {
		t.Fatalf("uncertain reaction did not preserve cleanup intent: %+v", handle.Receipt())
	}
}

func TestProcessingReactionCleanupTreatsVerifiedAbsenceAsSuccess(t *testing.T) {
	deleteErr := errors.New("reaction does not exist")
	err := clearProcessingReaction(context.Background(), "message-1", "reaction-1",
		func(context.Context, string, string) error { return deleteErr },
		func(context.Context, string) ([]string, error) { return []string{"other-reaction"}, nil },
	)
	if err != nil {
		t.Fatalf("verified-absent reaction was not idempotent: %v", err)
	}
}

func TestPendingProcessingReactionDeletesOwnedMatches(t *testing.T) {
	var deleted []string
	err := clearProcessingReaction(context.Background(), "message-1", processingReactionPending,
		func(_ context.Context, _, reactionID string) error {
			deleted = append(deleted, reactionID)
			return nil
		},
		func(context.Context, string) ([]string, error) { return []string{"reaction-1", "reaction-2"}, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deleted, []string{"reaction-1", "reaction-2"}) {
		t.Fatalf("deleted=%v", deleted)
	}
}

func TestCompletedCardRecoveryPreservesFinalBody(t *testing.T) {
	patches := 0
	handle := &progressHandle{messageID: "card-1", patch: func(context.Context, string, string) error {
		patches++
		return nil
	}}
	if err := handle.Recover(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if patches != 0 {
		t.Fatalf("completed recovery replaced the final card %d times", patches)
	}
}

func TestPacedReplyHonorsCancellationBeforeCallingAPI(t *testing.T) {
	pace := newRequestPacer(time.Hour)
	calls := 0
	reply := pacedReply(pace, func(context.Context, string, *larkim.ReplyMessageReqBody) (string, error) {
		calls++
		return "reply-1", nil
	})
	if _, err := reply(context.Background(), "message-1", nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if _, err := reply(ctx, "message-2", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("reply pacer ignored canceled context")
	}
	if calls != 1 {
		t.Fatalf("API calls=%d", calls)
	}
}

func TestPrepareRecoveryResolvesBotIdentityWithoutStartingChannel(t *testing.T) {
	adapter := &Adapter{resolveBotIdentity: func(context.Context) *channeltypes.BotIdentity {
		return &channeltypes.BotIdentity{OpenID: "bot-open-id", Name: "Recovery Bot"}
	}}
	if err := adapter.PrepareRecovery(context.Background()); err != nil {
		t.Fatal(err)
	}
	adapter.mu.RLock()
	identity := adapter.bot
	adapter.mu.RUnlock()
	if identity.OpenID != "bot-open-id" || identity.Name != "Recovery Bot" {
		t.Fatalf("identity=%+v", identity)
	}
}

func TestPrepareRecoveryRejectsMissingBotIdentity(t *testing.T) {
	adapter := &Adapter{resolveBotIdentity: func(context.Context) *channeltypes.BotIdentity { return nil }}
	if err := adapter.PrepareRecovery(context.Background()); err == nil {
		t.Fatal("missing bot identity was accepted")
	}
}
