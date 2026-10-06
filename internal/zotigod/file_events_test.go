package zotigod

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileEventsFollowAtomicSaveAndRecreation(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "opened.txt")
	if err := os.WriteFile(file, []byte("initial"), 0600); err != nil {
		t.Fatal(err)
	}
	h := newHandler(newSessionRegistry(), &fakeDisplayItemSource{}, handlerOptions{})
	server := httptest.NewServer(h)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body, _ := json.Marshal(map[string]any{"files": []fileRequest{{Path: file, ExplicitOpen: true}}})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/files/events", bytes.NewReader(body))
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != 200 {
		t.Fatalf("status %d", response.StatusCode)
	}
	events := make(chan string, 10)
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "data:") {
				events <- scanner.Text()
			}
		}
	}()
	next := func(kind string) {
		t.Helper()
		select {
		case event := <-events:
			if !strings.Contains(event, `"type":"`+kind+`"`) {
				t.Fatalf("unexpected event %s", event)
			}
			if kind == "changed" && !strings.Contains(event, file) {
				t.Fatalf("missing file: %s", event)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for " + kind)
		}
	}
	next("ready")
	// Unrelated siblings must not invalidate the opened file.
	if err := os.WriteFile(filepath.Join(dir, "other"), []byte("unrelated"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		t.Fatalf("unrelated event: %s", event)
	case <-time.After(200 * time.Millisecond):
	}
	if err := os.WriteFile(file, []byte("written"), 0600); err != nil {
		t.Fatal(err)
	}
	next("changed")
	tmp := filepath.Join(dir, "replacement")
	if err := os.WriteFile(tmp, []byte("atomic"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, file); err != nil {
		t.Fatal(err)
	}
	next("changed")
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	next("changed")
	if err := os.WriteFile(file, []byte("recreated"), 0600); err != nil {
		t.Fatal(err)
	}
	next("changed")
	cancel()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("unexpected pending event")
		}
	case <-time.After(time.Second):
		t.Fatal("stream did not close")
	}
}

func TestFileEventsValidateScopeAndMissingPaths(t *testing.T) {
	h := newHandler(newSessionRegistry(), &fakeDisplayItemSource{}, handlerOptions{})
	for _, body := range []string{`{"files":[]}`, `{"files":[{"path":"relative"}]}`} {
		response := requestCatalog(t, h, "POST", "/files/events", body)
		if response.Code != 400 {
			t.Fatalf("status %d", response.Code)
		}
	}
	body, _ := json.Marshal(map[string]any{"files": []fileRequest{{Path: filepath.Join(t.TempDir(), "missing")}}})
	response := requestCatalog(t, h, "POST", "/files/events", string(body))
	if response.Code != 403 {
		t.Fatalf("outside root status %d", response.Code)
	}
}
