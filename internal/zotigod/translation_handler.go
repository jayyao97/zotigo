package zotigod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/services"
	"github.com/openai/openai-go/v3"
	"google.golang.org/genai"
)

func (h *handler) handleTranslation(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, 405, "method not allowed")
		return
	}
	var input struct {
		Text           string `json:"text"`
		TargetLanguage string `json:"target_language"`
		Profile        string `json:"profile,omitempty"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeAPIError(w, 400, "invalid translation request")
		return
	}
	if strings.TrimSpace(input.Text) == "" || utf8.RuneCountInString(input.Text) > 8000 || (input.TargetLanguage != "zh-CN" && input.TargetLanguage != "en") {
		writeAPIError(w, 400, "provide 1–8000 characters and target_language zh-CN or en")
		return
	}
	// Translation needs configuration, not decorated runtime state: decorating
	// a live session reads its entire display log to derive activity and usage.
	session, ok, err := h.storedSession(r.Context(), id)
	if err != nil {
		writeAPIError(w, 500, "could not load session")
		return
	}
	if !ok {
		session, ok = h.registry.Get(id)
	}
	if !ok {
		writeAPIError(w, 404, "session not found")
		return
	}
	workDir, requestedProfile := session.WorkingDirectory, session.ProfileName
	if strings.TrimSpace(input.Profile) != "" {
		// Settings lists host-global profiles; project overrides must not silently
		// change the model or credentials of an explicitly selected profile.
		workDir, requestedProfile = "", input.Profile
	}
	cfg, err := config.NewManager().LoadForDir(workDir)
	if err != nil {
		writeAPIError(w, 500, "could not load translation profile")
		return
	}
	profileName, profile, err := cfg.ResolveProfile(requestedProfile)
	if err != nil {
		writeAPIError(w, 400, "translation profile not found; choose an available profile in Settings")
		return
	}
	provider, err := providers.NewProvider(profile)
	if err != nil {
		writeAPIError(w, 502, "could not initialize translation provider")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	translated, err := services.TranslateText(ctx, provider, input.Text, input.TargetLanguage)
	if err != nil {
		status, reason := translationFailure(err)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status, reason = 504, "translation timed out after 30 seconds; try a faster profile in Settings"
		}
		writeAPIError(w, status, fmt.Sprintf("%s (profile: %s; model: %s)", reason, profileName, profile.Model))
		return
	}
	writeAPIJSON(w, 200, map[string]string{"text": translated, "profile": profileName, "model": profile.Model})
}

// Report actionable categories without forwarding provider bodies, which may
// contain credentials, source text or internal URLs.
func translationFailure(err error) (int, string) {
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
		return 502, "translation provider rejected authentication or access; check the profile credentials"
	case 429:
		return 502, "translation provider rate limit or quota reached; retry later or select another profile"
	case 400, 404, 422:
		return 502, "translation provider rejected the model or request settings; check the selected profile"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 504, "translation provider timed out; retry or select another profile"
	}
	return 502, "translation provider failed; check connectivity and the selected profile, then retry"
}
