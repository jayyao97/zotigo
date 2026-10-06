package zotigod

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Each connection owns bounded, non-recursive parent watches. Watching the
// parent (rather than an inode) also catches editors' atomic-save replacements.
func (h *handler) handleFileEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, 405, "method not allowed")
		return
	}
	var input struct {
		Files []fileRequest `json:"files"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 320*1024)).Decode(&input); err != nil || len(input.Files) == 0 || len(input.Files) > 64 {
		writeAPIError(w, 400, "expected 1 to 64 files")
		return
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		writeAPIError(w, 503, "file watching is unavailable")
		return
	}
	defer func() { _ = watcher.Close() }()
	paths := make(map[string][]string)
	parents := make(map[string]bool)
	var unavailable []string
	for _, file := range input.Files {
		if !filepath.IsAbs(file.Path) || len(file.Path) > 4096 || strings.ContainsRune(file.Path, 0) {
			writeAPIError(w, 400, "expected an absolute file path")
			return
		}
		// Resolve the parent so a deleted leaf can still be subscribed on reconnect.
		parent, err := filepath.EvalSymlinks(filepath.Dir(file.Path))
		if err != nil {
			unavailable = append(unavailable, file.Path)
			continue
		}
		if !file.ExplicitOpen {
			roots, err := h.workspaceFileRoots(r, file.SessionID)
			if err != nil {
				writeAPIError(w, 500, "could not load workspace roots")
				return
			}
			root, _, _, err := openWorkspaceFileRoot(parent, roots)
			if err != nil {
				writeAPIError(w, 403, "file is outside workspace roots")
				return
			}
			_ = root.Close()
		}
		if !parents[parent] {
			if err := watcher.Add(parent); err != nil {
				unavailable = append(unavailable, file.Path)
				continue
			}
			parents[parent] = true
		}
		name := filepath.Join(parent, filepath.Base(file.Path))
		paths[name] = append(paths[name], file.Path)
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, 500, "streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	write := func(kind string, files []string) error {
		if files == nil {
			files = []string{}
		}
		data, err := json.Marshal(struct {
			Type  string   `json:"type"`
			Paths []string `json:"paths"`
		}{kind, files})
		if err != nil {
			return err
		}
		if err := setDisplayEventWriteDeadline(w); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return err
		}
		flusher.Flush()
		return clearDisplayEventWriteDeadline(w)
	}
	// The client rereads after ready, closing the initial read/subscribe gap and
	// recovering changes missed during a disconnected stream.
	if write("ready", unavailable) != nil {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	var timer *time.Timer
	var pendingTick <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	pending := make(map[string]bool)
	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if parents[event.Name] && event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				// Parent inode no longer represents the requested path; reconnect.
				return
			}
			for _, path := range paths[event.Name] {
				pending[path] = true
			}
			if len(pending) > 0 && pendingTick == nil {
				timer = time.NewTimer(100 * time.Millisecond)
				pendingTick = timer.C
			}
		case <-pendingTick:
			files := make([]string, 0, len(pending))
			for path := range pending {
				files = append(files, path)
			}
			sort.Strings(files)
			clear(pending)
			pendingTick = nil
			if write("changed", files) != nil {
				return
			}
		case _, ok := <-watcher.Errors:
			if !ok {
				return
			}
			// Overflow means some names were lost. Reconnect and reread snapshots.
			return
		case <-heartbeat.C:
			// No disk scans: this heartbeat only keeps network intermediaries alive.
			if writeDisplayEventComment(w, flusher, "heartbeat") != nil {
				return
			}
		}
	}
}
