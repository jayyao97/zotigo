package zotigod

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/google/uuid"
)

const maxAttachmentBytes = 20 << 20

// Uploads are confined to one session's working directory, never a client path.
// The content digest makes retries reuse a file without overwriting user edits.
func (h *handler) handleAttachmentUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, 405, "method not allowed")
		return
	}
	id, name := r.URL.Query().Get("sessionId"), r.URL.Query().Get("name")
	if name == "" || len(name) > 180 || strings.ContainsAny(name, "/\\\x00\r\n") || name == "." || name == ".." {
		writeAPIError(w, 400, "invalid attachment name")
		return
	}
	unlock := h.sessionOps.lock(id)
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	if unavailable, err := h.ensureSessionActivatable(r.Context(), id); err != nil || unavailable != "" {
		writeAPIError(w, 409, "session unavailable")
		return
	}
	session, ok, err := h.attachmentSession(r.Context(), id)
	if err != nil {
		writeAPIError(w, 500, "could not load session")
		return
	}
	if !ok {
		writeAPIError(w, 404, "session not found")
		return
	}
	cwd := session.WorkingDirectory
	if cwd == "" {
		writeAPIError(w, 409, "session has no working directory")
		return
	}
	root, err := os.OpenRoot(cwd)
	if err != nil {
		writeAPIError(w, 409, "working directory unavailable")
		return
	}
	defer func() { _ = root.Close() }()
	dir := filepath.Join("artifacts", "attachments", id)
	if err := root.MkdirAll(dir, 0700); err != nil {
		writeAPIError(w, 500, "could not create attachment directory")
		return
	}
	temp := filepath.Join(dir, ".upload-"+uuid.NewString())
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		writeAPIError(w, 500, "could not create attachment")
		return
	}
	defer func() { _ = f.Close(); _ = root.Remove(temp) }()
	// Network reads must not hold the runtime lifecycle lock: pause, callbacks
	// and deletion still need to progress while an upload is stalled.
	unlock()
	unlock = nil
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, hash), http.MaxBytesReader(w, r.Body, maxAttachmentBytes))
	closeErr := f.Close()
	if copyErr != nil {
		var sizeErr *http.MaxBytesError
		if errors.As(copyErr, &sizeErr) {
			writeAPIError(w, 413, "attachment exceeds 20 MiB")
		} else {
			writeAPIError(w, 400, "attachment upload interrupted")
		}
		return
	}
	if closeErr != nil {
		writeAPIError(w, 500, "could not save attachment")
		return
	}
	unlock = h.sessionOps.lock(id)
	if unavailable, err := h.ensureSessionActivatable(r.Context(), id); err != nil || unavailable != "" {
		writeAPIError(w, 409, "session unavailable")
		return
	}
	current, found, err := h.attachmentSession(r.Context(), id)
	if err != nil || !found || current.WorkingDirectory != cwd {
		writeAPIError(w, 409, "session changed during upload")
		return
	}
	contentDir := filepath.Join(dir, fmt.Sprintf("%x", hash.Sum(nil)))
	if err := root.MkdirAll(contentDir, 0700); err != nil {
		writeAPIError(w, 500, "could not create attachment directory")
		return
	}
	target := filepath.Join(contentDir, name)
	// Link publishes the complete file exclusively; a retry cannot replace it.
	if err := root.Link(temp, target); err != nil {
		if !errors.Is(err, os.ErrExist) {
			writeAPIError(w, 500, "could not publish attachment")
			return
		}
		old, err := root.OpenFile(target, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		if err != nil {
			writeAPIError(w, 409, "attachment already exists but is unavailable")
			return
		}
		info, statErr := old.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Size() != n {
			_ = old.Close()
			writeAPIError(w, 409, "attachment was modified")
			return
		}
		oldHash := sha256.New()
		_, err = io.Copy(oldHash, io.LimitReader(old, maxAttachmentBytes+1))
		_ = old.Close()
		if err != nil || fmt.Sprintf("%x", oldHash.Sum(nil)) != fmt.Sprintf("%x", hash.Sum(nil)) {
			writeAPIError(w, 409, "attachment was modified")
			return
		}
	}
	writeAPIJSON(w, 201, map[string]any{"path": filepath.Join(cwd, target), "name": name, "sizeBytes": n})
}

func (h *handler) attachmentSession(ctx context.Context, id string) (Session, bool, error) {
	if session, ok := h.registry.Get(id); ok {
		return session, true, nil
	}
	return h.storedSession(ctx, id)
}
