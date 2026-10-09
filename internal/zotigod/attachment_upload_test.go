package zotigod

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
)

func TestScratchSessionsHaveIndependentPersistentDirectories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".zotigo")
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	writeTestProfileConfig(t, root)
	config, err := os.ReadFile(filepath.Join(root, "zotigo.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), config, 0600); err != nil {
		t.Fatal(err)
	}
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})
	var previous string
	for range 2 {
		rec := requestCatalog(t, handler, http.MethodPost, "/sessions", `{}`)
		if rec.Code != 201 {
			t.Fatalf("create scratch: %d %s", rec.Code, rec.Body)
		}
		var session Session
		decodeCatalogData(t, rec, &session)
		want := filepath.Join(root, "scratch", session.ID)
		if session.WorkingDirectory != want || want == previous {
			t.Fatalf("scratch cwd %q", session.WorkingDirectory)
		}
		previous = want
		if info, err := os.Stat(want); err != nil || !info.IsDir() {
			t.Fatalf("scratch directory: %v", err)
		}
		stored, err := store.Get(context.Background(), session.ID)
		if err != nil || stored.WorkingDirectory != want {
			t.Fatalf("stored cwd: %v %+v", err, stored)
		}
	}
}

func TestAttachmentUploadConfinesFilesAndRetriesWithoutOverwriting(t *testing.T) {
	cwd := t.TempDir()
	registry := newSessionRegistry()
	registry.Add(Session{ID: "session-1", WorkingDirectory: cwd})
	handler := newHandler(registry, &fakeDisplayItemSource{}, handlerOptions{publicAuthToken: "test-token"})
	upload := func(name string, body io.Reader, authorized bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/files/upload?sessionId=session-1&name="+url.QueryEscape(name), body)
		if authorized {
			req.Header.Set("Authorization", "Bearer test-token")
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if got := upload("clip.mp4", strings.NewReader("video bytes"), false); got.Code != 401 {
		t.Fatalf("unauthorized: %d", got.Code)
	}
	for _, name := range []string{"../outside", "a/b.mp4", "..", ""} {
		if got := upload(name, strings.NewReader("bytes"), true); got.Code != 400 {
			t.Fatalf("name %q: %d", name, got.Code)
		}
	}
	rec := upload("clip.mp4", strings.NewReader("video bytes"), true)
	if rec.Code != 201 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	var result struct {
		Path string `json:"path"`
	}
	decodeCatalogData(t, rec, &result)
	if !strings.HasPrefix(result.Path, filepath.Join(cwd, "artifacts", "attachments", "session-1")+string(filepath.Separator)) {
		t.Fatal(result.Path)
	}
	data, err := os.ReadFile(result.Path)
	if err != nil || string(data) != "video bytes" {
		t.Fatalf("stored: %q %v", data, err)
	}
	retry := upload("clip.mp4", strings.NewReader("video bytes"), true)
	if retry.Code != 201 {
		t.Fatalf("retry: %s", retry.Body)
	}
	var retryResult struct {
		Path string `json:"path"`
	}
	decodeCatalogData(t, retry, &retryResult)
	if retryResult.Path != result.Path {
		t.Fatalf("retry changed path: %s", retryResult.Path)
	}
	if err := os.WriteFile(result.Path, []byte("edited"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := upload("clip.mp4", strings.NewReader("video bytes"), true); got.Code != 409 {
		t.Fatalf("overwrite: %d", got.Code)
	}
	if got := upload("large", io.LimitReader(zeroReader{}, maxAttachmentBytes+1), true); got.Code != 413 {
		t.Fatalf("limit: %d", got.Code)
	}
	entries, err := os.ReadDir(filepath.Dir(result.Path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("partial files: %v %v", entries, err)
	}
	entries, err = os.ReadDir(filepath.Dir(filepath.Dir(result.Path)))
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("temporary uploads left behind: %v %v", entries, err)
	}
	// A directory symlink must not redirect an upload outside the working root.
	other := t.TempDir()
	registry.Add(Session{ID: "session-2", WorkingDirectory: other})
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(other, "artifacts")); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/files/upload?sessionId=session-2&name=x", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer test-token")
	blocked := httptest.NewRecorder()
	handler.ServeHTTP(blocked, req)
	if blocked.Code < 400 {
		t.Fatalf("symlink upload accepted: %d", blocked.Code)
	}
	entries, _ = os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("upload escaped working directory")
	}
}

type zeroReader struct{}

func TestAttachmentUploadToPersistedOfflineSession(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{ID: "offline-session", WorkingDirectory: cwd}}); err != nil {
		t.Fatal(err)
	}
	registry := newSessionRegistry()
	handler := newHandler(registry, storedDisplayItemSource{store: store}, handlerOptions{store: store})
	req := httptest.NewRequest(http.MethodPost, "/files/upload?sessionId=offline-session&name=notes.txt", strings.NewReader("offline upload"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("offline upload: %d %s", rec.Code, rec.Body)
	}
	var result struct {
		Path string `json:"path"`
	}
	decodeCatalogData(t, rec, &result)
	data, err := os.ReadFile(result.Path)
	if err != nil || string(data) != "offline upload" {
		t.Fatalf("file: %q %v", data, err)
	}
	if _, live := registry.Get("offline-session"); live {
		t.Fatal("upload must not activate the session")
	}
}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestAttachmentUploadAllowsLifecycleChangesWhileReceiving(t *testing.T) {
	for _, action := range []string{"archive", "delete-workspace"} {
		t.Run(action, func(t *testing.T) {
			handler, _, _, workspace := newCatalogSessionFixture(t)
			writeTestProfileConfig(t, workspace.RootPath)
			created := requestCatalog(t, handler, http.MethodPost, "/sessions", `{"workspace_id":`+quotedJSON(t, workspace.ID)+`}`)
			if created.Code != http.StatusCreated {
				t.Fatalf("create: %d %s", created.Code, created.Body)
			}
			var session Session
			decodeCatalogData(t, created, &session)
			reader, writer := io.Pipe()
			defer writer.Close()
			defer reader.Close()
			upload := httptest.NewRecorder()
			uploadDone := make(chan struct{})
			go func() {
				defer close(uploadDone)
				handler.ServeHTTP(upload, httptest.NewRequest(http.MethodPost, "/files/upload?sessionId="+session.ID+"&name=clip.mp4", reader))
			}()
			// A successful Write means the upload is reading its body. Keep the
			// pipe open to simulate a remote client whose network then stalls.
			if _, err := writer.Write([]byte("partial")); err != nil {
				t.Fatal(err)
			}
			changed := httptest.NewRecorder()
			changedDone := make(chan struct{})
			go func() {
				defer close(changedDone)
				path, body := "/sessions/"+session.ID+"/archive", "{}"
				if action == "delete-workspace" {
					path, body = "/workspaces/"+workspace.ID+"/delete", `{"confirmation":"Workspace"}`
				}
				handler.ServeHTTP(changed, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
			}()
			select {
			case <-changedDone:
			case <-time.After(3 * time.Second):
				_ = writer.Close()
				<-uploadDone
				<-changedDone
				t.Fatal("stalled upload blocked a session lifecycle operation")
			}
			if changed.Code != http.StatusOK {
				t.Errorf("lifecycle operation: %d %s", changed.Code, changed.Body)
			}
			_ = writer.Close()
			<-uploadDone
			if upload.Code != http.StatusConflict {
				t.Fatalf("upload published after %s: %d %s", action, upload.Code, upload.Body)
			}
			dir := filepath.Join(workspace.RootPath, "artifacts", "attachments", session.ID)
			entries, err := os.ReadDir(dir)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("upload left files after %s: %v", action, entries)
			}
		})
	}
}
