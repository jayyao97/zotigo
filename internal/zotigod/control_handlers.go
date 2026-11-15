package zotigod

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/skills"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

const (
	defaultCommandsLimit   = 200
	commandOffsetScanLines = wire.MaxCommandsLimit
)

type pauseSessionRequest struct {
	TurnID string `json:"turn_id,omitempty"`
}

type submitMessageRequest struct {
	Text            string                      `json:"text"`
	Images          []submitMessageImageRequest `json:"images,omitempty"`
	Skills          []string                    `json:"skills,omitempty"`
	ClientMessageID string                      `json:"client_message_id,omitempty"`
	Path            json.RawMessage             `json:"path,omitempty"`
}

type steeringRequest struct {
	Text            string                      `json:"text"`
	Images          []submitMessageImageRequest `json:"images,omitempty"`
	TurnID          string                      `json:"turn_id,omitempty"`
	ExpectedTurnID  string                      `json:"expected_turn_id,omitempty"`
	ClientMessageID string                      `json:"client_message_id,omitempty"`
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

type publicCommandResponse struct {
	ID             string                       `json:"id"`
	Sequence       uint64                       `json:"sequence"`
	Type           string                       `json:"type"`
	Text           string                       `json:"text,omitempty"`
	Skills         []string                     `json:"skills,omitempty"`
	Images         []publicCommandImageResponse `json:"images,omitempty"`
	TurnID         string                       `json:"turn_id,omitempty"`
	Reason         string                       `json:"reason,omitempty"`
	Profile        string                       `json:"profile,omitempty"`
	ApprovalPolicy agent.ApprovalPolicy         `json:"approval_policy,omitempty"`
	CreatedAt      time.Time                    `json:"created_at"`
}

type publicCommandImageResponse struct {
	MimeType  string `json:"mime_type,omitempty"`
	SizeBytes int    `json:"size_bytes,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	URL       string `json:"url,omitempty"`
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
	if req.Path != nil {
		writeAPIError(w, http.StatusBadRequest, "path is not accepted; select skills by name")
		return
	}
	images, err := validateMessageImages(req.Images)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if text == "" && len(images) == 0 {
		writeAPIError(w, http.StatusBadRequest, "message requires text or images")
		return
	}
	var selectedNames []string
	if len(req.Skills) > 0 {
		selected, err := h.resolveSessionSkills(r.Context(), id, req.Skills)
		if err != nil {
			var resolutionErr *skills.ResolutionError
			if errors.As(err, &resolutionErr) {
				writeAPIErrorCode(w, http.StatusBadRequest, "skill_"+resolutionErr.Kind, resolutionErr.Error())
				return
			}
			if errors.Is(err, workerbase.ErrSessionNotFound) {
				writeAPIError(w, http.StatusNotFound, "session not found")
				return
			}
			h.logSkillsError("resolve skills", id, err)
			writeAPIError(w, http.StatusInternalServerError, "failed to resolve skills")
			return
		}
		selectedNames = make([]string, len(selected))
		for index, skill := range selected {
			selectedNames[index] = skill.Name
		}
	}
	if h.workers.InputStopping(id) {
		writeAPIErrorCode(w, http.StatusConflict, "turn_stopping", "the active turn is stopping")
		return
	}
	var command wire.Command
	var ensureErr error
	err = h.withRefreshedBoundChannelPrompt(r.Context(), id, func(steeringOnly bool) error {
		if _, startErr := h.ensureSessionRunning(r.Context(), id); startErr != nil {
			ensureErr = startErr
			return startErr
		}
		if !h.ensureWorkerOnline(r.Context(), id) {
			return errWorkerOffline
		}
		var acceptErr error
		command, acceptErr = h.acceptSessionInputCommand(r.Context(), id, strings.TrimSpace(req.ClientMessageID), text, images, selectedNames, "", steeringOnly, false, nil)
		return acceptErr
	})
	if err != nil {
		if ensureErr != nil {
			h.writeEnsureRunningError(w, ensureErr)
			return
		}
		switch {
		case errors.Is(err, errWorkerInputStopping):
			writeAPIErrorCode(w, http.StatusConflict, "turn_stopping", "the active turn is stopping")
		case errors.Is(err, sessionlog.ErrActiveTurn):
			writeAPIErrorCode(w, http.StatusConflict, "active_turn", err.Error())
		case errors.Is(err, sessionlog.ErrInputPolicyMismatch):
			writeAPIErrorCode(w, http.StatusConflict, "input_policy_mismatch", err.Error())
		case errors.Is(err, errSessionBusy):
			writeAPIErrorCode(w, http.StatusConflict, "command_pending", "a message command is already pending")
		case errors.Is(err, sessionlog.ErrCommandIDConflict):
			writeAPIErrorCode(w, http.StatusConflict, "command_id_conflict", err.Error())
		case errors.Is(err, workerbase.ErrSessionNotFound), errors.Is(err, errInvalidSessionTransition), errors.Is(err, errSessionUnavailable):
			h.writeEnsureRunningError(w, err)
		case errors.Is(err, errWorkerOffline):
			writeAPIError(w, http.StatusServiceUnavailable, "message requires an online worker")
		default:
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("append session input command: %v", err))
		}
		return
	}
	writeAPIJSON(w, http.StatusCreated, publicCommandFromCommand(command))
}

func (h *handler) handleSessionImage(w http.ResponseWriter, r *http.Request, id string, name string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if strings.HasPrefix(name, "codex-input-") {
		h.handleCodexInputImage(w, r, id, name)
		return
	}
	blobPath, ok := sessionlog.MessageImageBlobPath(id, name)
	if !ok {
		writeAPIError(w, http.StatusNotFound, "image not found")
		return
	}
	store, ok := h.imageStore()
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "image index is not configured")
		return
	}
	image, ok, err := store.GetImageRef(r.Context(), id, name)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load image reference: %v", err))
		return
	}
	if !ok {
		image, ok, err = h.indexLegacyMessageImageRef(r.Context(), store, id, name, blobPath)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load image reference: %v", err))
			return
		}
	}
	if !ok || filepath.Clean(image.BlobPath) != filepath.Clean(blobPath) {
		writeAPIError(w, http.StatusNotFound, "image not found")
		return
	}
	if !sessionlog.IsAllowedMessageImageMimeType(image.MimeType) {
		writeAPIError(w, http.StatusNotFound, "image not found")
		return
	}
	rootDir := store.RootDir()
	if rootDir == "" {
		writeAPIError(w, http.StatusInternalServerError, "image persistence is not configured")
		return
	}
	data, err := os.ReadFile(filepath.Join(rootDir, blobPath))
	if err != nil {
		if os.IsNotExist(err) {
			writeAPIError(w, http.StatusNotFound, "image not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("read image: %v", err))
		return
	}
	w.Header().Set("Content-Type", image.MimeType)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

var (
	errSessionBusy     = errors.New("session is busy")
	errApprovalPending = errors.New("approval is pending")
)

func (h *handler) prepareSessionInputCommand(ctx context.Context, id string, commandID string, text string, images []sessionlog.MessageImage, selectedSkills []string, steeringOnly bool) (wire.Command, []sessionlog.MessageImage, []zotigosession.ImageRef, error) {
	images, err := sessionlog.StoreMessageImageBlobs(h.sessionStoreRoot(), id, images)
	if err != nil {
		return wire.Command{}, nil, nil, err
	}
	refs, err := sessionlog.MessageImageRefs(id, images)
	if err != nil {
		sessionlog.CleanupMessageImageBlobs(images)
		return wire.Command{}, nil, nil, err
	}
	if err := h.putMessageImageRefs(ctx, refs); err != nil {
		sessionlog.CleanupMessageImageBlobs(images)
		return wire.Command{}, nil, nil, err
	}
	if commandID == "" {
		commandID = "item_" + uuid.NewString()
	}
	commandImages := make([]wire.CommandImageData, 0, len(images))
	for _, image := range images {
		commandImages = append(commandImages, wire.CommandImageData{
			MimeType: image.MimeType, SizeBytes: image.SizeBytes, Width: image.Width, Height: image.Height,
			DataBase64: base64.StdEncoding.EncodeToString(image.Data), BlobPath: image.BlobPath,
		})
	}
	command := wire.Command{ID: commandID, CreatedAt: time.Now().UTC()}
	if steeringOnly {
		command.Type = wire.CommandSteering
		command.Steering = &wire.SteeringCommandPayload{Text: text, Images: commandImages, Skills: append([]string(nil), selectedSkills...)}
	} else {
		command.Type = wire.CommandMessage
		command.Message = &wire.MessageCommandPayload{Text: text, Images: commandImages, Skills: append([]string(nil), selectedSkills...)}
	}
	return command, images, refs, nil
}

type sessionInputConstraints struct {
	approvalPolicy agent.ApprovalPolicy
	promptRevision uint64
	requestContext *protocol.RequestContext
}

func (h *handler) acceptSessionInputCommand(ctx context.Context, id string, commandID string, text string, images []sessionlog.MessageImage, selectedSkills []string, expectedTurnID string, steeringOnly bool, startOnly bool, constraints *sessionInputConstraints) (wire.Command, error) {
	h.updateMu.RLock()
	defer h.updateMu.RUnlock()
	if h.updater != nil && h.updater.Draining() {
		return wire.Command{}, fmt.Errorf("%w: daemon is waiting to apply an update", errSessionUnavailable)
	}
	if h.workers.InputStopping(id) {
		return wire.Command{}, errWorkerInputStopping
	}
	var command wire.Command
	var storedImages []sessionlog.MessageImage
	var refs []zotigosession.ImageRef
	var submission *workerInputSubmission
	err := func() error {
		unlock := h.sessionOps.lock(id)
		defer unlock()

		if unavailable, err := h.ensureSessionActivatable(ctx, id); err != nil {
			return fmt.Errorf("check session availability: %w", err)
		} else if unavailable != "" {
			return fmt.Errorf("%w: %s", errSessionUnavailable, unavailable)
		}
		session, ok := h.registry.Get(id)
		if !ok {
			return workerbase.ErrSessionNotFound
		}
		if session.State != SessionStateRunning && session.State != SessionStatePausing && session.State != SessionStatePaused {
			return errInvalidSessionTransition
		}
		if constraints != nil {
			stored, err := h.store.Get(ctx, id)
			if err != nil {
				return err
			}
			if stored == nil || stored.ApprovalPolicy != constraints.approvalPolicy || stored.PromptConfig.Revision != constraints.promptRevision {
				return sessionlog.ErrInputPolicyMismatch
			}
			items, _, err := h.items.LoadItems(ctx, id)
			if err != nil {
				return err
			}
			if pending, _, ok := pendingApprovalPolicyIntent(items); ok && pending != constraints.approvalPolicy {
				return sessionlog.ErrInputPolicyMismatch
			}
		}
		var err error
		command, storedImages, refs, err = h.prepareSessionInputCommand(ctx, id, commandID, text, images, selectedSkills, steeringOnly)
		if err != nil {
			return err
		}
		if constraints != nil && command.Message != nil {
			command.Message.RequestContext = constraints.requestContext.Clone()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		request := wire.InputRequest{Command: command, SteeringOnly: steeringOnly, StartOnly: startOnly, ExpectedTurnID: expectedTurnID}
		if constraints != nil {
			request.RequiredApprovalPolicy = constraints.approvalPolicy
			request.RequiredPromptRevision = constraints.promptRevision
			request.RequirePromptRevision = true
		}
		submission, err = h.workers.BeginInput(id, request)
		if err != nil {
			return err
		}
		releaseAdmission := h.sessionOps.reserveInput(id)
		submission.OnFinish(releaseAdmission)
		return nil
	}()
	if err != nil {
		cleanupCtx := ctx
		if cleanupCtx.Err() != nil {
			cleanupCtx = context.Background()
		}
		_ = h.deleteMessageImageRefs(cleanupCtx, id, sessionlog.ImageRefNames(refs))
		sessionlog.CleanupMessageImageBlobs(storedImages)
		return wire.Command{}, err
	}

	accepted, err := submission.Await(ctx)
	if err != nil {
		var inputErr *workerInputError
		if errors.As(err, &inputErr) {
			_ = h.deleteMessageImageRefs(ctx, id, sessionlog.ImageRefNames(refs))
			sessionlog.CleanupMessageImageBlobs(storedImages)
		}
		return wire.Command{}, err
	}
	if !sessionlog.SameCommandImagePaths(command, accepted) {
		_ = h.deleteMessageImageRefs(ctx, id, sessionlog.ImageRefNames(refs))
		sessionlog.CleanupMessageImageBlobs(storedImages)
	}
	return accepted, nil
}

func (h *handler) indexLegacyMessageImageRef(ctx context.Context, store imageRefStore, sessionID string, name string, blobPath string) (zotigosession.ImageRef, bool, error) {
	items, inStore, err := h.items.LoadItems(ctx, sessionID)
	if err != nil {
		return zotigosession.ImageRef{}, false, err
	}
	if !inStore {
		return zotigosession.ImageRef{}, false, nil
	}
	image, ok := referencedMessageImage(items, blobPath)
	if !ok || !sessionlog.IsAllowedMessageImageMimeType(image.MimeType) {
		return zotigosession.ImageRef{}, false, nil
	}
	ref := zotigosession.ImageRef{
		SessionID: sessionID,
		Name:      name,
		BlobPath:  image.BlobPath,
		MimeType:  image.MimeType,
		SizeBytes: image.SizeBytes,
		Width:     image.Width,
		Height:    image.Height,
		CreatedAt: time.Now().UTC(),
	}
	if err := store.PutImageRefs(ctx, []zotigosession.ImageRef{ref}); err != nil {
		return zotigosession.ImageRef{}, false, err
	}
	return ref, true, nil
}

func referencedMessageImage(items []zotigosession.DisplayItem, blobPath string) (zotigosession.DisplayCommandImage, bool) {
	for _, item := range items {
		if item.Command == nil || (item.Command.Type != wire.CommandMessage && item.Command.Type != wire.CommandSteering) {
			continue
		}
		for _, img := range item.Command.Images {
			if filepath.Clean(img.BlobPath) == filepath.Clean(blobPath) {
				return img, true
			}
		}
	}
	return zotigosession.DisplayCommandImage{}, false
}

type rootDirStore interface {
	RootDir() string
}

type imageRefStore interface {
	rootDirStore
	PutImageRefs(context.Context, []zotigosession.ImageRef) error
	DeleteImageRefs(context.Context, string, []string) error
	GetImageRef(context.Context, string, string) (zotigosession.ImageRef, bool, error)
}

func (h *handler) sessionStoreRoot() string {
	store, ok := h.store.(rootDirStore)
	if !ok || store == nil {
		return ""
	}
	return store.RootDir()
}

func (h *handler) imageStore() (imageRefStore, bool) {
	store, ok := h.store.(imageRefStore)
	return store, ok && store != nil
}

func (h *handler) putMessageImageRefs(ctx context.Context, refs []zotigosession.ImageRef) error {
	if len(refs) == 0 {
		return nil
	}
	store, ok := h.imageStore()
	if !ok {
		return errors.New("image index is not configured")
	}
	return store.PutImageRefs(ctx, refs)
}

func (h *handler) deleteMessageImageRefs(ctx context.Context, sessionID string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	store, ok := h.imageStore()
	if !ok {
		return nil
	}
	return store.DeleteImageRefs(ctx, sessionID, names)
}
