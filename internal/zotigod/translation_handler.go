package zotigod

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/services"
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
	cfg, err := config.NewManager().LoadForDir(session.WorkingDirectory)
	if err != nil {
		writeAPIError(w, 500, "could not load translation profile")
		return
	}
	_, profile, err := cfg.ResolveProfile(session.ProfileName)
	if err != nil {
		writeAPIError(w, 500, "could not resolve translation profile")
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
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			writeAPIError(w, 504, "translation timed out")
		} else {
			writeAPIError(w, 502, "translation failed")
		}
		return
	}
	writeAPIJSON(w, 200, map[string]string{"text": translated})
}
