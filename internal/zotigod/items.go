package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
)

const (
	defaultItemsLimit = 50
	maxItemsLimit     = 200
)

type displayItemSource interface {
	LoadItems(ctx context.Context, sessionID string) ([]zotigosession.DisplayItem, bool, error)
}

type storedDisplayItemSource struct {
	store zotigosession.Store
}

func newStoredDisplayItemSource() (displayItemSource, error) {
	store, err := zotigosession.NewFileStore("")
	if err != nil {
		return nil, err
	}
	return storedDisplayItemSource{store: store}, nil
}

func (s storedDisplayItemSource) LoadItems(ctx context.Context, sessionID string) ([]zotigosession.DisplayItem, bool, error) {
	return s.store.ListDisplayItems(ctx, sessionID)
}

type failingDisplayItemSource struct {
	err error
}

func (s failingDisplayItemSource) LoadItems(context.Context, string) ([]zotigosession.DisplayItem, bool, error) {
	return nil, false, s.err
}

type itemResponse struct {
	ID        string                `json:"id"`
	Sequence  uint64                `json:"sequence"`
	Type      string                `json:"type"`
	Role      string                `json:"role,omitempty"`
	Content   []itemContentResponse `json:"content,omitempty"`
	Turn      *itemTurnResponse     `json:"turn,omitempty"`
	Error     string                `json:"error,omitempty"`
	CreatedAt time.Time             `json:"created_at"`
}

type itemContentResponse struct {
	Type       string                  `json:"type"`
	Text       string                  `json:"text,omitempty"`
	ToolCall   *itemToolCallResponse   `json:"tool_call,omitempty"`
	ToolResult *itemToolResultResponse `json:"tool_result,omitempty"`
}

type itemToolCallResponse struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type itemToolResultResponse struct {
	ToolCallID string                          `json:"tool_call_id,omitempty"`
	ToolName   string                          `json:"tool_name,omitempty"`
	ResultType string                          `json:"result_type,omitempty"`
	Text       string                          `json:"text,omitempty"`
	JSON       any                             `json:"json,omitempty"`
	Reason     string                          `json:"reason,omitempty"`
	Content    []itemToolResultContentResponse `json:"content,omitempty"`
	IsError    bool                            `json:"is_error,omitempty"`
}

type itemToolResultContentResponse struct {
	Type  string                 `json:"type"`
	Text  string                 `json:"text,omitempty"`
	Image *itemMediaPartResponse `json:"image,omitempty"`
}

type itemMediaPartResponse struct {
	Data      []byte `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
	FileID    string `json:"file_id,omitempty"`
	MediaType string `json:"media_type,omitempty"`
}

type itemTurnResponse struct {
	ID                   string `json:"id,omitempty"`
	Reason               string `json:"reason,omitempty"`
	Status               string `json:"status,omitempty"`
	ProviderFinishReason string `json:"provider_finish_reason,omitempty"`
	LastAgentMessage     string `json:"last_agent_message,omitempty"`
	DurationMS           int64  `json:"duration_ms,omitempty"`
}

type itemsResponse struct {
	Items      []itemResponse `json:"items"`
	NextCursor string         `json:"next_cursor"`
	PrevCursor string         `json:"prev_cursor"`
	HasMore    bool           `json:"has_more"`
}

func parseDisplayItemQuery(r *http.Request) (zotigosession.DisplayPageQuery, error) {
	values := r.URL.Query()
	query := zotigosession.DisplayPageQuery{Limit: defaultItemsLimit}

	if raw := values.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit <= 0 || limit > maxItemsLimit {
			return zotigosession.DisplayPageQuery{}, fmt.Errorf("limit must be between 1 and %d", maxItemsLimit)
		}
		query.Limit = limit
	}

	if raw := values.Get("after"); raw != "" {
		cursor, err := parseDisplayCursor(raw)
		if err != nil {
			return zotigosession.DisplayPageQuery{}, fmt.Errorf("invalid after cursor")
		}
		query.After = cursor
		query.HasAfter = true
	}

	if raw := values.Get("before"); raw != "" {
		cursor, err := parseDisplayCursor(raw)
		if err != nil {
			return zotigosession.DisplayPageQuery{}, fmt.Errorf("invalid before cursor")
		}
		query.Before = cursor
		query.HasBefore = true
	}

	if query.HasAfter && query.HasBefore {
		return zotigosession.DisplayPageQuery{}, errors.New("after and before cannot both be set")
	}
	return query, nil
}

func parseDisplayCursor(raw string) (uint64, error) {
	cursor, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || cursor == 0 {
		return 0, fmt.Errorf("invalid cursor")
	}
	return cursor, nil
}

func buildItemsResponse(items []zotigosession.DisplayItem, query zotigosession.DisplayPageQuery) itemsResponse {
	page := zotigosession.PageDisplayItems(items, query)
	resp := itemsResponse{
		Items:      make([]itemResponse, 0, len(page.Items)),
		NextCursor: page.NextCursor,
		PrevCursor: page.PrevCursor,
		HasMore:    page.HasMore,
	}
	for _, item := range page.Items {
		resp.Items = append(resp.Items, publicDisplayItem(item))
	}
	return resp
}

func publicDisplayItem(item zotigosession.DisplayItem) itemResponse {
	return itemResponse{
		ID:        item.ID,
		Sequence:  item.Sequence,
		Type:      string(item.Type),
		Role:      item.Role,
		Content:   publicDisplayContent(item.Content),
		Turn:      publicDisplayTurn(item.Turn),
		Error:     item.Error,
		CreatedAt: item.CreatedAt,
	}
}

func publicDisplayContent(content []zotigosession.DisplayContentPart) []itemContentResponse {
	parts := make([]itemContentResponse, 0, len(content))
	for _, part := range content {
		parts = append(parts, itemContentResponse{
			Type:       part.Type,
			Text:       part.Text,
			ToolCall:   publicDisplayToolCall(part.ToolCall),
			ToolResult: publicDisplayToolResult(part.ToolResult),
		})
	}
	return parts
}

func publicDisplayToolCall(call *zotigosession.DisplayToolCall) *itemToolCallResponse {
	if call == nil {
		return nil
	}
	return &itemToolCallResponse{
		ID:        call.ID,
		Name:      call.Name,
		Arguments: call.Arguments,
	}
}

func publicDisplayToolResult(result *zotigosession.DisplayToolResult) *itemToolResultResponse {
	if result == nil {
		return nil
	}
	return &itemToolResultResponse{
		ToolCallID: result.ToolCallID,
		ToolName:   result.ToolName,
		ResultType: result.ResultType,
		Text:       result.Text,
		JSON:       result.JSON,
		Reason:     result.Reason,
		Content:    publicDisplayToolResultContent(result.Content),
		IsError:    result.IsError,
	}
}

func publicDisplayToolResultContent(content []zotigosession.DisplayToolResultContentPart) []itemToolResultContentResponse {
	if len(content) == 0 {
		return nil
	}
	parts := make([]itemToolResultContentResponse, 0, len(content))
	for _, part := range content {
		parts = append(parts, itemToolResultContentResponse{
			Type:  part.Type,
			Text:  part.Text,
			Image: publicDisplayMediaPart(part.Image),
		})
	}
	return parts
}

func publicDisplayMediaPart(media *zotigosession.DisplayMediaPart) *itemMediaPartResponse {
	if media == nil {
		return nil
	}
	return &itemMediaPartResponse{
		Data:      media.Data,
		URL:       media.URL,
		FileID:    media.FileID,
		MediaType: media.MediaType,
	}
}

func publicDisplayTurn(turn *zotigosession.DisplayTurn) *itemTurnResponse {
	if turn == nil {
		return nil
	}
	return &itemTurnResponse{
		ID:                   turn.ID,
		Reason:               turn.Reason,
		Status:               turn.Status,
		ProviderFinishReason: turn.ProviderFinishReason,
		LastAgentMessage:     turn.LastAgentMessage,
		DurationMS:           turn.DurationMS,
	}
}
