package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

const (
	defaultItemsLimit = 50
	maxItemsLimit     = 200
)

type displayItemSource interface {
	LoadItems(ctx context.Context, sessionID string) ([]zotigosession.DisplayItem, bool, error)
	AppendItem(ctx context.Context, sessionID string, item zotigosession.DisplayItem) (zotigosession.DisplayItem, error)
	AppendItemIf(ctx context.Context, sessionID string, item zotigosession.DisplayItem, condition func([]zotigosession.DisplayItem) error) (zotigosession.DisplayItem, error)
}

type offsetDisplayItemSource interface {
	LoadItemsFromOffset(ctx context.Context, sessionID string, offset int64, maxLines int) ([]zotigosession.DisplayItem, bool, int64, error)
}

type storedDisplayItemSource struct {
	store zotigosession.Store
}

func (s storedDisplayItemSource) LoadItems(ctx context.Context, sessionID string) ([]zotigosession.DisplayItem, bool, error) {
	return s.store.ListDisplayItems(ctx, sessionID)
}

func (s storedDisplayItemSource) LoadItemsFromOffset(ctx context.Context, sessionID string, offset int64, maxLines int) ([]zotigosession.DisplayItem, bool, int64, error) {
	type offsetStore interface {
		ListDisplayItemsFromOffset(ctx context.Context, id string, offset int64, maxLines int) ([]zotigosession.DisplayItem, bool, int64, error)
	}
	store, ok := s.store.(offsetStore)
	if !ok {
		items, exists, err := s.store.ListDisplayItems(ctx, sessionID)
		return items, exists, 0, err
	}
	return store.ListDisplayItemsFromOffset(ctx, sessionID, offset, maxLines)
}

func (s storedDisplayItemSource) AppendItem(ctx context.Context, sessionID string, item zotigosession.DisplayItem) (zotigosession.DisplayItem, error) {
	if err := s.ensureSession(ctx, sessionID); err != nil {
		return zotigosession.DisplayItem{}, err
	}
	return s.store.AppendDisplayItem(ctx, sessionID, item)
}

func (s storedDisplayItemSource) AppendItemIf(ctx context.Context, sessionID string, item zotigosession.DisplayItem, condition func([]zotigosession.DisplayItem) error) (zotigosession.DisplayItem, error) {
	if err := s.ensureSession(ctx, sessionID); err != nil {
		return zotigosession.DisplayItem{}, err
	}
	type conditionalStore interface {
		AppendDisplayItemIf(ctx context.Context, id string, item zotigosession.DisplayItem, condition func([]zotigosession.DisplayItem) error) (zotigosession.DisplayItem, error)
	}
	if store, ok := s.store.(conditionalStore); ok {
		return store.AppendDisplayItemIf(ctx, sessionID, item, condition)
	}
	items, _, err := s.store.ListDisplayItems(ctx, sessionID)
	if err != nil {
		return zotigosession.DisplayItem{}, err
	}
	if condition != nil {
		if err := condition(items); err != nil {
			return zotigosession.DisplayItem{}, err
		}
	}
	return s.store.AppendDisplayItem(ctx, sessionID, item)
}

func (s storedDisplayItemSource) ensureSession(ctx context.Context, sessionID string) error {
	sess, err := s.store.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("check session: %w", err)
	}
	if sess != nil {
		return nil
	}

	now := time.Now().UTC()
	return s.store.Put(ctx, &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        sessionID,
			CreatedAt: now,
			UpdatedAt: now,
		},
		AgentSnapshot: agent.Snapshot{
			State:     agent.StateIdle,
			CreatedAt: now,
		},
		Turns: make([]zotigosession.Turn, 0),
	})
}

type failingDisplayItemSource struct {
	err error
}

func (s failingDisplayItemSource) LoadItems(context.Context, string) ([]zotigosession.DisplayItem, bool, error) {
	return nil, false, s.err
}

func (s failingDisplayItemSource) AppendItem(context.Context, string, zotigosession.DisplayItem) (zotigosession.DisplayItem, error) {
	return zotigosession.DisplayItem{}, s.err
}

func (s failingDisplayItemSource) AppendItemIf(context.Context, string, zotigosession.DisplayItem, func([]zotigosession.DisplayItem) error) (zotigosession.DisplayItem, error) {
	return zotigosession.DisplayItem{}, s.err
}

type itemResponse struct {
	ID        string                `json:"id"`
	Sequence  uint64                `json:"sequence"`
	Type      string                `json:"type"`
	Role      string                `json:"role,omitempty"`
	Content   []itemContentResponse `json:"content,omitempty"`
	Turn      *itemTurnResponse     `json:"turn,omitempty"`
	Approval  *itemApprovalResponse `json:"approval,omitempty"`
	Command   *itemCommandResponse  `json:"command,omitempty"`
	Profile   *itemProfileResponse  `json:"profile,omitempty"`
	Error     string                `json:"error,omitempty"`
	CreatedAt time.Time             `json:"created_at"`
}

type itemContentResponse struct {
	Type       string                  `json:"type"`
	Text       string                  `json:"text,omitempty"`
	Image      *itemMediaPartResponse  `json:"image,omitempty"`
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
	URL       string `json:"url,omitempty"`
	FileID    string `json:"file_id,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	SizeBytes int    `json:"size_bytes,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
}

type itemTurnResponse struct {
	ID                   string `json:"id,omitempty"`
	Reason               string `json:"reason,omitempty"`
	Status               string `json:"status,omitempty"`
	ProviderFinishReason string `json:"provider_finish_reason,omitempty"`
	LastAgentMessage     string `json:"last_agent_message,omitempty"`
	DurationMS           int64  `json:"duration_ms,omitempty"`
}

type itemApprovalResponse struct {
	ID        string                         `json:"id,omitempty"`
	TurnID    string                         `json:"turn_id,omitempty"`
	Pending   []itemPendingApprovalResponse  `json:"pending,omitempty"`
	Decisions []itemApprovalDecisionResponse `json:"decisions,omitempty"`
}

type itemPendingApprovalResponse struct {
	ToolCallID       string `json:"tool_call_id,omitempty"`
	ToolName         string `json:"tool_name,omitempty"`
	Arguments        string `json:"arguments,omitempty"`
	Description      string `json:"description,omitempty"`
	Reason           string `json:"reason,omitempty"`
	RiskLevel        string `json:"risk_level,omitempty"`
	Source           string `json:"source,omitempty"`
	RequiresSnapshot bool   `json:"requires_snapshot,omitempty"`
}

type itemApprovalDecisionResponse struct {
	ToolCallID   string `json:"tool_call_id,omitempty"`
	Approved     bool   `json:"approved"`
	Reason       string `json:"reason,omitempty"`
	ModifiedArgs string `json:"modified_args,omitempty"`
}

type itemCommandResponse struct {
	Type    string                     `json:"type,omitempty"`
	Text    string                     `json:"text,omitempty"`
	Images  []itemCommandImageResponse `json:"images,omitempty"`
	TurnID  string                     `json:"turn_id,omitempty"`
	Reason  string                     `json:"reason,omitempty"`
	Profile string                     `json:"profile,omitempty"`
}

type itemProfileResponse struct {
	CommandID string `json:"command_id,omitempty"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
}

type itemCommandImageResponse struct {
	MimeType  string `json:"mime_type,omitempty"`
	SizeBytes int    `json:"size_bytes,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	URL       string `json:"url,omitempty"`
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
		Content:   publicDisplayContent(item.Content, displayCommandImagesForContent(item.Command)),
		Turn:      publicDisplayTurn(item.Turn),
		Approval:  publicDisplayApproval(item.Approval),
		Command:   publicDisplayCommand(item.Command),
		Profile:   publicDisplayProfile(item.Profile),
		Error:     item.Error,
		CreatedAt: item.CreatedAt,
	}
}

func publicDisplayProfile(profile *zotigosession.DisplayProfileChange) *itemProfileResponse {
	if profile == nil {
		return nil
	}
	return &itemProfileResponse{CommandID: profile.CommandID, From: profile.From, To: profile.To}
}

func publicDisplayContent(content []zotigosession.DisplayContentPart, commandImages []zotigosession.DisplayCommandImage) []itemContentResponse {
	parts := make([]itemContentResponse, 0, len(content))
	imageIdx := 0
	for _, part := range content {
		image := publicDisplayMediaPart(part.Image)
		if image != nil && image.URL == "" && imageIdx < len(commandImages) {
			image.URL = publicImageURLFromBlobPath(commandImages[imageIdx].BlobPath)
		}
		if part.Image != nil {
			imageIdx++
		}
		parts = append(parts, itemContentResponse{
			Type:       part.Type,
			Text:       part.Text,
			Image:      image,
			ToolCall:   publicDisplayToolCall(part.ToolCall),
			ToolResult: publicDisplayToolResult(part.ToolResult),
		})
	}
	return parts
}

func displayCommandImagesForContent(command *zotigosession.DisplayCommand) []zotigosession.DisplayCommandImage {
	if command == nil || (command.Type != sessionCommandMessage && command.Type != sessionCommandSteering) {
		return nil
	}
	return command.Images
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
		URL:       media.URL,
		FileID:    media.FileID,
		MediaType: media.MediaType,
		SizeBytes: media.SizeBytes,
		Width:     media.Width,
		Height:    media.Height,
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

func publicDisplayApproval(approval *zotigosession.DisplayApproval) *itemApprovalResponse {
	if approval == nil {
		return nil
	}
	return &itemApprovalResponse{
		ID:        approval.ID,
		TurnID:    approval.TurnID,
		Pending:   publicDisplayPendingApprovals(approval.Pending),
		Decisions: publicDisplayApprovalDecisions(approval.Decisions),
	}
}

func publicDisplayCommand(command *zotigosession.DisplayCommand) *itemCommandResponse {
	if command == nil {
		return nil
	}
	return &itemCommandResponse{
		Type:    command.Type,
		Text:    command.Text,
		Images:  publicDisplayCommandImages(command.Images),
		TurnID:  command.TurnID,
		Reason:  command.Reason,
		Profile: command.Profile,
	}
}

func publicDisplayCommandImages(images []zotigosession.DisplayCommandImage) []itemCommandImageResponse {
	if len(images) == 0 {
		return nil
	}
	resp := make([]itemCommandImageResponse, 0, len(images))
	for _, img := range images {
		resp = append(resp, itemCommandImageResponse{
			MimeType:  img.MimeType,
			SizeBytes: img.SizeBytes,
			Width:     img.Width,
			Height:    img.Height,
			URL:       publicImageURLFromBlobPath(img.BlobPath),
		})
	}
	return resp
}

func publicDisplayPendingApprovals(pending []zotigosession.DisplayPendingApproval) []itemPendingApprovalResponse {
	if len(pending) == 0 {
		return nil
	}
	resp := make([]itemPendingApprovalResponse, 0, len(pending))
	for _, item := range pending {
		resp = append(resp, itemPendingApprovalResponse{
			ToolCallID:       item.ToolCallID,
			ToolName:         item.ToolName,
			Arguments:        item.Arguments,
			Description:      item.Description,
			Reason:           item.Reason,
			RiskLevel:        item.RiskLevel,
			Source:           item.Source,
			RequiresSnapshot: item.RequiresSnapshot,
		})
	}
	return resp
}

func publicDisplayApprovalDecisions(decisions []zotigosession.DisplayApprovalDecision) []itemApprovalDecisionResponse {
	if len(decisions) == 0 {
		return nil
	}
	resp := make([]itemApprovalDecisionResponse, 0, len(decisions))
	for _, item := range decisions {
		resp = append(resp, itemApprovalDecisionResponse{
			ToolCallID:   item.ToolCallID,
			Approved:     item.Approved,
			Reason:       item.Reason,
			ModifiedArgs: item.ModifiedArgs,
		})
	}
	return resp
}
