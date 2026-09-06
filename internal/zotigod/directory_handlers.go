package zotigod

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
)

const directoryEntryLimit = 1000

type directoryEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Kind string `json:"kind"`
}

type directoryListing struct {
	Path       string           `json:"path"`
	ParentPath *string          `json:"parentPath"`
	Entries    []directoryEntry `json:"entries"`
	Truncated  bool             `json:"truncated"`
}

func (h *handler) handleDirectoryList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, 405, "method not allowed")
		return
	}
	var input struct {
		Path      string `json:"path"`
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&input); err != nil {
		writeAPIError(w, 400, "invalid directory request")
		return
	}
	selectSource := r.URL.Path == "/sources/directories"
	var roots []string
	var err error
	if selectSource {
		// Source registration already permits any directory accessible to the daemon
		// account. This endpoint reveals directory names only, never file contents.
		if input.Path == "" {
			input.Path, err = os.UserHomeDir()
		}
		roots = []string{filepath.VolumeName(input.Path) + string(filepath.Separator)}
	} else {
		roots, err = h.workspaceFileRoots(r, input.SessionID)
	}
	if err != nil {
		writeAPIError(w, 500, "could not load directory roots")
		return
	}
	listing := directoryListing{Entries: []directoryEntry{}}
	if input.Path == "" {
		seen := map[string]bool{}
		for _, candidate := range roots {
			resolved, e := filepath.EvalSymlinks(candidate)
			if e != nil || seen[resolved] {
				continue
			}
			seen[resolved] = true
			listing.Entries = append(listing.Entries, directoryEntry{Name: resolved, Path: resolved, Kind: "directory"})
			if len(listing.Entries) > directoryEntryLimit {
				listing.Truncated = true
				listing.Entries = listing.Entries[:directoryEntryLimit]
				break
			}
		}
	} else {
		if !filepath.IsAbs(input.Path) {
			writeAPIError(w, 400, "directory path must be absolute")
			return
		}
		root, relative, resolved, e := openWorkspaceFileRoot(input.Path, roots)
		if e != nil {
			writeAPIError(w, 403, "directory is missing or outside allowed roots")
			return
		}
		defer func() { _ = root.Close() }()
		info, e := root.Stat(relative)
		if e != nil || !info.IsDir() {
			writeAPIError(w, 400, "path is not a directory")
			return
		}
		directory, e := root.Open(relative)
		if e != nil {
			writeAPIError(w, 403, "directory cannot be read")
			return
		}
		defer func() { _ = directory.Close() }()
		entries, e := directory.ReadDir(directoryEntryLimit + 1)
		if e != nil && e != io.EOF {
			writeAPIError(w, 400, "directory cannot be read")
			return
		}
		listing.Path = resolved
		listing.Truncated = len(entries) > directoryEntryLimit
		if listing.Truncated {
			entries = entries[:directoryEntryLimit]
		}
		parent := filepath.Dir(resolved)
		if parent != resolved {
			parentRoot, _, _, parentErr := openWorkspaceFileRoot(parent, roots)
			if parentErr == nil {
				_ = parentRoot.Close()
				listing.ParentPath = &parent
			}
		}
		for _, entry := range entries {
			if r.Context().Err() != nil {
				return
			}
			kind := "unavailable"
			stat, statErr := root.Stat(filepath.Join(relative, entry.Name()))
			if statErr == nil {
				if stat.IsDir() {
					kind = "directory"
				} else if stat.Mode().IsRegular() {
					kind = "file"
				}
			}
			if selectSource && kind != "directory" {
				continue
			}
			listing.Entries = append(listing.Entries, directoryEntry{Name: entry.Name(), Path: filepath.Join(resolved, entry.Name()), Kind: kind})
		}
	}
	sort.Slice(listing.Entries, func(i, j int) bool {
		a, b := listing.Entries[i], listing.Entries[j]
		if (a.Kind == "directory") != (b.Kind == "directory") {
			return a.Kind == "directory"
		}
		return a.Name < b.Name
	})
	writeAPIJSON(w, 200, listing)
}
