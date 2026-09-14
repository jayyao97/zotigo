package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jayyao97/zotigo/internal/channels"
	"github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/channel"
	"github.com/larksuite/oapi-sdk-go/v3/channel/normalize"
	channeltypes "github.com/larksuite/oapi-sdk-go/v3/channel/types"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
	"golang.org/x/sync/singleflight"
)

type Factory struct{}

func (Factory) Capabilities() channels.AdapterCapabilities {
	return channels.AdapterCapabilities{ProgressModes: []string{channels.ProgressModeAuto, channels.ProgressModeCOT, channels.ProgressModeInteractiveCard}, DefaultProgressMode: channels.ProgressModeInteractiveCard}
}

func (Factory) New(connection channels.Connection, secret string, callbacks channels.AdapterCallbacks) (channels.Adapter, error) {
	if connection.Provider != channels.ProviderFeishu {
		return nil, fmt.Errorf("unsupported provider %q", connection.Provider)
	}
	if connection.AppID == "" || secret == "" {
		return nil, errors.New("app credentials are required")
	}
	events := dispatcher.NewEventDispatcher("", "")
	apiClient := lark.NewClient(connection.AppID, secret, lark.WithReqTimeout(15*time.Second))
	discoveryClient := lark.NewClient(connection.AppID, secret, lark.WithReqTimeout(15*time.Second))
	wsClient := larkws.NewClient(connection.AppID, secret, larkws.WithEventHandler(events))
	channelClient := channel.NewChannel(apiClient, wsClient)
	adapter := &Adapter{connection: connection, callbacks: callbacks, channel: channelClient, cot: newCOTClient(apiClient), senderNames: make(map[string]string), chatNames: make(map[string]chatNameCacheEntry)}
	adapter.lookupMessageDetails = func(ctx context.Context, messageID, senderID string) (messageDetails, error) {
		lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		req := larkim.NewGetMessageReqBuilder().MessageId(messageID).UserIdType("open_id").WithSenderName(true).Build()
		resp, err := apiClient.Im.V1.Message.Get(lookupCtx, req)
		if err != nil {
			return messageDetails{}, fmt.Errorf("get message details: %w", err)
		}
		if resp == nil || !resp.Success() {
			if resp == nil {
				return messageDetails{}, errors.New("get message details: response missing")
			}
			return messageDetails{}, fmt.Errorf("get message details: %s", resp.Msg)
		}
		if resp.Data == nil {
			return messageDetails{}, errors.New("get message details: response data missing")
		}
		for _, item := range resp.Data.Items {
			if item == nil || item.MessageId == nil || *item.MessageId != messageID {
				continue
			}
			details := messageDetails{}
			if item.ThreadId != nil {
				details.threadID = strings.TrimSpace(*item.ThreadId)
			}
			if item.RootId != nil {
				details.rootID = strings.TrimSpace(*item.RootId)
			}
			if item.Sender != nil && item.Sender.Id != nil && *item.Sender.Id == senderID && item.Sender.SenderName != nil {
				details.senderName = strings.TrimSpace(*item.Sender.SenderName)
			}
			return details, nil
		}
		return messageDetails{}, errors.New("get message details: matching message omitted")
	}
	adapter.lookupChatName = func(ctx context.Context, chatID string) (string, error) {
		lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		resp, err := apiClient.Im.V1.Chat.Get(lookupCtx, larkim.NewGetChatReqBuilder().ChatId(chatID).UserIdType("open_id").Build())
		if err != nil {
			return "", fmt.Errorf("get chat details: %w", err)
		}
		if resp == nil || !resp.Success() || resp.Data == nil {
			if resp == nil {
				return "", errors.New("get chat details: response missing")
			}
			return "", fmt.Errorf("get chat details: %s", resp.Msg)
		}
		if resp.Data.Name == nil {
			return "", nil
		}
		return strings.TrimSpace(*resp.Data.Name), nil
	}
	adapter.listGroups = func(ctx context.Context) ([]channels.Group, error) {
		listCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		var groups []channels.Group
		pageToken := ""
		for {
			builder := larkim.NewListChatReqBuilder().SortType("ByCreateTimeAsc").PageSize(100)
			if pageToken != "" {
				builder.PageToken(pageToken)
			}
			resp, err := discoveryClient.Im.V1.Chat.List(listCtx, builder.Build())
			if err != nil {
				return nil, fmt.Errorf("list bot groups: %w", err)
			}
			if resp == nil || !resp.Success() || resp.Data == nil {
				if resp == nil {
					return nil, errors.New("list bot groups: response missing")
				}
				return nil, fmt.Errorf("list bot groups: %s", resp.Msg)
			}
			for _, item := range resp.Data.Items {
				if group, ok := groupFromListChat(item); ok {
					groups = append(groups, group)
				}
			}
			if resp.Data.HasMore == nil || !*resp.Data.HasMore {
				break
			}
			if resp.Data.PageToken == nil || strings.TrimSpace(*resp.Data.PageToken) == "" || strings.TrimSpace(*resp.Data.PageToken) == pageToken {
				return nil, errors.New("list bot groups: pagination token missing")
			}
			pageToken = strings.TrimSpace(*resp.Data.PageToken)
		}
		return groups, nil
	}
	rawReply := func(ctx context.Context, messageID string, body *larkim.ReplyMessageReqBody) (string, error) {
		req := larkim.NewReplyMessageReqBuilder().MessageId(messageID).Body(body).Build()
		resp, err := apiClient.Im.V1.Message.Reply(ctx, req)
		if err != nil {
			return "", channels.UncertainDelivery(fmt.Errorf("reply to message: %w", err))
		}
		if resp == nil {
			return "", channels.UncertainDelivery(errors.New("reply to message: response missing"))
		}
		if !resp.Success() {
			replyErr := fmt.Errorf("reply to message: %s", resp.Msg)
			if resp.ApiResp == nil || resp.StatusCode >= 500 || resp.StatusCode == 408 || resp.StatusCode == 409 || resp.StatusCode == 429 {
				return "", channels.UncertainDelivery(replyErr)
			}
			return "", replyErr
		}
		if resp.Data == nil || resp.Data.MessageId == nil || *resp.Data.MessageId == "" {
			return "", channels.UncertainDelivery(errors.New("reply to message: response omitted message_id"))
		}
		return *resp.Data.MessageId, nil
	}
	// Feishu applies a shared 5 QPS limit to bot replies in one chat. A single
	// adapter-wide pacer is intentionally conservative because Reply only takes
	// an origin message ID, so the chat cannot be keyed safely at this boundary.
	adapter.reply = pacedReply(newRequestPacer(210*time.Millisecond), rawReply)
	adapter.patch = func(ctx context.Context, messageID, card string) error {
		req := larkim.NewPatchMessageReqBuilder().MessageId(messageID).Body(larkim.NewPatchMessageReqBodyBuilder().Content(card).Build()).Build()
		resp, err := apiClient.Im.V1.Message.Patch(ctx, req)
		if err != nil {
			return fmt.Errorf("update progress card: %w", err)
		}
		if !resp.Success() {
			return fmt.Errorf("update progress card: %s", resp.Msg)
		}
		return nil
	}
	adapter.finalReply = func(ctx context.Context, messageID, text, uuid string) (string, error) {
		content, err := renderFinalCard(text)
		if err != nil {
			return "", err
		}
		body := larkim.NewReplyMessageReqBodyBuilder().MsgType("interactive").Content(content).ReplyInThread(true).Uuid(uuid).Build()
		return adapter.reply(ctx, messageID, body)
	}
	events.OnP2MessageReceiveV1(adapter.receive)
	channelClient.OnReady(func() {
		identity := channelClient.GetBotIdentity(context.Background())
		if identity != nil {
			adapter.mu.Lock()
			adapter.bot = *identity
			adapter.mu.Unlock()
			callbacks.Ready(identity.OpenID, identity.Name)
		} else {
			callbacks.Error(errors.New("failed to resolve bot identity"))
		}
	})
	channelClient.OnError(callbacks.Error)
	return adapter, nil
}

func groupFromListChat(item *larkim.ListChat) (channels.Group, bool) {
	if item == nil || item.ChatId == nil || strings.TrimSpace(*item.ChatId) == "" || item.ChatMode == nil {
		return channels.Group{}, false
	}
	mode := strings.TrimSpace(*item.ChatMode)
	if mode != "group" && mode != "topic" {
		return channels.Group{}, false
	}
	group := channels.Group{ChatID: strings.TrimSpace(*item.ChatId), Available: true}
	if item.Name != nil {
		group.Name = strings.TrimSpace(*item.Name)
	}
	if item.Avatar != nil {
		group.Avatar = strings.TrimSpace(*item.Avatar)
	}
	if item.Description != nil {
		group.Description = strings.TrimSpace(*item.Description)
	}
	if item.External != nil {
		group.External = *item.External
	}
	return group, true
}

func pacedReply(pace func(context.Context) error, reply func(context.Context, string, *larkim.ReplyMessageReqBody) (string, error)) func(context.Context, string, *larkim.ReplyMessageReqBody) (string, error) {
	return func(ctx context.Context, messageID string, body *larkim.ReplyMessageReqBody) (string, error) {
		if pace != nil {
			if err := pace(ctx); err != nil {
				return "", fmt.Errorf("pace reply: %w", err)
			}
		}
		return reply(ctx, messageID, body)
	}
}

type Adapter struct {
	connection           channels.Connection
	callbacks            channels.AdapterCallbacks
	channel              channeltypes.Channel
	reply                func(context.Context, string, *larkim.ReplyMessageReqBody) (string, error)
	patch                func(context.Context, string, string) error
	finalReply           func(context.Context, string, string, string) (string, error)
	lookupMessageDetails func(context.Context, string, string) (messageDetails, error)
	lookupChatName       func(context.Context, string) (string, error)
	listGroups           func(context.Context) ([]channels.Group, error)
	cot                  cotAPI
	mu                   sync.RWMutex
	bot                  channeltypes.BotIdentity
	senderNames          map[string]string
	chatNames            map[string]chatNameCacheEntry
	messageLookup        singleflight.Group
	chatLookup           singleflight.Group
	lookupRetryAfter     time.Time
}

type messageDetails struct {
	senderName string
	rootID     string
	threadID   string
}

type chatNameCacheEntry struct {
	name      string
	expiresAt time.Time
}

const chatNameCacheTTL = 5 * time.Minute

func (a *Adapter) Start(ctx context.Context) error { return a.channel.Start(ctx) }
func (a *Adapter) Stop(ctx context.Context) error  { return a.channel.Stop(ctx) }

func (a *Adapter) ListGroups(ctx context.Context) ([]channels.Group, error) {
	if a.listGroups == nil {
		return nil, errors.New("group discovery is unavailable")
	}
	return a.listGroups(ctx)
}

func (a *Adapter) receive(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	normalized := normalize.ParseMessage(event)
	if normalized == nil || normalized.MessageID == "" || normalized.ChatID == "" {
		return nil
	}
	a.mu.RLock()
	bot := a.bot
	a.mu.RUnlock()
	if bot.OpenID == "" {
		if identity := a.channel.GetBotIdentity(ctx); identity != nil {
			bot = *identity
			a.mu.Lock()
			a.bot = bot
			a.mu.Unlock()
		}
	}
	if normalized.UserID == bot.OpenID || (bot.UserID != "" && normalized.UserID == bot.UserID) {
		return nil
	}
	if normalized.RawContentType != "text" && normalized.RawContentType != "post" {
		return nil
	}
	mentioned := false
	text := normalized.Content
	for _, mention := range normalized.Mentions {
		if mention.OpenID == bot.OpenID || mention.UserID == bot.OpenID || (bot.UserID != "" && mention.UserID == bot.UserID) {
			mentioned = true
			if mention.Key != "" {
				text = strings.ReplaceAll(text, mention.Key, "")
			}
		}
	}
	created := time.Now().UTC()
	if normalized.CreateTimeMs > 0 {
		created = time.UnixMilli(normalized.CreateTimeMs).UTC()
	}
	rawRootID, rawThreadID, rawParentID := "", "", ""
	if event.Event != nil && event.Event.Message != nil {
		if event.Event.Message.RootId != nil {
			rawRootID = strings.TrimSpace(*event.Event.Message.RootId)
		}
		if event.Event.Message.ThreadId != nil {
			rawThreadID = strings.TrimSpace(*event.Event.Message.ThreadId)
		}
		if event.Event.Message.ParentId != nil {
			rawParentID = strings.TrimSpace(*event.Event.Message.ParentId)
		}
	}
	requireScope := rawRootID == "" && rawParentID != ""
	details, detailsErr := a.resolveMessageDetails(ctx, normalized.MessageID, normalized.UserID, requireScope)
	if detailsErr != nil && requireScope {
		// A reply without root_id cannot safely be matched to the Conversation
		// created from its top-level message. Never fall back to another root.
		return detailsErr
	}
	rootID := rawRootID
	if rootID == "" {
		rootID = details.rootID
	}
	if requireScope && rootID == "" {
		return errors.New("get message details: reply root is unavailable")
	}
	if normalized.ChatType == "group" && rootID == "" {
		rootID = normalized.MessageID
	}
	threadID := rawThreadID
	if threadID == "" {
		threadID = details.threadID
	}
	chatName := ""
	if normalized.ChatType == "group" {
		chatName, _ = a.resolveChatName(ctx, normalized.ChatID)
	}
	startsConversation := normalized.ChatType == "group" && rootID == normalized.MessageID
	in := channels.InboundMessage{EventID: normalized.EventID, MessageID: normalized.MessageID, ChatID: normalized.ChatID, ChatType: normalized.ChatType, ChatName: chatName, RootID: rootID, ThreadID: threadID, Sender: channels.Sender{ID: normalized.UserID, DisplayName: details.senderName}, Text: strings.TrimSpace(text), MentionedBot: mentioned, ConversationKey: normalized.ChatID + "\x00" + rootID, StartsConversation: startsConversation, TriggerAllowed: !startsConversation || mentioned, CreatedAt: created}
	if in.RootID != "" {
		in.ConversationName = topicDisplayName(in.Text)
	}
	return a.callbacks.Inbound(ctx, in)
}

func (a *Adapter) resolveChatName(ctx context.Context, chatID string) (string, error) {
	now := time.Now()
	a.mu.RLock()
	cached, found := a.chatNames[chatID]
	a.mu.RUnlock()
	if found && cached.name != "" && now.Before(cached.expiresAt) {
		return cached.name, nil
	}
	if a.lookupChatName == nil {
		return "", nil
	}
	value, err, _ := a.chatLookup.Do(chatID, func() (any, error) {
		resolved, lookupErr := a.lookupChatName(ctx, chatID)
		if lookupErr == nil && resolved != "" {
			a.mu.Lock()
			if a.chatNames == nil {
				a.chatNames = make(map[string]chatNameCacheEntry)
			}
			a.chatNames[chatID] = chatNameCacheEntry{name: resolved, expiresAt: time.Now().Add(chatNameCacheTTL)}
			a.mu.Unlock()
		}
		return resolved, lookupErr
	})
	if err != nil {
		return "", err
	}
	name, _ := value.(string)
	return name, nil
}

func (a *Adapter) ResolveConversationName(ctx context.Context, chatID string) (string, error) {
	return a.resolveChatName(ctx, chatID)
}

func topicDisplayName(text string) string {
	const maxRunes = 48
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes]) + "…"
}

func (a *Adapter) resolveMessageDetails(ctx context.Context, messageID, senderID string, requireScope bool) (messageDetails, error) {
	a.mu.RLock()
	cachedName := a.senderNames[senderID]
	retryAfter := a.lookupRetryAfter
	a.mu.RUnlock()
	if !requireScope && cachedName != "" {
		return messageDetails{senderName: cachedName}, nil
	}
	if a.lookupMessageDetails == nil {
		if requireScope {
			return messageDetails{}, errors.New("get message details: lookup unavailable")
		}
		return messageDetails{senderName: cachedName}, nil
	}
	if time.Now().Before(retryAfter) {
		return messageDetails{senderName: cachedName}, errors.New("get message details: temporarily backed off after failure")
	}
	value, err, _ := a.messageLookup.Do(messageID, func() (any, error) {
		details, lookupErr := a.lookupMessageDetails(ctx, messageID, senderID)
		a.mu.Lock()
		defer a.mu.Unlock()
		if lookupErr != nil {
			a.lookupRetryAfter = time.Now().Add(5 * time.Second)
			return messageDetails{}, lookupErr
		}
		a.lookupRetryAfter = time.Time{}
		if details.senderName != "" {
			if a.senderNames == nil {
				a.senderNames = make(map[string]string)
			}
			a.senderNames[senderID] = details.senderName
		} else {
			details.senderName = a.senderNames[senderID]
		}
		return details, nil
	})
	if err != nil {
		return messageDetails{senderName: cachedName}, err
	}
	details, _ := value.(messageDetails)
	return details, nil
}

type progressHandle struct {
	messageID string
	patch     func(context.Context, string, string) error
}

func (h *progressHandle) Receipt() channels.DeliveryReceipt {
	return channels.DeliveryReceipt{Mode: channels.ProgressModeInteractiveCard, MessageID: h.messageID}
}

func (a *Adapter) OpenProgress(ctx context.Context, in channels.InboundMessage) (channels.ProgressHandle, error) {
	if a.connection.ProgressMode == channels.ProgressModeCOT || a.connection.ProgressMode == channels.ProgressModeAuto {
		handle, err := openCOTProgress(ctx, a.cot, a.finalReply, in)
		if err == nil {
			return handle, nil
		}
		var rejected *cotRejectedError
		if !errors.As(err, &rejected) {
			return nil, channels.UncertainDelivery(err)
		}
		if a.connection.ProgressMode == channels.ProgressModeCOT {
			return nil, err
		}
	}
	card, err := renderCard(channels.Progress{State: "running", Text: "Zotigo is working…"})
	if err != nil {
		return nil, err
	}
	body := larkim.NewReplyMessageReqBodyBuilder().MsgType("interactive").Content(card).ReplyInThread(true).Uuid("zotigo-" + in.MessageID).Build()
	messageID, err := a.reply(ctx, in.MessageID, body)
	if err != nil {
		return nil, err
	}
	return &progressHandle{messageID: messageID, patch: a.patch}, nil
}

func (a *Adapter) ResumeProgress(receipt channels.DeliveryReceipt) channels.ProgressHandle {
	if receipt.Mode == channels.ProgressModeCOT && receipt.COTID != "" {
		return resumeCOTProgress(a.cot, a.finalReply, receipt)
	}
	return &progressHandle{messageID: receipt.MessageID, patch: a.patch}
}

func (h *progressHandle) Update(ctx context.Context, progress channels.Progress) error {
	card, err := renderCard(progress)
	if err != nil {
		return err
	}
	return h.patch(ctx, h.messageID, card)
}

func (h *progressHandle) Complete(ctx context.Context, result channels.TaskResult, persistFinal func(string) error) error {
	if err := h.Update(ctx, channels.Progress{State: "completed", Text: result.Text}); err != nil {
		return err
	}
	return persistFinal(h.messageID)
}

func (h *progressHandle) Fail(ctx context.Context, progress channels.Progress) error {
	return h.Update(ctx, progress)
}

func (h *progressHandle) Recover(ctx context.Context, completed bool) error {
	if completed {
		return nil
	}
	return h.Update(ctx, channels.Progress{State: "failed", Text: "Zotigo restarted before this task could finish. Open the bound session to inspect its state."})
}

func renderCard(progress channels.Progress) (string, error) {
	title := "Zotigo"
	template := "blue"
	text := progress.Text
	switch progress.State {
	case "completed":
		title = "Completed"
		template = "green"
	case "failed":
		title = "Failed"
		template = "red"
	case "approval":
		title = "Waiting for approval"
		template = "orange"
	}
	if strings.TrimSpace(text) == "" {
		text = title
	}
	if len([]rune(text)) > 6000 {
		text = string([]rune(text)[:6000]) + "…"
	}
	card := map[string]any{"schema": "2.0", "config": map[string]any{"wide_screen_mode": true}, "header": map[string]any{"title": map[string]any{"tag": "plain_text", "content": title}, "template": template}, "body": map[string]any{"elements": []any{map[string]any{"tag": "markdown", "content": text}}}}
	data, err := json.Marshal(card)
	return string(data), err
}

func renderFinalCard(text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		text = "Completed"
	}
	if len([]rune(text)) > 6000 {
		text = string([]rune(text)[:6000]) + "…"
	}
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"wide_screen_mode": true},
		"body":   map[string]any{"elements": []any{map[string]any{"tag": "markdown", "content": text}}},
	}
	data, err := json.Marshal(card)
	return string(data), err
}
