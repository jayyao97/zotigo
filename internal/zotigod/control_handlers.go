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
	ID        string                 `json:"id"`
	Sequence  uint64                 `json:"sequence"`
	Type      string                 `json:"type"`
	Text      string                 `json:"text,omitempty"`
	Images    []commandImageResponse `json:"images,omitempty"`
	TurnID    string                 `json:"turn_id,omitempty"`
	Reason    string                 `json:"reason,omitempty"`
	CreatedAt time.Time              `json:"created_at"`
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
	if err := readRequiredLimitedJSON(r, &req, maxMessageRequestBytes); err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}
	images, err := validateMessageImages(req.Images)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
	item, err := h.appendMessageCommand(r.Context(), id, text, images)
	if err != nil {
		if errors.Is(err, errSessionBusy) {
			http.Error(w, "message requires an idle session; use steering for an active turn", http.StatusConflict)
			return
		}
		http.Error(w, fmt.Sprintf("append message command: %v", err), http.StatusInternalServerError)
		return
	}

	command, err := messageCommandFromItem(item, h.sessionStoreRoot())
	if err != nil {
		http.Error(w, fmt.Sprintf("build message command: %v", err), http.StatusInternalServerError)
		return
	}
	h.sendCommand(r.Context(), id, command)
	writeJSON(w, http.StatusCreated, publicCommandFromCommand(command))
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
	if err := readRequiredLimitedJSON(r, &req, maxMessageRequestBytes); err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	if len(req.Images) > 0 {
		http.Error(w, "steering does not support images", http.StatusBadRequest)
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
		resp, err := buildCommandsResponseFromOffset(r.Context(), h.items, id, query, h.sessionStoreRoot())
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

	resp, err := buildCommandsResponse(items, query, h.sessionStoreRoot())
	if err != nil {
		http.Error(w, fmt.Sprintf("build commands: %v", err), http.StatusInternalServerError)
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
	}
	if item.Command != nil {
		command.Text = item.Command.Text
		images, err := commandImagesFromDisplay(item.Command.Images, rootDir)
		if err != nil {
			return commandResponse{}, err
		}
		command.Images = images
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
				return nil, errors.New("image persistence is not configured")
			}
			data, err := os.ReadFile(filepath.Join(rootDir, img.BlobPath))
			if err != nil {
				return nil, fmt.Errorf("read command image %d: %w", idx, err)
			}
			part.DataBase64 = base64.StdEncoding.EncodeToString(data)
		}
		if part.DataBase64 == "" {
			return nil, fmt.Errorf("command image %d payload is unavailable", idx)
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
		Text:      command.Text,
		TurnID:    command.TurnID,
		Reason:    command.Reason,
		CreatedAt: command.CreatedAt,
	}
	if len(command.Images) == 0 {
		return resp
	}
	resp.Images = make([]publicCommandImageResponse, len(command.Images))
	for i, img := range command.Images {
		resp.Images[i] = publicCommandImageResponse{
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
