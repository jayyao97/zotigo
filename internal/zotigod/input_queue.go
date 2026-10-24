package zotigod

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/workspace"
)

type queuedInputResponse struct {
	publicCommandResponse
	State                 string `json:"state"`
	Revision              int64  `json:"revision"`
	Editable              bool   `json:"editable"`
	ImageEditingSupported bool   `json:"image_editing_supported"`
}

func (h *handler) handleInputQueue(w http.ResponseWriter, r *http.Request, id string) {
	if h.catalog == nil {
		writeAPIError(w, 503, "queue storage unavailable")
		return
	}
	if r.Method == http.MethodPost {
		_, found, err := h.catalogRuntime(r.Context(), id)
		if err != nil {
			writeAPIError(w, 500, "read session")
			return
		}
		if !found {
			writeAPIError(w, 404, "session not found")
			return
		}
		var req submitMessageRequest
		if err := readRequiredLimitedJSON(r, &req, maxMessageRequestBytes); err != nil {
			writeAPIError(w, 400, err.Error())
			return
		}
		images, err := validateMessageImages(req.Images)
		if err != nil {
			writeAPIError(w, 400, err.Error())
			return
		}
		text := strings.TrimSpace(req.Text)
		if req.Path != nil || text == "" && len(images) == 0 {
			writeAPIError(w, 400, "message requires text or images; paths are not accepted")
			return
		}
		if len(req.Skills) > 0 {
			selected, err := h.resolveSessionSkills(r.Context(), id, req.Skills)
			if err != nil {
				writeAPIError(w, 400, err.Error())
				return
			}
			req.Skills = make([]string, len(selected))
			for i, skill := range selected {
				req.Skills[i] = skill.Name
			}
		}
		unlock := h.queueOps.lock(id)
		if unavailable, err := h.ensureSessionActivatable(r.Context(), id); err != nil || unavailable != "" {
			unlock()
			writeAPIError(w, 409, "session is unavailable")
			return
		}
		if req.ClientMessageID != "" {
			existing, err := h.catalog.QueuedInputByID(r.Context(), id, strings.TrimSpace(req.ClientMessageID))
			if err != nil {
				unlock()
				writeAPIError(w, 500, err.Error())
				return
			}
			if existing != nil {
				var command commandResponse
				err = json.Unmarshal(existing.Payload, &command)
				if err == nil {
					err = h.hydrateQueuedImages(&command)
				}
				incoming := commandResponse{Message: &messageCommandPayload{Text: text, Skills: req.Skills}}
				for _, image := range images {
					incoming.Message.Images = append(incoming.Message.Images, commandImageData{MimeType: image.MimeType, SizeBytes: image.SizeBytes, Width: image.Width, Height: image.Height, DataBase64: base64.StdEncoding.EncodeToString(image.Data)})
				}
				unlock()
				if err != nil {
					writeAPIError(w, 500, err.Error())
					return
				}
				if !sameCommandInput(command, incoming) {
					writeAPIError(w, 409, errCommandIDConflict.Error())
					return
				}
				result := publicCommandFromCommand(command)
				result.Type = "queued"
				writeAPIJSON(w, 200, result)
				return
			}
		}
		rows, err := h.catalog.QueuedInputs(r.Context(), id)
		if err != nil {
			unlock()
			writeAPIError(w, 500, err.Error())
			return
		}
		if err := validateQueueCapacity(rows, text, images); err != nil {
			unlock()
			writeAPIError(w, 400, err.Error())
			return
		}
		command, storedImages, refs, err := h.prepareSessionInputCommand(r.Context(), id, strings.TrimSpace(req.ClientMessageID), text, images, req.Skills, false)
		if err == nil {
			// The queue retains file references, never another copy of image base64.
			for i := range command.Message.Images {
				command.Message.Images[i].DataBase64 = ""
			}
			var data []byte
			data, err = json.Marshal(command)
			if err == nil {
				err = h.catalog.QueueInput(r.Context(), id, command.ID, data)
			}
		}
		if err != nil {
			_ = h.deleteMessageImageRefs(context.Background(), id, imageRefNames(refs))
			cleanupMessageImageBlobs(storedImages)
		}
		unlock()
		if err != nil {
			writeAPIError(w, 500, err.Error())
			return
		}
		result := publicCommandFromCommand(command)
		result.Type = "queued"
		writeAPIJSON(w, http.StatusCreated, result)
		go func() { _ = h.dispatchQueuedInputs(id, false, false) }()
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, POST")
		writeAPIError(w, 405, "method not allowed")
		return
	}
	rows, err := h.catalog.QueuedInputs(r.Context(), id)
	if err != nil {
		writeAPIError(w, 500, err.Error())
		return
	}
	result := make([]queuedInputResponse, 0, len(rows))
	for _, row := range rows {
		var command commandResponse
		if err := json.Unmarshal(row.Payload, &command); err != nil {
			writeAPIError(w, 500, "invalid queued input")
			return
		}
		result = append(result, queuedInputResponse{publicCommandFromCommand(command), row.State, row.Revision, row.BatchID == "" && row.State != "sending", true})
	}
	writeAPIJSON(w, 200, map[string]any{"items": result})
}

func (h *handler) handleQueuedInput(w http.ResponseWriter, r *http.Request) {
	id, itemID := r.PathValue("id"), r.PathValue("item")
	if h.catalog == nil {
		writeAPIError(w, 503, "queue storage unavailable")
		return
	}
	if r.Method != http.MethodPatch && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "PATCH, DELETE")
		writeAPIError(w, 405, "method not allowed")
		return
	}
	var req struct {
		Text         string `json:"text"`
		Revision     int64  `json:"revision"`
		ImageIndices *[]int `json:"image_indices"`
	}
	if err := readRequiredLimitedJSON(r, &req, maxMessageRequestBytes); err != nil {
		writeAPIError(w, 400, err.Error())
		return
	}
	unlock := h.queueOps.lock(id)
	defer unlock()
	rows, err := h.catalog.QueuedInputs(r.Context(), id)
	if err != nil {
		writeAPIError(w, 500, err.Error())
		return
	}
	for _, row := range rows {
		if row.ID != itemID {
			continue
		}
		var command commandResponse
		if err := json.Unmarshal(row.Payload, &command); err != nil || command.Message == nil {
			writeAPIError(w, 500, "invalid queued input")
			return
		}
		command.Message.Text = strings.TrimSpace(req.Text)
		if r.Method == http.MethodPatch && req.ImageIndices != nil {
			images := make([]commandImageData, 0, len(*req.ImageIndices))
			seen := make(map[int]bool)
			for _, index := range *req.ImageIndices {
				if index < 0 || index >= len(command.Message.Images) || seen[index] {
					writeAPIError(w, 400, "invalid image_indices")
					return
				}
				seen[index] = true
				images = append(images, command.Message.Images[index])
			}
			command.Message.Images = images
		}
		if r.Method == http.MethodPatch && command.Message.Text == "" && len(command.Message.Images) == 0 {
			writeAPIError(w, 400, "message requires text or images")
			return
		}
		if r.Method == http.MethodPatch {
			other := make([]workspace.QueuedInput, 0, len(rows))
			for _, candidate := range rows {
				if candidate.ID != itemID {
					other = append(other, candidate)
				}
			}
			images := make([]messageImage, 0, len(command.Message.Images))
			for _, image := range command.Message.Images {
				images = append(images, messageImage{SizeBytes: image.SizeBytes})
			}
			if err := validateQueueCapacity(other, command.Message.Text, images); err != nil {
				writeAPIError(w, 400, err.Error())
				return
			}
		}
		data, err := json.Marshal(command)
		if err == nil {
			err = h.catalog.UpdateQueuedInput(r.Context(), id, itemID, req.Revision, data, r.Method == http.MethodDelete)
		}
		if errors.Is(err, workspace.ErrQueueConflict) {
			writeAPIError(w, 409, err.Error())
			return
		}
		if err != nil {
			writeAPIError(w, 500, err.Error())
			return
		}
		writeAPIJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	writeAPIError(w, 404, "queued input not found")
}

func (h *handler) handleSendQueuedInput(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeAPIError(w, 405, "method not allowed")
		return
	}
	if h.catalog == nil {
		writeAPIError(w, 503, "queue storage unavailable")
		return
	}
	var req struct {
		Revision int64 `json:"revision"`
	}
	if err := readRequiredLimitedJSON(r, &req, maxMessageRequestBytes); err != nil {
		writeAPIError(w, 400, err.Error())
		return
	}
	id, itemID := r.PathValue("id"), r.PathValue("item")
	unlock := h.queueOps.lock(id)
	defer unlock()
	// Keep admission alive if the caller disconnects; a lost HTTP response must
	// not turn an already accepted input into an editable queue entry.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	row, err := h.catalog.QueuedInputByID(ctx, id, itemID)
	if err != nil {
		writeAPIError(w, 500, err.Error())
		return
	}
	if row == nil {
		writeAPIError(w, 404, "queued input not found")
		return
	}
	if row.State == "sent" {
		writeAPIJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	if row.Revision != req.Revision || row.BatchID != "" || (row.State != "queued" && row.State != "paused") {
		writeAPIError(w, 409, workspace.ErrQueueConflict.Error())
		return
	}
	if _, err = h.ensureSessionRunning(ctx, id); err == nil && !h.ensureWorkerOnline(ctx, id) {
		err = errWorkerOffline
	}
	if err == nil {
		row.BatchID = "queue_" + uuid.NewString()
		err = h.catalog.ClaimQueuedInput(ctx, id, itemID, req.Revision, row.BatchID)
		if err == nil {
			err = h.submitQueuedInputs(ctx, id, []workspace.QueuedInput{*row}, false)
		}
	}
	if err != nil {
		writeAPIError(w, 409, err.Error())
		return
	}
	writeAPIJSON(w, 200, map[string]bool{"ok": true})
}

func (h *handler) handleResumeInputQueue(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeAPIError(w, 405, "method not allowed")
		return
	}
	if err := h.dispatchQueuedInputs(id, true, false); err != nil {
		writeAPIError(w, 409, err.Error())
		return
	}
	writeAPIJSON(w, 200, map[string]bool{"ok": true})
}

// Called after idle notification, outside the worker receive loop: admission
// waits for that same loop to deliver the acknowledgement.
func (h *handler) dispatchQueuedInputs(id string, resume, afterTurn bool) (resultErr error) {
	if h.catalog == nil {
		return fmt.Errorf("queue storage unavailable")
	}
	unlock := h.queueOps.lock(id)
	defer unlock()
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, h.catalog.PauseQueuedInputs(context.Background(), id))
			if h.logger != nil {
				h.logger.Printf("dispatch queued input: session=%s error=%v", id, resultErr)
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	queued, err := h.catalog.QueuedInputs(ctx, id)
	if err != nil || len(queued) == 0 {
		return err
	}
	// An acknowledgement may be lost even though the worker durably admitted
	// the batch. Reconcile it before checking whether a turn is still active.
	items, _, err := h.items.LoadItems(ctx, id)
	if err != nil {
		return err
	}
	for _, row := range queued {
		if row.BatchID == "" {
			continue
		}
		for _, item := range items {
			if item.ID == row.BatchID && item.Command != nil && (item.Command.Type == sessionCommandMessage || item.Command.Type == sessionCommandSteering) {
				if err := h.catalog.FinishQueuedBatch(ctx, id, row.BatchID, true); err != nil {
					return err
				}
				break
			}
		}
	}
	if lastOpenTurnID(items) != "" || hasPendingMessageCommand(items) || hasPendingHumanRequest(items) {
		if resume {
			return errActiveTurn
		}
		return nil
	}
	if afterTurn && !resume {
		for i := len(items) - 1; i >= 0; i-- {
			switch items[i].Type {
			case zotigosession.DisplayItemTurnFailed, zotigosession.DisplayItemTurnInterrupted:
				return h.catalog.PauseQueuedInputs(ctx, id)
			case zotigosession.DisplayItemTurnCompleted:
				i = -1
			}
		}
	}
	if h.workers.InputStopping(id) {
		if resume {
			return errWorkerInputStopping
		}
		return nil
	}
	rows, err := h.catalog.QueuedInputs(ctx, id)
	if err != nil || len(rows) == 0 {
		return err
	}
	if !resume {
		for _, row := range rows {
			if row.State != "queued" {
				return nil
			}
		}
	}
	if _, err = h.ensureSessionRunning(ctx, id); err != nil {
		return err
	}
	if !h.ensureWorkerOnline(ctx, id) {
		return errWorkerOffline
	}
	rows, err = h.catalog.ClaimQueuedInputs(ctx, id, "queue_"+uuid.NewString(), resume)
	if err != nil || len(rows) == 0 {
		return err
	}
	return h.submitQueuedInputs(ctx, id, rows, true)
}

func (h *handler) submitQueuedInputs(ctx context.Context, id string, rows []workspace.QueuedInput, startOnly bool) error {
	command, err := mergeQueuedInputs(rows)
	possiblyAdmitted := false
	if err == nil {
		err = h.hydrateQueuedImages(&command)
	}
	if err == nil {
		var submission *workerInputSubmission
		operationUnlock := h.sessionOps.lock(id)
		if unavailable, checkErr := h.ensureSessionActivatable(ctx, id); checkErr != nil {
			err = checkErr
		} else if unavailable != "" {
			err = errSessionUnavailable
		} else {
			submission, err = h.workers.BeginInput(id, workerInputRequest{Command: command, StartOnly: startOnly})
			if err == nil {
				submission.OnFinish(h.sessionOps.reserveInput(id))
			}
		}
		operationUnlock()
		if err == nil {
			possiblyAdmitted = true
			_, err = submission.Await(ctx)
			var rejected *workerInputError
			if errors.As(err, &rejected) {
				// A worker reply ends this admission attempt. Reconcile generic
				// failures against its synchronous durable admission record.
				// Transport timeouts have no such boundary and remain uncertain.
				switch rejected.Code {
				case "no_active_turn", "active_turn", "turn_mismatch", "input_policy_mismatch":
					possiblyAdmitted = false
				case "input_failed", "command_id_conflict":
					items, _, loadErr := h.items.LoadItems(context.Background(), id)
					if loadErr != nil {
						err = errors.Join(err, loadErr)
						break
					}
					possiblyAdmitted = false
					for _, item := range items {
						if item.ID == rows[0].BatchID && item.Command != nil &&
							(item.Command.Type == sessionCommandMessage || item.Command.Type == sessionCommandSteering) {
							possiblyAdmitted = true
							err = nil
							break
						}
					}
				}
			}
		}
	}
	if err != nil && !possiblyAdmitted {
		releaseErr := h.catalog.ReleaseQueuedBatch(context.Background(), id, rows[0].BatchID)
		// A concurrent ordinary send may win the idle boundary. Leave the queue
		// eligible for that turn's completion instead of treating it as failure.
		if startOnly && errors.Is(err, errActiveTurn) && releaseErr == nil {
			return nil
		}
		return errors.Join(err, releaseErr)
	}
	finishErr := h.catalog.FinishQueuedBatch(context.Background(), id, rows[0].BatchID, err == nil)
	return errors.Join(err, finishErr)
}

func (h *handler) hydrateQueuedImages(command *commandResponse) error {
	if command.Message == nil {
		return fmt.Errorf("queued input has no message")
	}
	images := make([]zotigosession.DisplayCommandImage, 0, len(command.Message.Images))
	for _, image := range command.Message.Images {
		images = append(images, zotigosession.DisplayCommandImage{MimeType: image.MimeType, SizeBytes: image.SizeBytes, Width: image.Width, Height: image.Height, BlobPath: image.BlobPath})
	}
	var err error
	command.Message.Images, err = commandImagesFromDisplay(images, h.sessionStoreRoot())
	return err
}

func validateQueueCapacity(rows []workspace.QueuedInput, text string, images []messageImage) error {
	if len(rows) >= 100 {
		return fmt.Errorf("queue contains too many messages (maximum 100)")
	}
	size, count := len(text), len(images)
	for _, image := range images {
		size += base64.StdEncoding.EncodedLen(image.SizeBytes)
	}
	for _, row := range rows {
		var command commandResponse
		if err := json.Unmarshal(row.Payload, &command); err != nil {
			return err
		}
		if command.Message == nil {
			return fmt.Errorf("invalid queued input")
		}
		size += len(command.Message.Text)
		count += len(command.Message.Images)
		for _, image := range command.Message.Images {
			size += base64.StdEncoding.EncodedLen(image.SizeBytes)
		}
	}
	if count > maxMessageImages || size > maxMessageRequestBytes-4096 {
		return fmt.Errorf("combined queued input exceeds the message attachment or size limit")
	}
	return nil
}

func mergeQueuedInputs(rows []workspace.QueuedInput) (commandResponse, error) {
	command := commandResponse{ID: rows[0].BatchID, Type: sessionCommandMessage, CreatedAt: time.Now().UTC(), Message: &messageCommandPayload{}}
	texts := []string{}
	seen := map[string]bool{}
	for _, row := range rows {
		var input commandResponse
		if err := json.Unmarshal(row.Payload, &input); err != nil {
			return command, err
		}
		if input.Message == nil {
			return command, fmt.Errorf("queued input %s has no message", row.ID)
		}
		if input.Message.Text != "" {
			texts = append(texts, input.Message.Text)
		}
		command.Message.Images = append(command.Message.Images, input.Message.Images...)
		for _, name := range input.Message.Skills {
			if !seen[name] {
				command.Message.Skills = append(command.Message.Skills, name)
				seen[name] = true
			}
		}
	}
	command.Message.Text = strings.Join(texts, "\n\n")
	return command, nil
}
