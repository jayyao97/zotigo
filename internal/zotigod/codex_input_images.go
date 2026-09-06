package zotigod

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	zotigosession "github.com/jayyao97/zotigo/core/session"
)

// Imported Codex localImage paths belong to the daemon host. Resolve only an
// image already recorded in this session; never accept a filesystem path from
// the HTTP caller. Existing imports can be displayed without rewriting history.
func (h *handler) handleCodexInputImage(w http.ResponseWriter, r *http.Request, sessionID, name string) {
	parts := strings.Split(strings.TrimPrefix(name, "codex-input-"), "-")
	if len(parts) != 2 {
		writeAPIError(w, http.StatusNotFound, "image not found")
		return
	}
	sequence, seqErr := strconv.ParseUint(parts[0], 10, 64)
	index, indexErr := strconv.Atoi(parts[1])
	if seqErr != nil || indexErr != nil || sequence == 0 || index < 0 {
		writeAPIError(w, http.StatusNotFound, "image not found")
		return
	}
	items, _, err := h.items.LoadItems(r.Context(), sessionID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "could not load attachment")
		return
	}
	var source string
	for _, item := range items {
		if item.Sequence != sequence || item.Type != zotigosession.DisplayItemUserMessage || index >= len(item.Content) {
			continue
		}
		if image := item.Content[index].Image; image != nil {
			source = image.URL
		}
		break
	}
	if !filepath.IsAbs(source) {
		writeAPIError(w, http.StatusNotFound, "image not found")
		return
	}
	// Non-blocking open prevents a replaced attachment path pointing at a FIFO
	// from occupying the request indefinitely. Only bounded regular images serve.
	file, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "image unavailable")
		return
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxMessageImageBytes {
		writeAPIError(w, http.StatusNotFound, "image unavailable")
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, maxMessageImageBytes+1))
	if err != nil || len(data) > maxMessageImageBytes {
		writeAPIError(w, http.StatusNotFound, "image unavailable")
		return
	}
	mimeType := http.DetectContentType(data)
	if !isAllowedMessageImageMimeType(mimeType) {
		writeAPIError(w, http.StatusNotFound, "image unavailable")
		return
	}
	if _, _, err := validateMessageImageData(mimeType, data); err != nil {
		writeAPIError(w, http.StatusNotFound, "image unavailable")
		return
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
