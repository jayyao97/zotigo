package zotigod

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

func TestCodexWorkerReleasesStorageLockBeforeReportingFailure(t *testing.T) {
	t.Parallel()
	root := newTestStoreRoot(t)
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const sessionID = "codex-shutdown"
	now := time.Now().UTC()
	if err := store.Put(t.Context(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: sessionID, Agent: "codex", CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	type observation struct {
		request finishSessionRequest
		locked  bool
		err     error
	}
	finished := make(chan observation, 1)
	socketClosed := make(chan struct{})
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/workers/connect":
			conn, err := upgrader.Upgrade(w, r, http.Header{workerGenerationHeader: []string{"generation"}})
			if err != nil {
				t.Errorf("upgrade worker: %v", err)
				return
			}
			defer func() { _ = conn.Close() }()
			defer close(socketClosed)
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		case "/internal/sessions/" + sessionID + "/worker/finish":
			var result observation
			result.err = json.NewDecoder(r.Body).Decode(&result.request)
			if result.err == nil {
				result.locked, result.err = store.IsLocked(r.Context(), sessionID)
			}
			select {
			case <-socketClosed:
				t.Error("worker socket closed before structured failure report")
			default:
			}
			finished <- result
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	err = runCodexWorkerClient(t.Context(), codexWorkerConfig{
		workerClientConfig: workerClientConfig{SessionID: sessionID, DaemonURL: server.URL},
		SessionStoreRoot:   root, WorkingDirectory: t.TempDir(),
		SocketPath: filepath.Join(t.TempDir(), "missing.sock"),
	})
	if err == nil {
		t.Fatal("missing Codex socket should fail startup")
	}
	select {
	case result := <-finished:
		if result.err != nil || result.locked {
			t.Fatalf("storage lock during finish report: locked=%v err=%v", result.locked, result.err)
		}
		if result.request.Generation != "generation" || result.request.Error != err.Error() {
			t.Fatalf("startup failure was not preserved: %+v, returned %v", result.request, err)
		}
	default:
		t.Fatal("worker did not report its startup failure")
	}
}
