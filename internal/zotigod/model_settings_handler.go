package zotigod

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/openai/openai-go/v3"
	"google.golang.org/genai"
)

func decodeModelChange(w http.ResponseWriter, r *http.Request) (config.ModelSettingsChange, error) {
	var change config.ModelSettingsChange
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&change); err != nil {
		return change, errors.New("invalid model settings request")
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return change, errors.New("invalid model settings request")
	}
	return change, nil
}

func writeModelSettingsError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, config.ErrModelSettingsConflict) {
		status = http.StatusConflict
	}
	writeAPIError(w, status, err.Error())
}

func (h *handler) handleModelSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	m := config.NewManager()
	switch r.Method {
	case http.MethodGet:
		result, err := m.ReadModelSettings()
		if err != nil {
			writeModelSettingsError(w, err)
			return
		}
		writeAPIJSON(w, 200, result)
	case http.MethodPut:
		change, err := decodeModelChange(w, r)
		if err != nil {
			writeModelSettingsError(w, err)
			return
		}
		result, err := m.UpdateModelSettings(change)
		if err != nil {
			writeModelSettingsError(w, err)
			return
		}
		writeAPIJSON(w, 200, result)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeAPIError(w, 405, "method not allowed")
	}
}

func modelTestFailure(err error) string {
	var oa *openai.Error
	var an *anthropic.Error
	var ge genai.APIError
	status := 0
	switch {
	case errors.As(err, &oa):
		status = oa.StatusCode
	case errors.As(err, &an):
		status = an.StatusCode
	case errors.As(err, &ge):
		status = ge.Code
	}
	switch status {
	case 401, 403:
		return "authentication"
	case 429, 402:
		return "quota"
	case 400, 404, 422:
		return "model"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "connection"
}

func (h *handler) handleModelTest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeAPIError(w, 405, "method not allowed")
		return
	}
	change, err := decodeModelChange(w, r)
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	profile, err := config.NewManager().ModelTestProfile(change)
	if err != nil {
		writeModelSettingsError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	provider, err := providers.NewProvider(profile)
	if err != nil {
		writeAPIJSON(w, 200, map[string]any{"ok": false, "reason": modelTestFailure(err)})
		return
	}
	err = testModelConnection(ctx, provider)
	if err == nil {
		writeAPIJSON(w, 200, map[string]any{"ok": true})
		return
	}
	writeAPIJSON(w, 200, map[string]any{"ok": false, "reason": modelTestFailure(err)})
}

// testModelConnection owns the stream until cancellation and provider cleanup finish.
func testModelConnection(ctx context.Context, provider providers.Provider) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := provider.StreamChat(ctx, []protocol.Message{protocol.NewUserMessage("Reply with OK only.")}, nil)
	if err == nil {
		defer func() {
			// Cancel first, then drain the provider's terminal events before
			// returning; stopping consumption alone leaves its sender blocked.
			cancel()
			for range stream {
			}
		}()
		for {
			select {
			case <-ctx.Done():
				err = ctx.Err()
			case event, ok := <-stream:
				if !ok {
					err = errors.New("model stream ended without completion")
					break
				}
				if event.Type == protocol.EventTypeError {
					err = event.Error
					if err == nil {
						err = errors.New("model error")
					}
					break
				}
				if event.Type == protocol.EventTypeFinish {
					return nil
				}
				continue
			}
			break
		}
	}
	return err
}
