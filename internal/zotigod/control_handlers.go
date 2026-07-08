package zotigod

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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

var errCommandImageUnavailable = errors.New("command image payload unavailable")

type pauseSessionRequest struct {
	TurnID string `json:"turn_id,omitempty"`
}

type submitMessageRequest struct {
	Text   string                      `json:"text"`
	Images []submitMessageImageRequest `json:"images,omitempty"`
}

type steeringRequest struct {
	Text   string                      `json:"text"`
	Images []submitMessageImageRequest `json:"images,omitempty"`
	TurnID string                      `json:"turn_id,omitempty"`
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
	ID        string                  `json:"id"`
	Sequence  uint64                  `json:"sequence"`
	Type      string                  `json:"type"`
	CreatedAt time.Time               `json:"created_at"`
	Message   *messageCommandPayload  `json:"message,omitempty"`
	Steering  *steeringCommandPayload `json:"steering,omitempty"`
	Pause     *pauseCommandPayload    `json:"pause,omitempty"`
}

type messageCommandPayload struct {
	Text   string                 `json:"text"`
	Images []commandImageResponse `json:"images,omitempty"`
}

type steeringCommandPayload struct {
	Text   string `json:"text"`
	TurnID string `json:"turn_id,omitempty"`
}

type pauseCommandPayload struct {
	TurnID string `json:"turn_id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type commandImageResponse struct {
	MimeType   string `json:"mime_type,omitempty"`
	SizeBytes  int    `json:"size_bytes,omitempty"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	DataBase64 string `json:"data_base64,omitempty"`
}

type publicCommandResponse struct {
	ID        string                       `json:"id"`
	Sequence  uint64                       `json:"sequence"`
	Type      string                       `json:"type"`
	Text      string                       `json:"text,omitempty"`
	Images    []publicCommandImageResponse `json:"images,omitempty"`
	TurnID    string                       `json:"turn_id,omitempty"`
	Reason    string                       `json:"reason,omitempty"`
	CreatedAt time.Time                    `json:"created_at"`
}

type publicCommandImageResponse struct {
	MimeType  string `json:"mime_type,omitempty"`
	SizeBytes int    `json:"size_bytes,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
}

func (h *handler) handleSessionMessage(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req submitMessageRequest
	if err := readRequiredLimitedJSON(r, &req, maxMessageRequestBytes); err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, err.Error())
			return
		}
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		writeAPIError(w, http.StatusBadRequest, "text is required")
		return
	}
	images, err := validateMessageImages(req.Images)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := h.ensureSessionRunning(r.Context(), id); err != nil {
		h.writeEnsureRunningError(w, err)
		return
	}
	items, _, err := h.items.LoadItems(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load display items: %v", err))
		return
	}
	if lastOpenTurnID(items) != "" || hasPendingMessageCommand(items) {
		writeAPIError(w, http.StatusConflict, "message requires an idle session; use steering for an active turn")
		return
	}
	if !h.ensureWorkerOnline(r.Context(), id) {
		writeAPIError(w, http.StatusServiceUnavailable, "message requires an online worker")
		return
	}
	item, err := h.appendMessageCommand(r.Context(), id, text, images)
	if err != nil {
		if errors.Is(err, errSessionBusy) {
			writeAPIError(w, http.StatusConflict, "message requires an idle session; use steering for an active turn")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("append message command: %v", err))
		return
	}

	command, err := messageCommandFromItem(item, h.sessionStoreRoot())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("build message command: %v", err))
		return
	}
	h.sendCommand(r.Context(), id, command)
	writeAPIJSON(w, http.StatusCreated, publicCommandFromCommand(command))
}

var (
	errSessionBusy     = errors.New("session is busy")
	errApprovalPending = errors.New("approval is pending")
)

func (h *handler) appendMessageCommand(ctx context.Context, id string, text string, images []messageImage) (zotigosession.DisplayItem, error) {
	images, err := storeMessageImageBlobs(h.sessionStoreRoot(), id, images)
	if err != nil {
		return zotigosession.DisplayItem{}, err
	}
	item := displayMessageItem(zotigosession.DisplayItemUserMessage, text, images)
	item.Command = &zotigosession.DisplayCommand{
		Type:   sessionCommandMessage,
		Text:   text,
		Images: displayCommandImages(images),
	}
	item, err = h.items.AppendItemIf(ctx, id, item, requireIdleSession)
	if err != nil {
		cleanupMessageImageBlobs(images)
		return zotigosession.DisplayItem{}, err
	}
	return item, nil
}

type rootDirStore interface {
	RootDir() string
}

func (h *handler) sessionStoreRoot() string {
	store, ok := h.store.(rootDirStore)
	if !ok || store == nil {
		return ""
	}
	return store.RootDir()
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
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	session, ok := h.registry.Get(id)
	if !ok {
		h.writeSessionNotLiveOrMissing(w, r.Context(), id, "pause requires a live session")
		return
	}
	if session.State != SessionStateRunning {
		writeAPIError(w, http.StatusConflict, "pause requires a running session")
		return
	}
	var req pauseSessionRequest
	if err := readOptionalJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}

	turnID := strings.TrimSpace(req.TurnID)
	items, _, err := h.items.LoadItems(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load display items: %v", err))
		return
	}
	openTurnID := lastOpenTurnID(items)
	if openTurnID == "" {
		writeAPIError(w, http.StatusConflict, "pause requires an active turn")
		return
	}
	if turnID == "" {
		turnID = openTurnID
	} else if turnID != openTurnID {
		writeAPIError(w, http.StatusConflict, "pause turn_id does not match active turn")
		return
	}
	if hasPendingApprovalForTurn(items, turnID) {
		writeAPIError(w, http.StatusConflict, "pause rejected while approval is pending")
		return
	}
	if !h.ensureWorkerOnline(r.Context(), id) {
		writeAPIError(w, http.StatusServiceUnavailable, "pause requires an online worker")
		return
	}
	items, _, err = h.items.LoadItems(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load display items: %v", err))
		return
	}
	if lastOpenTurnID(items) != turnID {
		writeAPIError(w, http.StatusConflict, "pause requires an active turn")
		return
	}
	if hasPendingApprovalForTurn(items, turnID) {
		writeAPIError(w, http.StatusConflict, "pause rejected while approval is pending")
		return
	}

	item, err := h.appendPauseCommand(r.Context(), id, turnID)
	if err != nil {
		if errors.Is(err, errSessionBusy) {
			writeAPIError(w, http.StatusConflict, "pause requires an active turn")
			return
		}
		if errors.Is(err, errApprovalPending) {
			writeAPIError(w, http.StatusConflict, "pause rejected while approval is pending")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("append pause command: %v", err))
		return
	}

	command := pauseCommandFromItem(item)
	h.sendCommand(r.Context(), id, command)
	writeAPIJSON(w, http.StatusAccepted, publicCommandFromCommand(command))
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
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	session, ok := h.registry.Get(id)
	if !ok {
		h.writeSessionNotLiveOrMissing(w, r.Context(), id, "steering requires a live session")
		return
	}
	if session.State != SessionStateRunning {
		writeAPIError(w, http.StatusConflict, "steering requires a running session")
		return
	}

	var req steeringRequest
	if err := readRequiredLimitedJSON(r, &req, maxMessageRequestBytes); err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, err.Error())
			return
		}
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	if len(req.Images) > 0 {
		writeAPIError(w, http.StatusBadRequest, "steering does not support images")
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		writeAPIError(w, http.StatusBadRequest, "text is required")
		return
	}
	items, _, err := h.items.LoadItems(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load display items: %v", err))
		return
	}
	turnID := lastOpenTurnID(items)
	if turnID == "" {
		writeAPIError(w, http.StatusConflict, "steering requires an active turn")
		return
	}
	if expected := strings.TrimSpace(req.TurnID); expected != "" && expected != turnID {
		writeAPIError(w, http.StatusConflict, "steering turn_id does not match active turn")
		return
	}
	if hasPendingApprovalForTurn(items, turnID) {
		writeAPIError(w, http.StatusConflict, "steering rejected while approval is pending")
		return
	}
	if !h.ensureWorkerOnline(r.Context(), id) {
		writeAPIError(w, http.StatusServiceUnavailable, "steering requires an online worker")
		return
	}
	items, _, err = h.items.LoadItems(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load display items: %v", err))
		return
	}
	if lastOpenTurnID(items) != turnID {
		writeAPIError(w, http.StatusConflict, "steering requires an active turn")
		return
	}
	if hasPendingApprovalForTurn(items, turnID) {
		writeAPIError(w, http.StatusConflict, "steering rejected while approval is pending")
		return
	}

	item, err := h.appendSteeringCommand(r.Context(), id, turnID, text)
	if err != nil {
		if errors.Is(err, errSessionBusy) {
			writeAPIError(w, http.StatusConflict, "steering requires an active turn")
			return
		}
		if errors.Is(err, errApprovalPending) {
			writeAPIError(w, http.StatusConflict, "steering rejected while approval is pending")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("append steering command: %v", err))
		return
	}

	command := steeringCommandFromItem(item)
	h.sendCommand(r.Context(), id, command)
	writeAPIJSON(w, http.StatusCreated, publicCommandFromCommand(command))
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

func (h *handler) writeSessionNotLiveOrMissing(w http.ResponseWriter, ctx context.Context, id string, message string) {
	_, inStore, err := h.storedSession(ctx, id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load session: %v", err))
		return
	}
	if inStore {
		writeAPIErrorCode(w, http.StatusConflict, "session_not_live", message)
		return
	}
	writeAPIError(w, http.StatusNotFound, "session not found")
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
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := h.registry.Get(id); !ok {
		writeAPIError(w, http.StatusNotFound, "session not found")
		return
	}

	var req interruptTurnRequest
	if err := readRequiredJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	turnID := strings.TrimSpace(req.TurnID)
	if turnID == "" {
		writeAPIError(w, http.StatusBadRequest, "turn_id is required")
		return
	}
	items, _, err := h.items.LoadItems(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load display items: %v", err))
		return
	}
	if openTurnID := lastOpenTurnID(items); openTurnID == "" || openTurnID != turnID {
		writeAPIError(w, http.StatusConflict, "turn_id does not match active turn")
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
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("append turn interrupted item: %v", err))
		return
	}

	writeAPIJSON(w, http.StatusCreated, publicDisplayItem(item))
}

func (h *handler) handleWorkerCommands(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := h.registry.Get(id); !ok {
		writeAPIError(w, http.StatusNotFound, "session not found")
		return
	}

	query, err := parseCommandQuery(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if query.HasOffset {
		resp, err := buildCommandsResponseFromOffset(r.Context(), h.items, id, query, h.sessionStoreRoot())
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load display items: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	items, _, err := h.items.LoadItems(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load display items: %v", err))
		return
	}

	resp, err := buildCommandsResponse(items, query, h.sessionStoreRoot())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("build commands: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, resp)
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

func buildCommandsResponse(items []zotigosession.DisplayItem, query commandQuery, rootDir string) (commandsResponse, error) {
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
				command, err := messageCommandFromItem(item, rootDir)
				if err != nil {
					if errors.Is(err, errCommandImageUnavailable) {
						continue
					}
					return commandsResponse{}, err
				}
				resp.Commands = append(resp.Commands, command)
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
	return resp, nil
}

func buildCommandsResponseFromOffset(ctx context.Context, source displayItemSource, sessionID string, query commandQuery, rootDir string) (commandsResponse, error) {
	offsetSource, ok := source.(offsetDisplayItemSource)
	if !ok {
		items, _, err := source.LoadItems(ctx, sessionID)
		if err != nil {
			return commandsResponse{}, err
		}
		resp, err := buildCommandsResponse(items, commandQuery{Limit: query.Limit}, rootDir)
		if err != nil {
			return commandsResponse{}, err
		}
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
			command, ok, err := commandFromDisplayItem(item, rootDir)
			if err != nil {
				if errors.Is(err, errCommandImageUnavailable) {
					continue
				}
				return commandsResponse{}, err
			}
			if ok {
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

func commandFromDisplayItem(item zotigosession.DisplayItem, rootDir string) (commandResponse, bool, error) {
	if item.Type == zotigosession.DisplayItemSteeringMessage {
		if commandText(item.Content) == "" {
			return commandResponse{}, false, nil
		}
		return steeringCommandFromItem(item), true, nil
	}
	if item.Command == nil {
		return commandResponse{}, false, nil
	}
	switch item.Command.Type {
	case sessionCommandMessage:
		command, err := messageCommandFromItem(item, rootDir)
		return command, err == nil, err
	case sessionCommandPause:
		return pauseCommandFromItem(item), true, nil
	default:
		return commandResponse{}, false, nil
	}
}

func displayMessageItem(itemType zotigosession.DisplayItemType, text string, images []messageImage) zotigosession.DisplayItem {
	content := []zotigosession.DisplayContentPart{{
		Type: string(protocol.ContentTypeText),
		Text: text,
	}}
	for _, img := range images {
		content = append(content, zotigosession.DisplayContentPart{
			Type: string(protocol.ContentTypeImage),
			Image: &zotigosession.DisplayMediaPart{
				MediaType: img.MimeType,
				SizeBytes: img.SizeBytes,
				Width:     img.Width,
				Height:    img.Height,
			},
		})
	}
	return zotigosession.DisplayItem{
		Type:    itemType,
		Role:    string(protocol.RoleUser),
		Content: content,
	}
}

func displayCommandImages(images []messageImage) []zotigosession.DisplayCommandImage {
	if len(images) == 0 {
		return nil
	}
	resp := make([]zotigosession.DisplayCommandImage, 0, len(images))
	for _, img := range images {
		resp = append(resp, zotigosession.DisplayCommandImage{
			MimeType:  img.MimeType,
			SizeBytes: img.SizeBytes,
			Width:     img.Width,
			Height:    img.Height,
			BlobPath:  img.BlobPath,
			Data:      img.Data,
		})
	}
	return resp
}

func messageCommandFromItem(item zotigosession.DisplayItem, rootDir string) (commandResponse, error) {
	command := commandResponse{
		ID:        item.ID,
		Sequence:  item.Sequence,
		Type:      sessionCommandMessage,
		CreatedAt: item.CreatedAt,
		Message:   &messageCommandPayload{},
	}
	if item.Command != nil {
		command.Message.Text = item.Command.Text
		images, err := commandImagesFromDisplay(item.Command.Images, rootDir)
		if err != nil {
			return commandResponse{}, err
		}
		command.Message.Images = images
	}
	return command, nil
}

func commandImagesFromDisplay(images []zotigosession.DisplayCommandImage, rootDir string) ([]commandImageResponse, error) {
	if len(images) == 0 {
		return nil, nil
	}
	resp := make([]commandImageResponse, 0, len(images))
	for idx, img := range images {
		part := commandImageResponse{
			MimeType:  img.MimeType,
			SizeBytes: img.SizeBytes,
			Width:     img.Width,
			Height:    img.Height,
		}
		if len(img.Data) > 0 {
			part.DataBase64 = base64.StdEncoding.EncodeToString(img.Data)
		} else if img.BlobPath != "" {
			if rootDir == "" {
				return nil, fmt.Errorf("%w: image persistence is not configured", errCommandImageUnavailable)
			}
			data, err := os.ReadFile(filepath.Join(rootDir, img.BlobPath))
			if err != nil {
				return nil, fmt.Errorf("%w: read command image %d: %v", errCommandImageUnavailable, idx, err)
			}
			part.DataBase64 = base64.StdEncoding.EncodeToString(data)
		}
		if part.DataBase64 == "" {
			return nil, fmt.Errorf("%w: command image %d payload is unavailable", errCommandImageUnavailable, idx)
		}
		resp = append(resp, part)
	}
	return resp, nil
}

func publicCommandFromCommand(command commandResponse) publicCommandResponse {
	resp := publicCommandResponse{
		ID:        command.ID,
		Sequence:  command.Sequence,
		Type:      command.Type,
		CreatedAt: command.CreatedAt,
	}
	switch command.Type {
	case sessionCommandMessage:
		if command.Message != nil {
			resp.Text = command.Message.Text
			resp.Images = publicCommandImages(command.Message.Images)
		}
	case sessionCommandSteering:
		if command.Steering != nil {
			resp.Text = command.Steering.Text
			resp.TurnID = command.Steering.TurnID
		}
	case sessionCommandPause:
		if command.Pause != nil {
			resp.TurnID = command.Pause.TurnID
			resp.Reason = command.Pause.Reason
		}
	}
	return resp
}

func publicCommandImages(images []commandImageResponse) []publicCommandImageResponse {
	if len(images) == 0 {
		return nil
	}
	resp := make([]publicCommandImageResponse, len(images))
	for i, img := range images {
		resp[i] = publicCommandImageResponse{
			MimeType:  img.MimeType,
			SizeBytes: img.SizeBytes,
			Width:     img.Width,
			Height:    img.Height,
		}
	}
	return resp
}

func steeringCommandFromItem(item zotigosession.DisplayItem) commandResponse {
	command := commandResponse{
		ID:        item.ID,
		Sequence:  item.Sequence,
		Type:      sessionCommandSteering,
		CreatedAt: item.CreatedAt,
		Steering: &steeringCommandPayload{
			Text: commandText(item.Content),
		},
	}
	if item.Turn != nil {
		command.Steering.TurnID = item.Turn.ID
	}
	return command
}

func pauseCommandFromItem(item zotigosession.DisplayItem) commandResponse {
	command := commandResponse{
		ID:        item.ID,
		Sequence:  item.Sequence,
		Type:      sessionCommandPause,
		CreatedAt: item.CreatedAt,
		Pause:     &pauseCommandPayload{},
	}
	if item.Command != nil {
		command.Pause.TurnID = item.Command.TurnID
		command.Pause.Reason = item.Command.Reason
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
