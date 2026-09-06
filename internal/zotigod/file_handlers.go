package zotigod

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"
)

// Serialize conflict checks and writes across all clients of this daemon.
var workspaceFileWrites sync.Mutex

type fileRequest struct {
	Path            string  `json:"path"`
	SessionID       string  `json:"sessionId"`
	BasePath        string  `json:"basePath"`
	BaseKind        string  `json:"baseKind"`
	Content         string  `json:"content"`
	ExpectedMtimeMs float64 `json:"expectedMtimeMs"`
}
type textFileSnapshot struct {
	Path      string  `json:"path"`
	Name      string  `json:"name"`
	Content   string  `json:"content"`
	SizeBytes int64   `json:"sizeBytes"`
	MtimeMs   float64 `json:"mtimeMs"`
	ReadOnly  bool    `json:"readOnly"`
}

func (h *handler) handleWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/files/capabilities" && r.Method == http.MethodGet {
		writeAPIJSON(w, http.StatusOK, map[string]bool{"read": true, "write": true})
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, 405, "method not allowed")
		return
	}
	var input fileRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 7*1024*1024)).Decode(&input); err != nil {
		writeAPIError(w, 400, "invalid file request")
		return
	}
	roots, err := h.workspaceFileRoots(r, input.SessionID)
	if err != nil {
		writeAPIError(w, 500, "could not load workspace roots")
		return
	}
	requested := input.Path
	if strings.HasPrefix(requested, "file:") {
		u, e := url.Parse(requested)
		if e != nil || u.Host != "" && u.Host != "localhost" {
			writeAPIError(w, 400, "invalid file URL")
			return
		}
		requested = u.Path
	}
	if !filepath.IsAbs(requested) {
		base := input.BasePath
		if input.BaseKind == "file" {
			base = filepath.Dir(base)
		}
		if !filepath.IsAbs(base) {
			writeAPIError(w, 400, "relative file link has no working directory")
			return
		}
		requested = filepath.Join(base, requested)
	}
	root, relative, resolved, err := openWorkspaceFileRoot(requested, roots)
	if err != nil {
		writeAPIError(w, 403, "path does not exist or is outside registered workspaces")
		return
	}
	defer func() { _ = root.Close() }()
	if r.URL.Path == "/files/save" {
		workspaceFileWrites.Lock()
		defer workspaceFileWrites.Unlock()
		snapshot, e := readWorkspaceText(root, relative, resolved)
		if e != nil || snapshot == nil || snapshot.ReadOnly {
			writeAPIError(w, 400, "only small UTF-8 text files can be saved")
			return
		}
		if snapshot.MtimeMs != input.ExpectedMtimeMs {
			writeAPIError(w, 409, "file changed on disk; reopen before saving")
			return
		}
		if len(input.Content) > 1024*1024 || strings.ContainsRune(input.Content, 0) {
			writeAPIError(w, 400, "file content is not editable text")
			return
		}
		file, e := root.OpenFile(relative, os.O_WRONLY, 0)
		if e != nil {
			writeAPIError(w, 400, "cannot open file for writing")
			return
		}
		_, e = file.WriteAt([]byte(input.Content), 0)
		if e == nil {
			e = file.Truncate(int64(len(input.Content)))
		}
		closeErr := file.Close()
		if e != nil || closeErr != nil {
			writeAPIError(w, 500, "file save failed")
			return
		}
		snapshot, e = readWorkspaceText(root, relative, resolved)
		if e != nil {
			writeAPIError(w, 500, "saved file could not be read")
			return
		}
		writeAPIJSON(w, 200, snapshot)
		return
	}
	snapshot, err := readWorkspaceText(root, relative, resolved)
	if err != nil {
		writeAPIError(w, 400, "file cannot be read")
		return
	}
	if snapshot == nil {
		writeAPIJSON(w, 200, map[string]any{"kind": "system", "path": resolved})
		return
	}
	writeAPIJSON(w, 200, map[string]any{"kind": "text", "file": snapshot})
}

func openWorkspaceFileRoot(requested string, roots []string) (*os.Root, string, string, error) {
	resolved, err := filepath.EvalSymlinks(requested)
	if err != nil {
		return nil, "", "", err
	}
	for _, candidate := range roots {
		parent, e := filepath.EvalSymlinks(candidate)
		if e != nil {
			continue
		}
		rel, e := filepath.Rel(parent, resolved)
		if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		root, e := os.OpenRoot(parent)
		if e == nil {
			return root, rel, resolved, nil
		}
	}
	return nil, "", "", errors.New("outside workspace")
}

func readWorkspaceText(root *os.Root, relative, resolved string) (*textFileSnapshot, error) {
	info, err := root.Stat(relative)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil
	}
	file, err := root.Open(relative)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err = file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 5*1024*1024 {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(file, 5*1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 5*1024*1024 || bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return nil, nil
	}
	return &textFileSnapshot{Path: resolved, Name: filepath.Base(resolved), Content: string(data), SizeBytes: int64(len(data)), MtimeMs: float64(info.ModTime().UnixNano()) / 1e6, ReadOnly: len(data) > 1024*1024}, nil
}

func (h *handler) workspaceFileRoots(r *http.Request, sessionID string) ([]string, error) {
	var roots []string
	if sessionID != "" {
		if session, ok := h.registry.Get(sessionID); ok && session.WorkingDirectory != "" {
			roots = append(roots, session.WorkingDirectory)
		} else if h.store != nil {
			if session, err := h.store.Get(r.Context(), sessionID); err == nil && session != nil {
				roots = append(roots, session.WorkingDirectory)
			}
		}
	}
	if h.catalog == nil {
		return roots, nil
	}
	projects, err := h.catalog.ListProjects(r.Context())
	if err != nil {
		return nil, err
	}
	for _, project := range projects {
		sources, err := h.catalog.ListSources(r.Context(), project.ID)
		if err != nil {
			return nil, fmt.Errorf("sources: %w", err)
		}
		for _, source := range sources {
			roots = append(roots, source.CanonicalPath)
		}
		workspaces, err := h.catalog.ListWorkspaces(r.Context(), project.ID, false)
		if err != nil {
			return nil, fmt.Errorf("workspaces: %w", err)
		}
		for _, workspace := range workspaces {
			roots = append(roots, workspace.RootPath)
		}
	}
	return roots, nil
}
