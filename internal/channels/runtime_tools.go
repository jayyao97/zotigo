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
	for _, message := range messages {
		text := strings.TrimSpace(message.Text)
		if text == "" {
			continue
		}
		result = append(result, runtimeToolMessage{
			MessageID: message.ProviderID, ParentMessageID: message.ParentProviderID,
			SenderID: message.Sender.ID, SenderName: message.Sender.DisplayName,
			Text: text, CreatedAt: message.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	encoded, err := json.Marshal(map[string]any{
		"conversation_id": conversation.ID,
		"messages":        result,
	})
	if err != nil {
		return RuntimeToolResult{}, fmt.Errorf("encode Channel messages: %w", err)
	}
	return RuntimeToolResult{Text: string(encoded)}, nil
}
