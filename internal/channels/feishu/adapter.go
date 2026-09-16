package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jayyao97/zotigo/internal/channels"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/channel"
	"github.com/larksuite/oapi-sdk-go/v3/channel/normalize"
	channeltypes "github.com/larksuite/oapi-sdk-go/v3/channel/types"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkapplication "github.com/larksuite/oapi-sdk-go/v3/service/application/v6"
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
	apiHTTPClient := &http.Client{Timeout: 15 * time.Second, Transport: &resourceLimitTransport{base: http.DefaultTransport}}
	apiClient := lark.NewClient(connection.AppID, secret, lark.WithReqTimeout(15*time.Second), lark.WithHttpClient(apiHTTPClient))
	discoveryClient := lark.NewClient(connection.AppID, secret, lark.WithReqTimeout(15*time.Second))
	wsClient := larkws.NewClient(connection.AppID, secret, larkws.WithEventHandler(events))
	channelClient := channel.NewChannel(apiClient, wsClient)
	adapter := &Adapter{connection: connection, callbacks: callbacks, channel: channelClient, resolveBotIdentity: channelClient.GetBotIdentity, cot: newCOTClient(apiClient), senderNames: make(map[string]string), chatNames: make(map[string]chatNameCacheEntry)}
	adapter.resolveOwnerIDs = func(ctx context.Context) ([]string, error) {
		resp, err := apiClient.Application.Application.Get(ctx, larkapplication.NewGetApplicationReqBuilder().AppId(connection.AppID).Lang("zh_cn").UserIdType("open_id").Build())
		if err != nil {
			return nil, fmt.Errorf("get application owner: %w", err)
		}
		if resp == nil || !resp.Success() || resp.Data == nil || resp.Data.App == nil {
			if resp == nil {
				return nil, errors.New("get application owner: response missing")
			}
			return nil, fmt.Errorf("get application owner: %s", resp.Msg)
		}
		ownerID := ""
		if resp.Data.App.Owner != nil && resp.Data.App.Owner.OwnerId != nil {
			ownerID = strings.TrimSpace(*resp.Data.App.Owner.OwnerId)
		}
		if ownerID == "" && resp.Data.App.CreatorId != nil {
			ownerID = strings.TrimSpace(*resp.Data.App.CreatorId)
		}
		if ownerID == "" {
			return nil, errors.New("get application owner: owner is missing")
		}
		return []string{ownerID}, nil
	}
	adapter.listGroupMembers = func(ctx context.Context, chatID string) ([]channels.Sender, error) {
		var members []channels.Sender
		pageToken := ""
		for {
			builder := larkim.NewGetChatMembersReqBuilder().ChatId(chatID).MemberIdType("open_id").PageSize(100)
			if pageToken != "" {
				builder.PageToken(pageToken)
			}
			resp, err := discoveryClient.Im.ChatMembers.Get(ctx, builder.Build())
			if err != nil {
				return nil, fmt.Errorf("list group members: %w", err)
			}
			if resp == nil || !resp.Success() || resp.Data == nil {
				if resp == nil {
					return nil, errors.New("list group members: response missing")
				}
				return nil, fmt.Errorf("list group members: %s", resp.Msg)
			}
			for _, item := range resp.Data.Items {
				if item == nil || item.MemberId == nil || strings.TrimSpace(*item.MemberId) == "" {
					continue
				}
				member := channels.Sender{ID: strings.TrimSpace(*item.MemberId)}
				if item.Name != nil {
					member.DisplayName = strings.TrimSpace(*item.Name)
				}
				members = append(members, member)
			}
			if resp.Data.HasMore == nil || !*resp.Data.HasMore {
				return members, nil
			}
			if resp.Data.PageToken == nil || strings.TrimSpace(*resp.Data.PageToken) == "" {
				return nil, errors.New("list group members: pagination token missing")
			}
			pageToken = *resp.Data.PageToken
		}
	}
	getMessage := func(ctx context.Context, messageID string) (*larkim.Message, error) {
		lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		req := larkim.NewGetMessageReqBuilder().MessageId(messageID).UserIdType("open_id").WithSenderName(true).Build()
		resp, err := apiClient.Im.Message.Get(lookupCtx, req)
		if err != nil {
			return nil, err
		}
		if resp == nil || !resp.Success() || resp.Data == nil {
			if resp == nil {
				return nil, errors.New("response missing")
			}
			return nil, errors.New(resp.Msg)
		}
		for _, item := range resp.Data.Items {
			if item == nil || item.MessageId == nil || *item.MessageId != messageID {
				continue
			}
			return item, nil
		}
		return nil, errors.New("matching message omitted")
	}
	adapter.lookupMessageDetails = func(ctx context.Context, messageID, senderID string) (messageDetails, error) {
		item, err := getMessage(ctx, messageID)
		if err != nil {
			return messageDetails{}, fmt.Errorf("get message details: %w", err)
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
	adapter.lookupReferencedMessage = func(ctx context.Context, chatID, messageID string) (channels.ReferencedMessage, error) {
		item, err := getMessage(ctx, messageID)
		if err != nil {
			return channels.ReferencedMessage{}, fmt.Errorf("get referenced message: %w", err)
		}
		return referencedMessageFromAPI(item, chatID)
	}
	adapter.lookupChatName = func(ctx context.Context, chatID string) (string, error) {
		lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		resp, err := apiClient.Im.Chat.Get(lookupCtx, larkim.NewGetChatReqBuilder().ChatId(chatID).UserIdType("open_id").Build())
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
			resp, err := discoveryClient.Im.Chat.List(listCtx, builder.Build())
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
	adapter.downloadMessageImage = func(ctx context.Context, messageID, imageKey string) ([]byte, error) {
		downloadCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		request := larkim.NewGetMessageResourceReqBuilder().MessageId(messageID).FileKey(imageKey).Type("image").Build()
		resp, err := apiClient.Im.MessageResource.Get(downloadCtx, request)
		if err != nil {
			return nil, fmt.Errorf("download message image: %w", err)
		}
		if resp == nil || !resp.Success() || resp.File == nil {
			if resp == nil {
				return nil, errors.New("download message image: response missing")
			}
			return nil, fmt.Errorf("download message image: %s", resp.Msg)
		}
		data, err := io.ReadAll(resp.File)
		if err != nil {
			return nil, fmt.Errorf("download message image: %w", err)
		}
		if len(data) == 0 {
			return nil, errors.New("download message image: empty resource")
		}
		if len(data) > maxInboundImageBytes {
			return nil, fmt.Errorf("download message image: resource exceeds %d bytes", maxInboundImageBytes)
		}
		return data, nil
	}
	rawReply := func(ctx context.Context, messageID string, body *larkim.ReplyMessageReqBody) (string, error) {
		req := larkim.NewReplyMessageReqBuilder().MessageId(messageID).Body(body).Build()
		resp, err := apiClient.Im.Message.Reply(ctx, req)
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
		resp, err := apiClient.Im.Message.Patch(ctx, req)
		if err != nil {
			return fmt.Errorf("update progress card: %w", err)
		}
		if !resp.Success() {
			return fmt.Errorf("update progress card: %s", resp.Msg)
		}
		return nil
	}
	adapter.finalReply = func(ctx context.Context, messageID string, result channels.TaskResult, uuid string, replyInThread bool) (string, error) {
		content, err := renderFinalCard(result)
		if err != nil {
			return "", err
		}
		body := larkim.NewReplyMessageReqBodyBuilder().MsgType("interactive").Content(content).ReplyInThread(replyInThread).Uuid(uuid).Build()
		return adapter.reply(ctx, messageID, body)
	}
	adapter.addProcessingReaction = func(ctx context.Context, messageID string) (string, error) {
		reaction := larkim.NewEmojiBuilder().EmojiType("OnIt").Build()
		body := larkim.NewCreateMessageReactionReqBodyBuilder().ReactionType(reaction).Build()
		resp, err := apiClient.Im.MessageReaction.Create(ctx, larkim.NewCreateMessageReactionReqBuilder().MessageId(messageID).Body(body).Build())
		if err != nil {
			return "", channels.UncertainDelivery(fmt.Errorf("add processing reaction: %w", err))
		}
		if resp == nil {
			return "", channels.UncertainDelivery(errors.New("add processing reaction: response missing"))
		}
		if !resp.Success() {
			if resp.ApiResp == nil || resp.StatusCode >= 500 || resp.StatusCode == 408 || resp.StatusCode == 409 || resp.StatusCode == 429 {
				return "", channels.UncertainDelivery(fmt.Errorf("add processing reaction: %s", resp.Msg))
			}
			return "", fmt.Errorf("add processing reaction: %s", resp.Msg)
		}
		if resp.Data == nil || resp.Data.ReactionId == nil || *resp.Data.ReactionId == "" {
			return "", channels.UncertainDelivery(errors.New("add processing reaction: response omitted reaction_id"))
		}
		return *resp.Data.ReactionId, nil
	}
	deleteReaction := func(ctx context.Context, messageID, reactionID string) error {
		resp, err := apiClient.Im.MessageReaction.Delete(ctx, larkim.NewDeleteMessageReactionReqBuilder().MessageId(messageID).ReactionId(reactionID).Build())
		if err != nil {
			return err
		}
		if resp == nil {
			return errors.New("remove processing reaction: response missing")
		}
		if !resp.Success() && (resp.ApiResp == nil || resp.StatusCode != 404) {
			return fmt.Errorf("remove processing reaction: %s", resp.Msg)
		}
		return nil
	}
	listOwnedProcessingReactions := func(ctx context.Context, messageID string) ([]string, error) {
		adapter.mu.RLock()
		botOpenID := adapter.bot.OpenID
		adapter.mu.RUnlock()
		if botOpenID == "" {
			return nil, errors.New("list processing reactions: bot identity is not ready")
		}
		var reactionIDs []string
		pageToken := ""
		for {
			builder := larkim.NewListMessageReactionReqBuilder().MessageId(messageID).ReactionType("OnIt").UserIdType("open_id").PageSize(50)
			if pageToken != "" {
				builder.PageToken(pageToken)
			}
			resp, err := apiClient.Im.MessageReaction.List(ctx, builder.Build())
			if err != nil {
				return nil, fmt.Errorf("list processing reactions: %w", err)
			}
			if resp == nil || !resp.Success() || resp.Data == nil {
				if resp == nil {
					return nil, errors.New("list processing reactions: response missing")
				}
				return nil, fmt.Errorf("list processing reactions: %s", resp.Msg)
			}
			for _, reaction := range resp.Data.Items {
				if reaction == nil || reaction.ReactionId == nil || reaction.Operator == nil || reaction.Operator.OperatorId == nil || *reaction.Operator.OperatorId != botOpenID {
					continue
				}
				reactionIDs = append(reactionIDs, *reaction.ReactionId)
			}
			if resp.Data.HasMore == nil || !*resp.Data.HasMore {
				return reactionIDs, nil
			}
			if resp.Data.PageToken == nil || *resp.Data.PageToken == "" {
				return nil, errors.New("list processing reactions: pagination token missing")
			}
			pageToken = *resp.Data.PageToken
		}
	}
	adapter.removeProcessingReaction = func(ctx context.Context, messageID, reactionID string) error {
		return clearProcessingReaction(ctx, messageID, reactionID, deleteReaction, listOwnedProcessingReactions)
	}
	events.OnP2MessageReceiveV1(adapter.receive)
	channelClient.OnReady(func() {
		identity := channelClient.GetBotIdentity(context.Background())
		if identity != nil {
			adapter.mu.Lock()
			adapter.bot = *identity
			adapter.mu.Unlock()
			ownerCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			ownerIDs, ownerErr := adapter.resolveOwnerIDs(ownerCtx)
			cancel()
			if ownerErr != nil {
				callbacks.Error(ownerErr)
				return
			}
			if callbacks.Owners != nil {
				callbacks.Owners(ownerIDs)
			}
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
	group := channels.Group{ChatID: strings.TrimSpace(*item.ChatId), ChatMode: mode, Available: true}
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
	connection               channels.Connection
	callbacks                channels.AdapterCallbacks
	channel                  channeltypes.Channel
	resolveBotIdentity       func(context.Context) *channeltypes.BotIdentity
	reply                    func(context.Context, string, *larkim.ReplyMessageReqBody) (string, error)
	patch                    func(context.Context, string, string) error
	finalReply               func(context.Context, string, channels.TaskResult, string, bool) (string, error)
	addProcessingReaction    func(context.Context, string) (string, error)
	removeProcessingReaction func(context.Context, string, string) error
	lookupMessageDetails     func(context.Context, string, string) (messageDetails, error)
	lookupReferencedMessage  func(context.Context, string, string) (channels.ReferencedMessage, error)
	lookupChatName           func(context.Context, string) (string, error)
	listGroups               func(context.Context) ([]channels.Group, error)
	listGroupMembers         func(context.Context, string) ([]channels.Sender, error)
	resolveOwnerIDs          func(context.Context) ([]string, error)
	downloadMessageImage     func(context.Context, string, string) ([]byte, error)
	cot                      cotAPI
	mu                       sync.RWMutex
	bot                      channeltypes.BotIdentity
	senderNames              map[string]string
	chatNames                map[string]chatNameCacheEntry
	messageLookup            singleflight.Group
	chatLookup               singleflight.Group
	lookupRetryAfter         time.Time
}

const processingReactionPending = "feishu:on_it:pending"

func clearProcessingReaction(
	ctx context.Context,
	messageID, reactionID string,
	deleteReaction func(context.Context, string, string) error,
	listOwned func(context.Context, string) ([]string, error),
) error {
	if reactionID != processingReactionPending {
		deleteErr := deleteReaction(ctx, messageID, reactionID)
		if deleteErr == nil {
			return nil
		}
		owned, listErr := listOwned(ctx, messageID)
		if listErr != nil {
			return errors.Join(deleteErr, listErr)
		}
		if !slices.Contains(owned, reactionID) {
			return nil
		}
		return deleteErr
	}
	owned, err := listOwned(ctx, messageID)
	if err != nil {
		return err
	}
	for _, ownedReactionID := range owned {
		if err := deleteReaction(ctx, messageID, ownedReactionID); err != nil {
			return err
		}
	}
	return nil
}

func (a *Adapter) ProcessingMarkerIntent(channels.InboundMessage) string {
	if a.addProcessingReaction == nil {
		return ""
	}
	return processingReactionPending
}

type messageDetails struct {
	senderName string
	rootID     string
	threadID   string
}

func referencedMessageFromAPI(item *larkim.Message, expectedChatID string) (channels.ReferencedMessage, error) {
	if item.ChatId == nil || strings.TrimSpace(*item.ChatId) != expectedChatID {
		return channels.ReferencedMessage{}, errors.New("get referenced message: message is outside the bound chat")
	}
	if item.Deleted != nil && *item.Deleted {
		return channels.ReferencedMessage{}, errors.New("get referenced message: message was deleted")
	}
	if item.MessageId == nil || item.MsgType == nil || item.Body == nil || item.Body.Content == nil {
		return channels.ReferencedMessage{}, errors.New("get referenced message: content is incomplete")
	}
	raw := *item.Body.Content
	text := ""
	switch strings.TrimSpace(*item.MsgType) {
	case "text":
		var content struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(raw), &content); err != nil {
			return channels.ReferencedMessage{}, fmt.Errorf("get referenced message: parse text: %w", err)
		}
		text = strings.TrimSpace(content.Text)
	case "post":
		content, err := parseInboundContent("post", raw, channeltypes.BotIdentity{})
		if err != nil {
			return channels.ReferencedMessage{}, fmt.Errorf("get referenced message: parse post: %w", err)
		}
		text = strings.TrimSpace(content.text)
	default:
		return channels.ReferencedMessage{}, fmt.Errorf("get referenced message: unsupported message type %q", strings.TrimSpace(*item.MsgType))
	}
	if text == "" {
		return channels.ReferencedMessage{}, errors.New("get referenced message: text is empty")
	}
	message := channels.ReferencedMessage{ProviderID: strings.TrimSpace(*item.MessageId), Text: text}
	if item.ParentId != nil {
		message.ParentProviderID = strings.TrimSpace(*item.ParentId)
	}
	if item.Sender != nil {
		if item.Sender.Id != nil {
			message.Sender.ID = strings.TrimSpace(*item.Sender.Id)
		}
		if item.Sender.SenderName != nil {
			message.Sender.DisplayName = strings.TrimSpace(*item.Sender.SenderName)
		}
	}
	if item.CreateTime != nil {
		if milliseconds, err := strconv.ParseInt(strings.TrimSpace(*item.CreateTime), 10, 64); err == nil {
			message.CreatedAt = time.UnixMilli(milliseconds).UTC()
		}
	}
	return message, nil
}

type chatNameCacheEntry struct {
	name      string
	expiresAt time.Time
}

const chatNameCacheTTL = 5 * time.Minute

func (a *Adapter) Start(ctx context.Context) error { return a.channel.Start(ctx) }
func (a *Adapter) Stop(ctx context.Context) error  { return a.channel.Stop(ctx) }

func (a *Adapter) ResolveReferencedMessage(ctx context.Context, chatID, messageID string) (channels.ReferencedMessage, error) {
	if a.lookupReferencedMessage == nil {
		return channels.ReferencedMessage{}, errors.New("get referenced message: lookup unavailable")
	}
	return a.lookupReferencedMessage(ctx, chatID, messageID)
}

func (a *Adapter) PrepareRecovery(ctx context.Context) error {
	if a.resolveBotIdentity == nil {
		return errors.New("prepare Feishu recovery: bot identity resolver is unavailable")
	}
	identity := a.resolveBotIdentity(ctx)
	if identity == nil || strings.TrimSpace(identity.OpenID) == "" {
		return errors.New("prepare Feishu recovery: failed to resolve bot identity")
	}
	a.mu.Lock()
	a.bot = *identity
	a.mu.Unlock()
	return nil
}

func (a *Adapter) ListGroups(ctx context.Context) ([]channels.Group, error) {
	if a.listGroups == nil {
		return nil, errors.New("group discovery is unavailable")
	}
	return a.listGroups(ctx)
}

func (a *Adapter) ListGroupMembers(ctx context.Context, chatID string) ([]channels.Sender, error) {
	if a.listGroupMembers == nil {
		return nil, errors.New("group member discovery is unavailable")
	}
	return a.listGroupMembers(ctx, chatID)
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
	if normalized.RawContentType != "text" && normalized.RawContentType != "post" && normalized.RawContentType != "image" {
		return nil
	}
	mentioned := false
	text := normalized.Content
	var imageKeys []string
	if normalized.RawContentType == "post" || normalized.RawContentType == "image" {
		rawContent := ""
		if event.Event != nil && event.Event.Message != nil && event.Event.Message.Content != nil {
			rawContent = *event.Event.Message.Content
		}
		parsed, err := parseInboundContent(normalized.RawContentType, rawContent, bot)
		if err != nil {
			return fmt.Errorf("parse Feishu %s message: %w", normalized.RawContentType, err)
		}
		text, imageKeys, mentioned = parsed.text, parsed.imageKeys, parsed.mentionedBot
	}
	for _, mention := range normalized.Mentions {
		if mention.OpenID == bot.OpenID || mention.UserID == bot.OpenID || (bot.UserID != "" && mention.UserID == bot.UserID) {
			mentioned = true
			if mention.Key != "" {
				text = strings.ReplaceAll(text, mention.Key, "")
			}
		}
	}
	images := make([]channels.InboundImage, 0, len(imageKeys))
	for _, imageKey := range imageKeys {
		images = append(images, channels.InboundImage{ProviderKey: normalized.MessageID + "\x00" + imageKey})
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
	in := channels.InboundMessage{EventID: normalized.EventID, MessageID: normalized.MessageID, ParentMessageID: rawParentID, ChatID: normalized.ChatID, ChatType: normalized.ChatType, ChatName: chatName, RootID: rootID, ThreadID: threadID, Sender: channels.Sender{ID: normalized.UserID, DisplayName: details.senderName}, Text: strings.TrimSpace(text), Images: images, MentionedBot: mentioned, ConversationKey: normalized.ChatID + "\x00" + rootID, StartsConversation: startsConversation, TriggerAllowed: !startsConversation || mentioned, CreatedAt: created}
	if in.RootID != "" {
		in.ConversationName = topicDisplayName(in.Text)
	}
	return a.callbacks.Inbound(ctx, in)
}

func (a *Adapter) ResolveInboundImages(ctx context.Context, images []channels.InboundImage) ([]channels.InboundImage, error) {
	resolved := make([]channels.InboundImage, len(images))
	for index, image := range images {
		if len(image.Data) > 0 {
			resolved[index] = image
			continue
		}
		messageID, imageKey, ok := strings.Cut(image.ProviderKey, "\x00")
		if !ok || strings.TrimSpace(messageID) == "" || strings.TrimSpace(imageKey) == "" {
			return nil, fmt.Errorf("resolve Feishu image %d: invalid provider reference", index)
		}
		if a.downloadMessageImage == nil {
			return nil, errors.New("resolve Feishu image: downloader unavailable")
		}
		data, err := a.downloadMessageImage(ctx, messageID, imageKey)
		if err != nil {
			return nil, err
		}
		resolved[index] = channels.InboundImage{Data: data}
	}
	return resolved, nil
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

type processingReactionHandle struct {
	channels.ProgressHandle
	receipt  channels.DeliveryReceipt
	originID string
	remove   func(context.Context, string, string) error
}

func (h *processingReactionHandle) Receipt() channels.DeliveryReceipt { return h.receipt }

func (h *processingReactionHandle) ClearProcessingMarker(ctx context.Context) error {
	if h.receipt.ProcessingMarkerID == "" || h.originID == "" || h.remove == nil {
		return nil
	}
	return h.remove(ctx, h.originID, h.receipt.ProcessingMarkerID)
}

func (a *Adapter) withProcessingReaction(ctx context.Context, in channels.InboundMessage, handle channels.ProgressHandle) channels.ProgressHandle {
	receipt := handle.Receipt()
	receipt.InboundMessageID = in.MessageID
	receipt.ReplyMode = in.ReplyMode
	receipt.ProcessingMarkerID = processingReactionPending
	if a.addProcessingReaction != nil {
		if reactionID, err := a.addProcessingReaction(ctx, in.MessageID); err == nil {
			receipt.ProcessingMarkerID = reactionID
		} else if !channels.IsUncertainDelivery(err) {
			receipt.ProcessingMarkerID = ""
		}
	} else {
		receipt.ProcessingMarkerID = ""
	}
	return &processingReactionHandle{ProgressHandle: handle, receipt: receipt, originID: in.MessageID, remove: a.removeProcessingReaction}
}

func (h *progressHandle) Receipt() channels.DeliveryReceipt {
	return channels.DeliveryReceipt{Mode: channels.ProgressModeInteractiveCard, MessageID: h.messageID}
}

func (a *Adapter) OpenProgress(ctx context.Context, in channels.InboundMessage) (channels.ProgressHandle, error) {
	if a.connection.ProgressMode == channels.ProgressModeCOT || a.connection.ProgressMode == channels.ProgressModeAuto {
		handle, err := openCOTProgress(ctx, a.cot, a.finalReply, in)
		if err == nil {
			return a.withProcessingReaction(ctx, in, handle), nil
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
	replyInThread := in.ReplyMode != channels.ReplyModeDirect
	body := larkim.NewReplyMessageReqBodyBuilder().MsgType("interactive").Content(card).ReplyInThread(replyInThread).Uuid("zotigo-" + in.MessageID).Build()
	messageID, err := a.reply(ctx, in.MessageID, body)
	if err != nil {
		return nil, err
	}
	return a.withProcessingReaction(ctx, in, &progressHandle{messageID: messageID, patch: a.patch}), nil
}

func (a *Adapter) ResumeProgress(receipt channels.DeliveryReceipt) channels.ProgressHandle {
	if receipt.Mode == channels.ProgressModeCOT && receipt.COTID != "" {
		return &processingReactionHandle{ProgressHandle: resumeCOTProgress(a.cot, a.finalReply, receipt), receipt: receipt, originID: receipt.InboundMessageID, remove: a.removeProcessingReaction}
	}
	return &processingReactionHandle{ProgressHandle: &progressHandle{messageID: receipt.MessageID, patch: a.patch}, receipt: receipt, originID: receipt.InboundMessageID, remove: a.removeProcessingReaction}
}

func (h *progressHandle) Update(ctx context.Context, progress channels.Progress) error {
	card, err := renderCard(progress)
	if err != nil {
		return err
	}
	return h.patch(ctx, h.messageID, card)
}

func (h *progressHandle) Complete(ctx context.Context, result channels.TaskResult, persistFinal func(string) error) error {
	if err := h.Update(ctx, channels.Progress{State: "completed", Text: renderTaskResultMarkdown(result)}); err != nil {
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

func renderFinalCard(result channels.TaskResult) (string, error) {
	text := renderTaskResultMarkdown(result)
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

func renderTaskResultMarkdown(result channels.TaskResult) string {
	text := strings.TrimSpace(result.Text)
	if runes := []rune(text); len(runes) > 5400 {
		text = string(runes[:5400]) + "…"
	}
	parts := make([]string, 0, 6)
	if agent := strings.TrimSpace(result.Runtime.Agent); agent != "" {
		switch strings.ToLower(agent) {
		case "codex":
			parts = append(parts, "Codex")
		case "zotigo":
			parts = append(parts, "Zotigo")
		default:
			parts = append(parts, safeAttributionPart(agent))
		}
	}
	if profile := strings.TrimSpace(result.Runtime.ProfileName); profile != "" && profile != result.Runtime.Model {
		parts = append(parts, safeAttributionPart(profile))
	}
	if model := strings.TrimSpace(result.Runtime.Model); model != "" {
		parts = append(parts, safeAttributionPart(model))
	}
	if effort := strings.TrimSpace(result.Runtime.ReasoningEffort); effort != "" {
		parts = append(parts, safeAttributionPart(effort))
	}
	if result.DurationMS > 0 {
		parts = append(parts, formatDuration(result.DurationMS))
	}
	if result.Usage != nil {
		if total := result.Usage.Normalized().TotalTokens; total > 0 {
			parts = append(parts, formatTokenCount(total)+" tokens")
		}
	}
	if len(parts) == 0 {
		return text
	}
	return text + "\n\n---\n<font color='grey'>Powered by " + strings.Join(parts, " · ") + "</font>"
}

func formatDuration(milliseconds int64) string {
	if milliseconds < 1000 {
		return strconv.FormatInt(milliseconds, 10) + "ms"
	}
	seconds := float64(milliseconds) / 1000
	if milliseconds%1000 == 0 || seconds >= 10 {
		return strconv.FormatInt((milliseconds+500)/1000, 10) + "s"
	}
	return strconv.FormatFloat(seconds, 'f', 1, 64) + "s"
}

func formatTokenCount(tokens int) string {
	if tokens < 1000 {
		return strconv.Itoa(tokens)
	}
	if tokens < 10000 {
		return strconv.FormatFloat(float64(tokens)/1000, 'f', 1, 64) + "k"
	}
	return strconv.Itoa((tokens+500)/1000) + "k"
}

func safeAttributionPart(value string) string {
	runes := []rune(strings.Join(strings.Fields(value), " "))
	if len(runes) > 120 {
		runes = append(runes[:120], '…')
	}
	return html.EscapeString(string(runes))
}
