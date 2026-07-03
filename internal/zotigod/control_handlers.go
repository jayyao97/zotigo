package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

const (
	sessionCommandMessage      = "message"
	sessionCommandPause        = "pause"
	sessionCommandSteering     = "steering"
	userPauseReason            = "user_pause"
	controlChannelClosedReason = "control_channel_closed"
	workerRestartedReason      = "worker_restarted"
	defaultCommandsLimit       = 200
	maxCommandsLimit           = 200
	commandOffsetScanLines     = maxCommandsLimit
)

type pauseSessionRequest struct {
	TurnID string `json:"turn_id,omitempty"`
}

type submitMessageRequest struct {
	Text string `json:"text"`
}

type steeringRequest struct {
	Text   string `json:"text"`
	TurnID string `json:"turn_id,omitempty"`
}

type interruptTurnRequest struct {
	TurnID     string `json:"turn_id,omitempty"`
	Reason     string `json:"reason,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

type commandQuery struct {
	After     uint64
	Offset    int64
	Limit     int
	HasOffset bool
}

type commandsResponse struct {
	Commands   []commandResponse `json:"commands"`
	NextCursor string            `json:"next_cursor"`
	NextOffset int64             `json:"next_offset,omitempty"`
}

type commandResponse struct {
	ID        string    `json:"id"`
	Sequence  uint64    `json:"sequence"`
	Type      string    `json:"type"`
	Text      string    `json:"text,omitempty"`
	TurnID    string    `json:"turn_id,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func (h *handler) handleSessionMessage(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	session, ok := h.registry.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if session.State != SessionStateRunning {
		http.Error(w, "message requires a running session", http.StatusConflict)
		return
	}

	var req submitMessageRequest
	if err := readRequiredJSON(r, &req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}
	items, _, err := h.items.LoadItems(r.Context(), id)
	if err != nil {
		http.Error(w, fmt.Sprintf("load display items: %v", err), http.StatusInternalServerError)
		return
	}
	if lastOpenTurnID(items) != "" || hasPendingMessageCommand(items) {
		http.Error(w, "message requires an idle session; use steering for an active turn", http.StatusConflict)
		return
	}
	if !h.ensureWorkerOnline(r.Context(), id) {
		http.Error(w, "message requires an online worker", http.StatusServiceUnavailable)
		return
	}
	item, err := h.appendMessageCommand(r.Context(), id, text)
	if err != nil {
		if errors.Is(err, errSessionBusy) {
			http.Error(w, "message requires an idle session; use steering for an active turn", http.StatusConflict)
			return
		}
		http.Error(w, fmt.Sprintf("append message command: %v", err), http.StatusInternalServerError)
		return
	}

	command := messageCommandFromItem(item)
	h.sendCommand(r.Context(), id, command)
	writeJSON(w, http.StatusCreated, command)
}

var (
	errSessionBusy     = errors.New("session is busy")
	errApprovalPending = errors.New("approval is pending")
)

func (h *handler) appendMessageCommand(ctx context.Context, id string, text string) (zotigosession.DisplayItem, error) {
	item := displayTextMessageItem(zotigosession.DisplayItemUserMessage, text)
	item.Command = &zotigosession.DisplayCommand{
		Type: sessionCommandMessage,
		Text: text,
	}
	return h.items.AppendItemIf(ctx, id, item, requireIdleSession)
}

func requireIdleSession(items []zotigosession.DisplayItem) error {
	if lastOpenTurnID(items) != "" || hasPendingMessageCommand(items) {
		return errSessionBusy
	}
	return nil
}

func (h *handler) handleSessionPause(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	session, ok := h.registry.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if session.State != SessionStateRunning {
		http.Error(w, "pause requires a running session", http.StatusConflict)
		return
	}
	var req pauseSessionRequest
	if err := readOptionalJSON(r, &req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}

	turnID := strings.TrimSpace(req.TurnID)
	items, _, err := h.items.LoadItems(r.Context(), id)
	if err != nil {
		http.Error(w, fmt.Sprintf("load display items: %v", err), http.StatusInternalServerError)
		return
	}
	openTurnID := lastOpenTurnID(items)
	if openTurnID == "" {
		http.Error(w, "pause requires an active turn", http.StatusConflict)
		return
	}
	if turnID == "" {
		turnID = openTurnID
	} else if turnID != openTurnID {
		http.Error(w, "pause turn_id does not match active turn", http.StatusConflict)
		return
	}
	if hasPendingApprovalForTurn(items, turnID) {
		http.Error(w, "pause rejected while approval is pending", http.StatusConflict)
		return
	}
	if !h.ensureWorkerOnline(r.Context(), id) {
		http.Error(w, "pause requires an online worker", http.StatusServiceUnavailable)
		return
	}
	items, _, err = h.items.LoadItems(r.Context(), id)
	if err != nil {
		http.Error(w, fmt.Sprintf("load display items: %v", err), http.StatusInternalServerError)
		return
	}
	if lastOpenTurnID(items) != turnID {
		http.Error(w, "pause requires an active turn", http.StatusConflict)
		return
	}
	if hasPendingApprovalForTurn(items, turnID) {
		http.Error(w, "pause rejected while approval is pending", http.StatusConflict)
		return
	}

	item, err := h.appendPauseCommand(r.Context(), id, turnID)
	if err != nil {
		if errors.Is(err, errSessionBusy) {
			http.Error(w, "pause requires an active turn", http.StatusConflict)
			return
		}
		if errors.Is(err, errApprovalPending) {
			http.Error(w, "pause rejected while approval is pending", http.StatusConflict)
			return
		}
		http.Error(w, fmt.Sprintf("append pause command: %v", err), http.StatusInternalServerError)
		return
	}

	command := pauseCommandFromItem(item)
	h.sendCommand(r.Context(), id, command)
	writeJSON(w, http.StatusAccepted, command)
}

func (h *handler) appendPauseCommand(ctx context.Context, id string, turnID string) (zotigosession.DisplayItem, error) {
	return h.items.AppendItemIf(ctx, id, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemSessionCommand,
		Command: &zotigosession.DisplayCommand{
			Type:   sessionCommandPause,
			TurnID: turnID,
			Reason: userPauseReason,
		},
	}, requireOpenTurnWithoutPendingApproval(turnID))
}

func (h *handler) handleSessionSteering(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	session, ok := h.registry.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if session.State != SessionStateRunning {
		http.Error(w, "steering requires a running session", http.StatusConflict)
		return
	}

	var req steeringRequest
	if err := readRequiredJSON(r, &req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}
	items, _, err := h.items.LoadItems(r.Context(), id)
	if err != nil {
		http.Error(w, fmt.Sprintf("load display items: %v", err), http.StatusInternalServerError)
		return
	}
	turnID := lastOpenTurnID(items)
	if turnID == "" {
		http.Error(w, "steering requires an active turn", http.StatusConflict)
		return
	}
	if expected := strings.TrimSpace(req.TurnID); expected != "" && expected != turnID {
		http.Error(w, "steering turn_id does not match active turn", http.StatusConflict)
		return
	}
	if hasPendingApprovalForTurn(items, turnID) {
		http.Error(w, "steering rejected while approval is pending", http.StatusConflict)
		return
	}
	if !h.ensureWorkerOnline(r.Context(), id) {
		http.Error(w, "steering requires an online worker", http.StatusServiceUnavailable)
		return
	}
	items, _, err = h.items.LoadItems(r.Context(), id)
	if err != nil {
		http.Error(w, fmt.Sprintf("load display items: %v", err), http.StatusInternalServerError)
		return
	}
	if lastOpenTurnID(items) != turnID {
		http.Error(w, "steering requires an active turn", http.StatusConflict)
		return
	}
	if hasPendingApprovalForTurn(items, turnID) {
		http.Error(w, "steering rejected while approval is pending", http.StatusConflict)
		return
	}

	item, err := h.appendSteeringCommand(r.Context(), id, turnID, text)
	if err != nil {
		if errors.Is(err, errSessionBusy) {
			http.Error(w, "steering requires an active turn", http.StatusConflict)
			return
		}
		if errors.Is(err, errApprovalPending) {
			http.Error(w, "steering rejected while approval is pending", http.StatusConflict)
			return
		}
		http.Error(w, fmt.Sprintf("append steering command: %v", err), http.StatusInternalServerError)
		return
	}

	command := steeringCommandFromItem(item)
	h.sendCommand(r.Context(), id, command)
	writeJSON(w, http.StatusCreated, command)
}

func (h *handler) appendSteeringCommand(ctx context.Context, id string, turnID string, text string) (zotigosession.DisplayItem, error) {
	return h.items.AppendItemIf(ctx, id, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemSteeringMessage,
		Role: string(protocol.RoleUser),
		Content: []zotigosession.DisplayContentPart{{
			Type: string(protocol.ContentTypeText),
			Text: text,
		}},
		Turn: &zotigosession.DisplayTurn{ID: turnID},
	}, requireOpenTurnWithoutPendingApproval(turnID))
}

func requireOpenTurnWithoutPendingApproval(turnID string) func([]zotigosession.DisplayItem) error {
	return func(items []zotigosession.DisplayItem) error {
		if lastOpenTurnID(items) != turnID {
			return errSessionBusy
		}
		if hasPendingApprovalForTurn(items, turnID) {
			return errApprovalPending
		}
		return nil
	}
}

func (h *handler) handleWorkerTurnInterrupted(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := h.registry.Get(id); !ok {
		http.NotFound(w, r)
		return
	}

	var req interruptTurnRequest
	if err := readRequiredJSON(r, &req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	turnID := strings.TrimSpace(req.TurnID)
	if turnID == "" {
		http.Error(w, "turn_id is required", http.StatusBadRequest)
		return
	}
	items, _, err := h.items.LoadItems(r.Context(), id)
	if err != nil {
		http.Error(w, fmt.Sprintf("load display items: %v", err), http.StatusInternalServerError)
		return
	}
	if openTurnID := lastOpenTurnID(items); openTurnID == "" || openTurnID != turnID {
		http.Error(w, "turn_id does not match active turn", http.StatusConflict)
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = userPauseReason
	}

	item, err := h.items.AppendItem(r.Context(), id, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnInterrupted,
		Turn: &zotigosession.DisplayTurn{
			ID:         turnID,
			Status:     "interrupted",
			Reason:     reason,
			DurationMS: req.DurationMS,
		},
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("append turn interrupted item: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, publicDisplayItem(item))
}

func (h *handler) handleWorkerCommands(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := h.registry.Get(id); !ok {
		http.NotFound(w, r)
		return
	}

	query, err := parseCommandQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if query.HasOffset {
		resp, err := buildCommandsResponseFromOffset(r.Context(), h.items, id, query)
		if err != nil {
			http.Error(w, fmt.Sprintf("load display items: %v", err), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	items, _, err := h.items.LoadItems(r.Context(), id)
	if err != nil {
		http.Error(w, fmt.Sprintf("load display items: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, buildCommandsResponse(items, query))
}

func parseCommandQuery(r *http.Request) (commandQuery, error) {
	values := r.URL.Query()
	query := commandQuery{Limit: defaultCommandsLimit}

	if raw := values.Get("after"); raw != "" {
		after, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return commandQuery{}, fmt.Errorf("invalid after cursor")
		}
		query.After = after
	}
	if raw := values.Get("offset"); raw != "" {
		offset, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || offset < 0 {
			return commandQuery{}, fmt.Errorf("invalid offset")
		}
		query.Offset = offset
		query.HasOffset = true
	}
	if query.HasOffset && query.After != 0 {
		return commandQuery{}, fmt.Errorf("after and offset cannot both be set")
	}

	if raw := values.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit <= 0 || limit > maxCommandsLimit {
			return commandQuery{}, fmt.Errorf("limit must be between 1 and %d", maxCommandsLimit)
		}
		query.Limit = limit
	}
	return query, nil
}

func (h *handler) ensureWorkerOnline(ctx context.Context, id string) bool {
	if h.workers.Has(id) {
		return true
	}
	if err := h.launchWorker(ctx, id); err != nil {
		return false
	}
	return h.waitForWorker(ctx, id)
}

func (h *handler) sendCommand(ctx context.Context, id string, command commandResponse) bool {
	if h.workers.Send(id, command) {
		return true
	}
	if err := h.launchWorker(ctx, id); err != nil {
		return false
	}
	if !h.waitForWorker(ctx, id) {
		return false
	}
	return h.workers.Send(id, command)
}

func buildCommandsResponse(items []zotigosession.DisplayItem, query commandQuery) commandsResponse {
	resp := commandsResponse{Commands: make([]commandResponse, 0)}
	lastScanned := query.After

	for _, item := range items {
		if item.Sequence <= query.After {
			continue
		}
		lastScanned = item.Sequence

		if item.Type == zotigosession.DisplayItemSteeringMessage {
			if commandText(item.Content) != "" {
				resp.Commands = append(resp.Commands, steeringCommandFromItem(item))
				if len(resp.Commands) >= query.Limit {
					break
				}
			}
			continue
		}

		if item.Command != nil {
			appended := false
			switch item.Command.Type {
			case sessionCommandMessage:
				resp.Commands = append(resp.Commands, messageCommandFromItem(item))
				appended = true
			case sessionCommandPause:
				resp.Commands = append(resp.Commands, pauseCommandFromItem(item))
				appended = true
			}
			if appended && len(resp.Commands) >= query.Limit {
				break
			}
		}
	}

	switch {
	case len(resp.Commands) > 0:
		resp.NextCursor = strconv.FormatUint(resp.Commands[len(resp.Commands)-1].Sequence, 10)
	case lastScanned > query.After:
		resp.NextCursor = strconv.FormatUint(lastScanned, 10)
	default:
		resp.NextCursor = strconv.FormatUint(query.After, 10)
	}
	return resp
}

func buildCommandsResponseFromOffset(ctx context.Context, source displayItemSource, sessionID string, query commandQuery) (commandsResponse, error) {
	offsetSource, ok := source.(offsetDisplayItemSource)
	if !ok {
		items, _, err := source.LoadItems(ctx, sessionID)
		if err != nil {
			return commandsResponse{}, err
		}
		resp := buildCommandsResponse(items, commandQuery{Limit: query.Limit})
		return resp, nil
	}

	resp := commandsResponse{Commands: make([]commandResponse, 0)}
	offset := query.Offset
	for len(resp.Commands) < query.Limit {
		previousOffset := offset
		items, _, nextOffset, err := offsetSource.LoadItemsFromOffset(ctx, sessionID, offset, commandOffsetScanLines)
		if err != nil {
			return commandsResponse{}, err
		}
		for _, item := range items {
			if command, ok := commandFromDisplayItem(item); ok {
				resp.Commands = append(resp.Commands, command)
				if len(resp.Commands) >= query.Limit {
					if item.LogOffset > 0 {
						offset = item.LogOffset
					} else {
						offset = nextOffset
					}
					resp.NextOffset = offset
					resp.NextCursor = strconv.FormatUint(resp.Commands[len(resp.Commands)-1].Sequence, 10)
					return resp, nil
				}
			}
		}
		if nextOffset == previousOffset || len(items) < commandOffsetScanLines {
			offset = nextOffset
			break
		}
		offset = nextOffset
	}
	resp.NextOffset = offset
	if len(resp.Commands) > 0 {
		resp.NextCursor = strconv.FormatUint(resp.Commands[len(resp.Commands)-1].Sequence, 10)
	}
	return resp, nil
}

func commandFromDisplayItem(item zotigosession.DisplayItem) (commandResponse, bool) {
	if item.Type == zotigosession.DisplayItemSteeringMessage {
		if commandText(item.Content) == "" {
			return commandResponse{}, false
		}
		return steeringCommandFromItem(item), true
	}
	if item.Command == nil {
		return commandResponse{}, false
	}
	switch item.Command.Type {
	case sessionCommandMessage:
		return messageCommandFromItem(item), true
	case sessionCommandPause:
		return pauseCommandFromItem(item), true
	default:
		return commandResponse{}, false
	}
}

func displayTextMessageItem(itemType zotigosession.DisplayItemType, text string) zotigosession.DisplayItem {
	return zotigosession.DisplayItem{
		Type: itemType,
		Role: string(protocol.RoleUser),
		Content: []zotigosession.DisplayContentPart{{
			Type: string(protocol.ContentTypeText),
			Text: text,
		}},
	}
}

func messageCommandFromItem(item zotigosession.DisplayItem) commandResponse {
	command := commandResponse{
		ID:        item.ID,
		Sequence:  item.Sequence,
		Type:      sessionCommandMessage,
		CreatedAt: item.CreatedAt,
	}
	if item.Command != nil {
		command.Text = item.Command.Text
	}
	return command
}

func steeringCommandFromItem(item zotigosession.DisplayItem) commandResponse {
	command := commandResponse{
		ID:        item.ID,
		Sequence:  item.Sequence,
		Type:      sessionCommandSteering,
		Text:      commandText(item.Content),
		CreatedAt: item.CreatedAt,
	}
	if item.Turn != nil {
		command.TurnID = item.Turn.ID
	}
	return command
}

func pauseCommandFromItem(item zotigosession.DisplayItem) commandResponse {
	command := commandResponse{
		ID:        item.ID,
		Sequence:  item.Sequence,
		Type:      sessionCommandPause,
		CreatedAt: item.CreatedAt,
	}
	if item.Command != nil {
		command.TurnID = item.Command.TurnID
		command.Reason = item.Command.Reason
	}
	return command
}

func commandText(content []zotigosession.DisplayContentPart) string {
	parts := make([]string, 0, len(content))
	for _, part := range content {
		if part.Type == string(protocol.ContentTypeText) && strings.TrimSpace(part.Text) != "" {
			parts = append(parts, strings.TrimSpace(part.Text))
		}
	}
	return strings.Join(parts, "\n")
}

func lastOpenTurnID(items []zotigosession.DisplayItem) string {
	id, _ := lastOpenTurn(items)
	return id
}

func lastOpenTurn(items []zotigosession.DisplayItem) (string, time.Time) {
	var open string
	var started time.Time
	for _, item := range items {
		if item.Turn == nil || item.Turn.ID == "" {
			continue
		}
		switch item.Type {
		case zotigosession.DisplayItemTurnStarted, zotigosession.DisplayItemTurnPaused:
			open = item.Turn.ID
			if item.Type == zotigosession.DisplayItemTurnStarted {
				started = item.CreatedAt
			}
		case zotigosession.DisplayItemTurnCompleted, zotigosession.DisplayItemTurnFailed, zotigosession.DisplayItemTurnInterrupted:
			if open == item.Turn.ID {
				open = ""
				started = time.Time{}
			}
		}
	}
	return open, started
}

func hasPendingApprovalForTurn(items []zotigosession.DisplayItem, turnID string) bool {
	pending := map[string]struct{}{}
	pendingByTurn := false
	for _, item := range items {
		if item.Approval == nil || item.Approval.TurnID != turnID {
			continue
		}
		switch item.Type {
		case zotigosession.DisplayItemApprovalRequest:
			if item.Approval.ID == "" {
				pendingByTurn = true
			} else {
				pending[item.Approval.ID] = struct{}{}
			}
		case zotigosession.DisplayItemApprovalDecision:
			if item.Approval.ID == "" {
				pendingByTurn = false
			} else {
				delete(pending, item.Approval.ID)
			}
		}
	}
	return pendingByTurn || len(pending) > 0
}

func hasPendingMessageCommand(items []zotigosession.DisplayItem) bool {
	pending := false
	for _, item := range items {
		if item.Command != nil && item.Command.Type == sessionCommandMessage {
			pending = true
		}
		switch item.Type {
		case zotigosession.DisplayItemTurnStarted:
			pending = false
		case zotigosession.DisplayItemTurnCompleted, zotigosession.DisplayItemTurnFailed, zotigosession.DisplayItemTurnInterrupted:
			pending = false
		}
	}
	return pending
}
