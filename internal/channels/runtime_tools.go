package channels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
)

const (
	RuntimeToolsVersion     = 1
	RuntimeToolReadMessages = "read_messages"
)

type RuntimeToolResult struct {
	Text string `json:"text"`
}

type runtimeToolMessage struct {
	MessageID       string `json:"message_id"`
	ParentMessageID string `json:"parent_message_id,omitempty"`
	SenderID        string `json:"sender_id"`
	SenderName      string `json:"sender_name,omitempty"`
	Text            string `json:"text"`
	CreatedAt       string `json:"created_at"`
}

// ExecuteRuntimeTool resolves the target exclusively from the bound Session
// and host-attributed turn context. Tool arguments can never select another
// connection or conversation.
func (s *Service) ExecuteRuntimeTool(ctx context.Context, sessionID string, requestContext *protocol.RequestContext, name string, arguments json.RawMessage) (RuntimeToolResult, error) {
	if requestContext == nil || strings.TrimSpace(requestContext.ConnectionID) == "" || strings.TrimSpace(requestContext.ConversationID) == "" {
		return RuntimeToolResult{}, errors.New("channel tools require an active Channel request")
	}
	if name != RuntimeToolReadMessages {
		return RuntimeToolResult{}, fmt.Errorf("unsupported channel tool %q", name)
	}

	s.configMu.RLock()
	defer s.configMu.RUnlock()
	if s.closed {
		return RuntimeToolResult{}, errors.New("channel service is closed")
	}
	conversation, err := s.store.GetConversationBySession(ctx, strings.TrimSpace(sessionID))
	if err != nil {
		return RuntimeToolResult{}, fmt.Errorf("load Channel binding: %w", err)
	}
	if conversation.ConnectionID != requestContext.ConnectionID || conversation.ID != requestContext.ConversationID || conversation.ChatID != requestContext.ExternalConversation {
		return RuntimeToolResult{}, errors.New("channel request does not match the bound Session")
	}
	connection, err := s.store.GetConnection(ctx, conversation.ConnectionID)
	if err != nil {
		return RuntimeToolResult{}, fmt.Errorf("load Channel connection: %w", err)
	}
	if requestContext.Source != connection.Provider {
		return RuntimeToolResult{}, errors.New("channel provider does not match the active request")
	}

	var input struct {
		Limit int `json:"limit"`
	}
	if len(arguments) > 0 && string(arguments) != "null" {
		if err := json.Unmarshal(arguments, &input); err != nil {
			return RuntimeToolResult{}, fmt.Errorf("decode channel tool arguments: %w", err)
		}
	}
	if input.Limit == 0 {
		input.Limit = 20
	}
	if input.Limit < 1 || input.Limit > 50 {
		return RuntimeToolResult{}, errors.New("limit must be between 1 and 50")
	}
	messages, err := s.store.ListMessages(ctx, conversation.ID, input.Limit)
	if err != nil {
		return RuntimeToolResult{}, fmt.Errorf("list Channel messages: %w", err)
	}
	slices.Reverse(messages)
	result := make([]runtimeToolMessage, 0, len(messages))
	var referenced *runtimeToolMessage
	for _, message := range messages {
		text := strings.TrimSpace(message.Text)
		if text == "" {
			continue
		}
		item := runtimeToolMessage{
			MessageID: message.ProviderID, ParentMessageID: message.ParentProviderID,
			SenderID: message.Sender.ID, SenderName: message.Sender.DisplayName,
			Text: text, CreatedAt: message.CreatedAt.UTC().Format(time.RFC3339),
		}
		result = append(result, item)
		if message.ProviderID == requestContext.ExternalParentMessageID {
			copy := item
			referenced = &copy
		}
	}
	if referenced == nil && requestContext.ExternalParentMessageID != "" {
		cached, getErr := s.store.GetMessageByProviderID(ctx, conversation.ConnectionID, requestContext.ExternalParentMessageID)
		switch {
		case getErr == nil:
			if cached.ConversationID != conversation.ID {
				return RuntimeToolResult{}, errors.New("referenced channel message is outside the bound conversation")
			}
			if text := strings.TrimSpace(cached.Text); text != "" {
				item := runtimeToolMessage{
					MessageID: cached.ProviderID, ParentMessageID: cached.ParentProviderID,
					SenderID: cached.Sender.ID, SenderName: cached.Sender.DisplayName,
					Text: text, CreatedAt: cached.CreatedAt.UTC().Format(time.RFC3339),
				}
				referenced = &item
			}
		case !errors.Is(getErr, ErrNotFound):
			return RuntimeToolResult{}, fmt.Errorf("load referenced Channel message: %w", getErr)
		}
	}
	if referenced == nil && requestContext.ExternalParentMessageID != "" {
		s.mu.Lock()
		run := s.runs[conversation.ConnectionID]
		s.mu.Unlock()
		resolver, ok := referencedMessageResolver(run)
		if ok {
			if !s.acquireRunOperation(conversation.ConnectionID, run) {
				return RuntimeToolResult{}, errors.New("channel adapter is stopping")
			}
			resolved, resolveErr := func() (ReferencedMessage, error) {
				defer run.deliveries.Done()
				return resolver.ResolveReferencedMessage(ctx, conversation.ChatID, requestContext.ExternalParentMessageID)
			}()
			if resolveErr != nil {
				return RuntimeToolResult{}, fmt.Errorf("resolve referenced channel message: %w", resolveErr)
			}
			if strings.TrimSpace(resolved.ProviderID) == "" || resolved.ProviderID != requestContext.ExternalParentMessageID {
				return RuntimeToolResult{}, errors.New("resolved channel message does not match the referenced message")
			}
			item := runtimeToolMessage{
				MessageID: resolved.ProviderID, ParentMessageID: resolved.ParentProviderID,
				SenderID: resolved.Sender.ID, SenderName: resolved.Sender.DisplayName,
				Text: strings.TrimSpace(resolved.Text), CreatedAt: resolved.CreatedAt.UTC().Format(time.RFC3339),
			}
			referenced = &item
		}
	}
	encoded, err := json.Marshal(map[string]any{
		"conversation_id":    conversation.ID,
		"referenced_message": referenced,
		"messages":           result,
	})
	if err != nil {
		return RuntimeToolResult{}, fmt.Errorf("encode Channel messages: %w", err)
	}
	return RuntimeToolResult{Text: string(encoded)}, nil
}

func referencedMessageResolver(run *adapterRun) (ReferencedMessageResolver, bool) {
	if run == nil || run.adapter == nil {
		return nil, false
	}
	resolver, ok := run.adapter.(ReferencedMessageResolver)
	return resolver, ok
}
