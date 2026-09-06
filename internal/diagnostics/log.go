// Package diagnostics stores bounded local operational logs, not session history.
package diagnostics

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const maxBytes = 5 * 1024 * 1024
const maxFiles = 128
const maxTotalBytes = 100 * 1024 * 1024

// Start redirects Go diagnostic logging until the returned function is called.
// Each process writes its own files; retention is shared by the whole component.
func Start(component string) func() {
	previous := log.Writer()
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Local logging unavailable: %v\n", err)
		return func() {}
	}
	w, err := newWriter(filepath.Join(home, ".zotigo", "logs", component), maxBytes, maxFiles, maxTotalBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Local logging unavailable: %v\n", err)
		return func() {}
	}
	var output io.Writer = w
	if component == "daemon" {
		output = io.MultiWriter(w, os.Stderr)
	}
	log.SetOutput(output)
	return func() { log.SetOutput(previous) }
}

type writer struct {
	mu       sync.Mutex
	dir      string
	path     string
	limit    int64
	keep     int
	budget   int64
	warnOnce sync.Once
}

func newWriter(dir string, limit int64, keep int, budget int64) (*writer, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return &writer{dir: dir, limit: limit, keep: keep, budget: budget}, nil
}

func (w *writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	entryLimit := min(w.limit, 64*1024)
	if int64(len(p)) > entryLimit {
		p = append(append([]byte(nil), p[:entryLimit-14]...), " [truncated]\n"...)
	}
	err := w.append(p)
	if err != nil {
		// A full/unwritable disk must not break a CLI turn or a daemon request.
		w.warnOnce.Do(func() { fmt.Fprintf(os.Stderr, "Local logging failed: %v\n", err) })
	}
	return n, nil
}

func (w *writer) append(p []byte) error {
	var file *os.File
	if w.path != "" {
		var err error
		file, err = os.OpenFile(w.path, os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if file != nil {
			info, err := file.Stat()
			if err != nil || info.Size()+int64(len(p)) > w.limit {
				_ = file.Close()
				file = nil
			}
		}
	}
	if file == nil {
		var err error
		file, err = os.CreateTemp(w.dir, fmt.Sprintf("run-%s-%d-*.log", time.Now().UTC().Format("20060102T150405"), os.Getpid()))
		if err != nil {
			return err
		}
		w.path = file.Name()
	}
	_, err := file.Write(p)
	closeErr := file.Close() // Never retain an FD to a file another process may prune.
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return w.prune()
}

func (w *writer) prune() error {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return err
	}
	var files []os.FileInfo
	var total int64
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "run-") || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		info, err := entry.Info()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			files = append(files, info)
			total += info.Size()
		}
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].ModTime().Equal(files[j].ModTime()) {
			return files[i].Name() < files[j].Name()
		}
		return files[i].ModTime().Before(files[j].ModTime())
	})
	for index, file := range files {
		if total <= w.budget && len(files)-index <= w.keep {
			break
		}
		if err := os.Remove(filepath.Join(w.dir, file.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
		total -= file.Size()
	}
	return nil
}

var _ io.Writer = (*writer)(nil)
