package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/gorilla/websocket"
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/runner"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigotransport "github.com/jayyao97/zotigo/core/transport"
	"github.com/jayyao97/zotigo/internal/hooks"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

func TestWorkerClientKeepaliveClosesUnresponsiveDaemon(t *testing.T) {
	serverConnReady := make(chan *websocket.Conn, 1)
	releaseServerConn := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := workerUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConnReady <- conn
		<-releaseServerConn
		_ = conn.Close()
	}))
	defer server.Close()
	defer close(releaseServerConn)

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial worker websocket: %v", err)
	}
	defer clientConn.Close()
	serverConn := <-serverConnReady
	defer serverConn.Close()

	writer := workerbase.NewWriter(clientConn, 10*time.Millisecond, 30*time.Millisecond)
	defer writer.Close()

	if _, _, err := clientConn.ReadMessage(); err == nil {
		t.Fatal("expected worker websocket read to fail after pong timeout")
	}
}

func TestReplayWorkerCommandsFetchesMultiplePages(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("limit"); got != strconv.Itoa(wire.MaxCommandsLimit) {
			t.Fatalf("expected limit %d, got %q", wire.MaxCommandsLimit, got)
		}
		offset := r.URL.Query().Get("offset")
		requests++
		switch offset {
		case "0":
			commands := make([]wire.Command, 0, wire.MaxCommandsLimit)
			for sequence := uint64(1); sequence <= wire.MaxCommandsLimit; sequence++ {
				commands = append(commands, wire.Command{
					ID:       fmt.Sprintf("cmd-%d", sequence),
					Sequence: sequence,
					Type:     "noop",
				})
			}
			writeJSON(w, http.StatusOK, wire.CommandsPage{
				Commands:   commands,
				NextCursor: strconv.Itoa(wire.MaxCommandsLimit),
				NextOffset: 1,
			})
		case "1":
			writeJSON(w, http.StatusOK, wire.CommandsPage{
				Commands: []wire.Command{{
					ID:       "cmd-201",
					Sequence: uint64(wire.MaxCommandsLimit + 1),
					Type:     "noop",
				}},
				NextCursor: strconv.Itoa(wire.MaxCommandsLimit + 1),
				NextOffset: 2,
			})
		default:
			t.Fatalf("unexpected offset %q", offset)
		}
	}))
	defer server.Close()

	cursor, err := replayWorkerCommands(context.Background(), server.Client(), server.URL, "sess-replay", &workerRuntime{}, workerbase.CommandCursor{})
	if err != nil {
		t.Fatalf("replay commands: %v", err)
	}
	if requests != 2 {
		t.Fatalf("expected 2 command fetches, got %d", requests)
	}
	if cursor.Sequence != uint64(wire.MaxCommandsLimit+1) || cursor.Offset != 2 {
		t.Fatalf("unexpected cursor after replay: %#v", cursor)
	}

	saved, err := workerbase.LoadCommandCursor(context.Background(), nil, "sess-replay")
	if err != nil {
		t.Fatalf("load saved cursor: %v", err)
	}
	if saved != cursor {
		t.Fatalf("expected saved cursor %#v, got %#v", cursor, saved)
	}
}

func TestWorkerRuntimeRejectsMalformedTypedCommand(t *testing.T) {
	runtime := &workerRuntime{}
	if err := runtime.HandleCommand(context.Background(), wire.Command{
		ID:       "cmd-1",
		Sequence: 1,
		Type:     wire.CommandMessage,
	}); err == nil || !strings.Contains(err.Error(), "invalid message command payload") {
		t.Fatalf("expected invalid message payload error, got %v", err)
	}
	if err := runtime.HandleCommand(context.Background(), wire.Command{
		ID:       "cmd-2",
		Sequence: 2,
		Type:     wire.CommandPause,
		Message:  &wire.MessageCommandPayload{Text: "wrong"},
		Pause:    &wire.PauseCommandPayload{Reason: wire.UserPauseReason},
	}); err == nil || !strings.Contains(err.Error(), "invalid pause command payload") {
		t.Fatalf("expected invalid pause payload error, got %v", err)
	}
}

func TestReplayWorkerCommandsDoesNotAdvanceMalformedCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, wire.CommandsPage{
			Commands: []wire.Command{{
				ID:       "cmd-1",
				Sequence: 1,
				Type:     wire.CommandMessage,
			}},
			NextCursor: "1",
			NextOffset: 1,
		})
	}))
	defer server.Close()

	cursor, err := replayWorkerCommands(context.Background(), server.Client(), server.URL, "sess-malformed", &workerRuntime{}, workerbase.CommandCursor{})
	if err == nil || !strings.Contains(err.Error(), "invalid message command payload") {
		t.Fatalf("expected malformed command error, got %v", err)
	}
	if cursor.Sequence != 0 || cursor.Offset != 0 {
		t.Fatalf("expected cursor not to advance, got %#v", cursor)
	}
	saved, err := workerbase.LoadCommandCursor(context.Background(), nil, "sess-malformed")
	if err != nil {
		t.Fatalf("load saved cursor: %v", err)
	}
	if saved.Sequence != 0 || saved.Offset != 0 {
		t.Fatalf("expected saved cursor not to advance, got %#v", saved)
	}
}

func TestLoadWorkerCommandCursorDoesNotSkipPendingMessage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	defer store.Close()
	sessionID := "sess-corrupt-cursor"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        sessionID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	item, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemUserMessage,
		Command: &zotigosession.DisplayCommand{
			Type: wire.CommandMessage,
			Text: "already applied",
		},
	})
	if err != nil {
		t.Fatalf("append command item: %v", err)
	}
	_ = item
	if err := os.MkdirAll(filepath.Dir(workerbase.CommandCursorPath(sessionID)), 0755); err != nil {
		t.Fatalf("create cursor dir: %v", err)
	}
	if err := os.WriteFile(workerbase.CommandCursorPath(sessionID), []byte("{"), 0644); err != nil {
		t.Fatalf("write corrupt cursor: %v", err)
	}

	cursor, err := workerbase.LoadCommandCursor(context.Background(), store, sessionID)
	if err != nil {
		t.Fatalf("load worker command cursor: %v", err)
	}
	if cursor.Sequence != 0 || cursor.Offset != 0 {
		t.Fatalf("expected corrupt cursor recovery not to skip pending message, got %#v", cursor)
	}
}

func TestLoadWorkerCommandCursorRecoversAppliedMessage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	defer store.Close()
	sessionID := "sess-applied-cursor"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        sessionID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	item, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemUserMessage,
		Command: &zotigosession.DisplayCommand{
			Type: wire.CommandMessage,
			Text: "already applied",
		},
	})
	if err != nil {
		t.Fatalf("append command item: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
	}); err != nil {
		t.Fatalf("append turn started: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(workerbase.CommandCursorPath(sessionID)), 0755); err != nil {
		t.Fatalf("create cursor dir: %v", err)
	}
	if err := os.WriteFile(workerbase.CommandCursorPath(sessionID), []byte("{"), 0644); err != nil {
		t.Fatalf("write corrupt cursor: %v", err)
	}

	cursor, err := workerbase.LoadCommandCursor(context.Background(), store, sessionID)
	if err != nil {
		t.Fatalf("load worker command cursor: %v", err)
	}
	if cursor.Sequence != item.Sequence || cursor.Offset != 0 {
		t.Fatalf("expected recovered applied message sequence %d, got %#v", item.Sequence, cursor)
	}
}

func TestLoadWorkerCommandCursorIgnoresPersistedSteering(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	defer store.Close()
	sessionID := "sess-pending-steering-cursor"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        sessionID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
	}); err != nil {
		t.Fatalf("append turn started: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemSteeringMessage,
		Role: string(protocol.RoleUser),
		Content: []zotigosession.DisplayContentPart{{
			Type: string(protocol.ContentTypeText),
			Text: "still pending",
		}},
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
		Command: &zotigosession.DisplayCommand{
			Type:   wire.CommandSteering,
			Text:   "still pending",
			TurnID: "turn-1",
		},
	}); err != nil {
		t.Fatalf("append steering command: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(workerbase.CommandCursorPath(sessionID)), 0755); err != nil {
		t.Fatalf("create cursor dir: %v", err)
	}
	if err := os.WriteFile(workerbase.CommandCursorPath(sessionID), []byte("{"), 0644); err != nil {
		t.Fatalf("write corrupt cursor: %v", err)
	}

	cursor, err := workerbase.LoadCommandCursor(context.Background(), store, sessionID)
	if err != nil {
		t.Fatalf("load worker command cursor: %v", err)
	}
	if cursor.Sequence != 0 || cursor.Offset != 0 {
		t.Fatalf("expected steering display item to be ignored, got %#v", cursor)
	}
}

func TestRecoverWorkerCommandCursorTracksDurableSteeringApplication(t *testing.T) {
	items := []zotigosession.DisplayItem{
		{Sequence: 1, Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
		{ID: "steer-1", Sequence: 2, Type: zotigosession.DisplayItemSessionCommand, Command: &zotigosession.DisplayCommand{Type: wire.CommandSteering, Text: "change", TurnID: "turn-1"}},
	}
	if got := workerbase.RecoverAppliedCommandSequence(items); got != 0 {
		t.Fatalf("pending steering advanced cursor to %d", got)
	}
	items = append(items, zotigosession.DisplayItem{
		ID: "steer-1", Sequence: 3, Type: zotigosession.DisplayItemSteeringMessage,
		Command: &zotigosession.DisplayCommand{Type: wire.CommandSteering, Text: "change", TurnID: "turn-1"},
	})
	if got := workerbase.RecoverAppliedCommandSequence(items); got != 2 {
		t.Fatalf("applied steering cursor = %d, want 2", got)
	}
}

func TestLoadWorkerCommandCursorDoesNotTreatAppliedImageSteeringAsCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	defer store.Close()
	sessionID := "sess-applied-image-steering-cursor"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        sessionID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
	}); err != nil {
		t.Fatalf("append turn started: %v", err)
	}
	_, err = store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemSteeringMessage,
		Role: string(protocol.RoleUser),
		Content: []zotigosession.DisplayContentPart{{
			Type: string(protocol.ContentTypeImage),
			Image: &zotigosession.DisplayMediaPart{
				MediaType: "image/png",
				SizeBytes: 1,
				Width:     1,
				Height:    1,
			},
		}},
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
		Command: &zotigosession.DisplayCommand{
			Type: wire.CommandSteering,
			Images: []zotigosession.DisplayCommandImage{{
				MimeType:  "image/png",
				SizeBytes: 1,
				Width:     1,
				Height:    1,
			}},
			TurnID: "turn-1",
		},
	})
	if err != nil {
		t.Fatalf("append steering command: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnCompleted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
	}); err != nil {
		t.Fatalf("append turn completed: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(workerbase.CommandCursorPath(sessionID)), 0755); err != nil {
		t.Fatalf("create cursor dir: %v", err)
	}
	if err := os.WriteFile(workerbase.CommandCursorPath(sessionID), []byte("{"), 0644); err != nil {
		t.Fatalf("write corrupt cursor: %v", err)
	}

	cursor, err := workerbase.LoadCommandCursor(context.Background(), store, sessionID)
	if err != nil {
		t.Fatalf("load worker command cursor: %v", err)
	}
	if cursor.Sequence != 0 || cursor.Offset != 0 {
		t.Fatalf("expected applied image steering to stay outside the command cursor, got %#v", cursor)
	}
}

func TestLoadWorkerCommandCursorDowngradesUnsafeSequence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	defer store.Close()
	sessionID := "sess-unsafe-sequence"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        sessionID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemUserMessage,
		Command: &zotigosession.DisplayCommand{
			Type: wire.CommandMessage,
			Text: "pending",
		},
	}); err != nil {
		t.Fatalf("append pending command: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(workerbase.CommandCursorPath(sessionID)), 0755); err != nil {
		t.Fatalf("create cursor dir: %v", err)
	}
	if err := os.WriteFile(workerbase.CommandCursorPath(sessionID), []byte(`{"offset":0,"sequence":999}`), 0644); err != nil {
		t.Fatalf("write corrupt cursor: %v", err)
	}

	cursor, err := workerbase.LoadCommandCursor(context.Background(), store, sessionID)
	if err != nil {
		t.Fatalf("load worker command cursor: %v", err)
	}
	if cursor.Sequence != 0 || cursor.Offset != 0 {
		t.Fatalf("expected unsafe sequence to recover to pending boundary, got %#v", cursor)
	}
}

func TestLoadWorkerCommandCursorDowngradesInvalidOffset(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	defer store.Close()
	sessionID := "sess-invalid-offset"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        sessionID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	item, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemUserMessage,
		Command: &zotigosession.DisplayCommand{
			Type: wire.CommandMessage,
			Text: "applied",
		},
	})
	if err != nil {
		t.Fatalf("append command: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
	}); err != nil {
		t.Fatalf("append turn started: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(workerbase.CommandCursorPath(sessionID)), 0755); err != nil {
		t.Fatalf("create cursor dir: %v", err)
	}
	if err := os.WriteFile(workerbase.CommandCursorPath(sessionID), []byte(`{"offset":999999,"sequence":1}`), 0644); err != nil {
		t.Fatalf("write corrupt cursor: %v", err)
	}

	cursor, err := workerbase.LoadCommandCursor(context.Background(), store, sessionID)
	if err != nil {
		t.Fatalf("load worker command cursor: %v", err)
	}
	if cursor.Sequence != item.Sequence || cursor.Offset != 0 {
		t.Fatalf("expected invalid offset to recover to safe sequence %d, got %#v", item.Sequence, cursor)
	}
}

func TestLoadWorkerCommandCursorDowngradesOffsetBeforePendingCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	defer store.Close()
	sessionID := "sess-pending-offset"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        sessionID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemUserMessage,
		Command: &zotigosession.DisplayCommand{
			Type: wire.CommandMessage,
			Text: "pending",
		},
	}); err != nil {
		t.Fatalf("append pending command: %v", err)
	}
	_, _, eofOffset, err := store.ListDisplayItemsFromOffset(context.Background(), sessionID, 0, 10)
	if err != nil {
		t.Fatalf("read display log offset: %v", err)
	}
	if eofOffset <= 0 {
		t.Fatalf("expected positive display log offset, got %d", eofOffset)
	}
	if err := os.MkdirAll(filepath.Dir(workerbase.CommandCursorPath(sessionID)), 0755); err != nil {
		t.Fatalf("create cursor dir: %v", err)
	}
	cursorData := fmt.Sprintf(`{"offset":%d,"sequence":0}`, eofOffset)
	if err := os.WriteFile(workerbase.CommandCursorPath(sessionID), []byte(cursorData), 0644); err != nil {
		t.Fatalf("write cursor: %v", err)
	}

	cursor, err := workerbase.LoadCommandCursor(context.Background(), store, sessionID)
	if err != nil {
		t.Fatalf("load worker command cursor: %v", err)
	}
	if cursor.Sequence != 0 || cursor.Offset != 0 {
		t.Fatalf("expected offset downgrade before pending command, got %#v", cursor)
	}
}

func TestWorkerClientConnectsBeforeRuntimeInitialization(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	t.Chdir(workDir)
	if err := os.WriteFile(filepath.Join(workDir, "zotigo.yaml"), []byte("default_profile: ["), 0644); err != nil {
		t.Fatalf("write invalid project config: %v", err)
	}

	var connected, ready, finished atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/internal/workers/connect":
			responseHeader := http.Header{}
			responseHeader.Set(wire.GenerationHeader, "test-generation")
			conn, err := workerUpgrader.Upgrade(w, r, responseHeader)
			if err != nil {
				return
			}
			connected.Store(true)
			go func() {
				defer conn.Close()
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
				}
			}()
		case strings.HasSuffix(r.URL.Path, "/worker/attach"):
			ready.Store(true)
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/worker/finish"):
			finished.Store(true)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	err := runWorkerClient(context.Background(), workerbase.Config{
		DaemonURL: server.URL,
		SessionID: "sess-init-before-connect",
	})
	if err == nil {
		t.Fatal("expected worker runtime initialization to fail")
	}
	if !connected.Load() {
		t.Fatal("worker did not connect before runtime initialization")
	}
	if ready.Load() {
		t.Fatal("worker reported ready after runtime initialization failed")
	}
	if !finished.Load() {
		t.Fatal("worker did not report runtime initialization failure")
	}
}

func TestWorkerClientInitializationFailurePreservesError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	const sessionID = "sess-init-failure"
	store, err := zotigosession.NewFileStore("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	putStoredSession(t, store, sessionID, workDir)
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte("default_profile: ["), 0644); err != nil {
		t.Fatal(err)
	}

	registry := newSessionRegistry()
	registry.Add(Session{ID: sessionID, State: SessionStateStarting, WorkingDirectory: workDir})
	handler := newHandler(registry, sessionlog.StoredItemSource{Store: store}, handlerOptions{
		store:           store,
		workerAuthToken: "worker-secret",
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	runErr := runWorkerClient(context.Background(), workerbase.Config{
		DaemonURL: server.URL,
		SessionID: sessionID,
		AuthToken: "worker-secret",
	})
	if runErr == nil {
		t.Fatal("expected worker initialization failure")
	}
	failed, ok := registry.Get(sessionID)
	if !ok {
		t.Fatal("session missing after worker failure")
	}
	if failed.State != SessionStateFailed {
		t.Fatalf("session state = %q, want %q", failed.State, SessionStateFailed)
	}
	if failed.Error != runErr.Error() {
		t.Fatalf("session error = %q, want %q", failed.Error, runErr.Error())
	}
}

func TestWorkerClientDialErrorIncludesHTTPResponse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"conflict","message":"worker connect requires a live session"}`))
	}))
	defer server.Close()

	err := runWorkerClient(context.Background(), workerbase.Config{
		DaemonURL: server.URL,
		SessionID: "sess-dial-conflict",
	})
	if err == nil || !strings.Contains(err.Error(), "409 Conflict") || !strings.Contains(err.Error(), "worker connect requires a live session") {
		t.Fatalf("dial error = %v", err)
	}
}

func TestResolveWorkerProfile(t *testing.T) {
	appConfig := &config.Config{
		DefaultProfile: "default-profile",
		Profiles: map[string]config.ProfileConfig{
			"default-profile":  {Provider: "openai", Model: "default-model"},
			"selected-profile": {Provider: "anthropic", Model: "selected-model"},
		},
	}

	t.Run("persisted profile", func(t *testing.T) {
		name, profile, err := resolveWorkerProfile(&zotigosession.Session{Metadata: zotigosession.Metadata{ProfileName: "selected-profile"}}, appConfig)
		if err != nil {
			t.Fatalf("resolve selected profile: %v", err)
		}
		if name != "selected-profile" || profile.Model != "selected-model" {
			t.Fatalf("expected selected profile, got name=%q profile=%#v", name, profile)
		}
	})

	t.Run("legacy session uses default", func(t *testing.T) {
		name, profile, err := resolveWorkerProfile(&zotigosession.Session{}, appConfig)
		if err != nil {
			t.Fatalf("resolve default profile: %v", err)
		}
		if name != "default-profile" || profile.Model != "default-model" {
			t.Fatalf("expected default profile, got name=%q profile=%#v", name, profile)
		}
	})
}

func TestWorkerRuntimeAppliesAndPersistsProfileCommand(t *testing.T) {
	const oldProviderName = "worker-profile-old"
	const newProviderName = "worker-profile-new"
	const newerProviderName = "worker-profile-newer"
	providers.Register(oldProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	providers.Register(newProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	providers.Register(newerProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	projectConfig := fmt.Sprintf("default_profile: old\nprofiles:\n  old:\n    provider: %s\n    model: old\n  new:\n    provider: %s\n    model: new\n  newer:\n    provider: %s\n    model: newer\n", oldProviderName, newProviderName, newerProviderName)
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID:               "sess-worker-profile",
		WorkingDirectory: workDir,
		ProfileName:      "old",
		CreatedAt:        now,
		UpdatedAt:        now,
	}}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	localExec, err := executor.NewLocalExecutor(workDir)
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: oldProviderName, Model: "old"}, localExec, agent.WithProfileName("old"))
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	display := sessionlog.NewLog("sess-worker-profile", sessionlog.StoredItemSource{Store: store})
	runtime := &workerRuntime{
		sessionID: "sess-worker-profile",
		workDir:   workDir,
		store:     store,
		agent:     ag,
		display:   display,
	}
	commandItem, err := store.AppendDisplayItem(context.Background(), "sess-worker-profile", zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemSessionCommand,
		Command: &zotigosession.DisplayCommand{
			Type:    wire.CommandProfile,
			Profile: "new",
		},
	})
	if err != nil {
		t.Fatalf("append profile command: %v", err)
	}
	if err := runtime.HandleCommand(context.Background(), sessionlog.ProfileCommandFromItem(commandItem)); err != nil {
		t.Fatalf("handle profile command: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		stored, err := store.Get(context.Background(), "sess-worker-profile")
		items, _, itemsErr := store.ListDisplayItems(context.Background(), "sess-worker-profile")
		// BeforeApply persists the change before the agent installs it. Wait for
		// both observations instead of racing the asynchronous runtime switch.
		if err == nil && stored != nil && stored.ProfileName == "new" && itemsErr == nil && len(items) == 2 && ag.ActiveProfileName() == "new" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	stored, err := store.Get(context.Background(), "sess-worker-profile")
	if err != nil || stored.ProfileName != "new" {
		t.Fatalf("expected persisted profile new, got session=%#v err=%v", stored, err)
	}
	if ag.ActiveProfileName() != "new" {
		t.Fatalf("expected active profile new, got %q", ag.ActiveProfileName())
	}
	items, _, err := store.ListDisplayItems(context.Background(), "sess-worker-profile")
	if err != nil {
		t.Fatalf("load display items: %v", err)
	}
	if len(items) != 2 || items[1].Type != zotigosession.DisplayItemProfileChanged || items[1].Profile == nil || items[1].Profile.CommandID != commandItem.ID {
		t.Fatalf("unexpected profile display items: %#v", items)
	}
	if cursor := workerbase.RecoverAppliedCommandSequence(items); cursor != commandItem.Sequence {
		t.Fatalf("expected applied profile command cursor %d, got %d", commandItem.Sequence, cursor)
	}
	newerCommand, err := store.AppendDisplayItem(context.Background(), "sess-worker-profile", zotigosession.DisplayItem{
		Type:    zotigosession.DisplayItemSessionCommand,
		Command: &zotigosession.DisplayCommand{Type: wire.CommandProfile, Profile: "newer"},
	})
	if err != nil {
		t.Fatalf("append newer profile command: %v", err)
	}
	if err := runtime.HandleCommand(context.Background(), sessionlog.ProfileCommandFromItem(newerCommand)); err != nil {
		t.Fatalf("handle newer profile command: %v", err)
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) && ag.ActiveProfileName() != "newer" {
		time.Sleep(time.Millisecond)
	}
	items, _, err = store.ListDisplayItems(context.Background(), "sess-worker-profile")
	if err != nil {
		t.Fatalf("load consecutive profile items: %v", err)
	}
	if len(items) != 4 || items[3].Profile == nil || items[3].Profile.CommandID != newerCommand.ID || items[3].Profile.From != "new" || items[3].Profile.To != "newer" {
		t.Fatalf("expected consecutive marker new->newer, got %#v", items)
	}
	registry := newSessionRegistry()
	registry.Add(Session{
		ID:               "sess-worker-profile",
		State:            SessionStateRunning,
		WorkingDirectory: workDir,
		ProfileName:      "old",
		CreatedAt:        now,
	})
	handler := newHandler(registry, sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/sessions/sess-worker-profile", nil))
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected live session status %d, got %d: %s", http.StatusOK, getRec.Code, getRec.Body.String())
	}
	var live Session
	if err := decodeAPIData(t, getRec.Body.Bytes(), &live); err != nil {
		t.Fatalf("decode live session: %v", err)
	}
	if live.ProfileName != "newer" {
		t.Fatalf("expected durable profile to override stale registry value, got %#v", live)
	}
	startRec := httptest.NewRecorder()
	handler.ServeHTTP(startRec, httptest.NewRequest(http.MethodPost, "/sessions/sess-worker-profile/start", nil))
	if startRec.Code != http.StatusOK {
		t.Fatalf("expected idempotent start status %d, got %d: %s", http.StatusOK, startRec.Code, startRec.Body.String())
	}
	var started Session
	if err := decodeAPIData(t, startRec.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode idempotent start: %v", err)
	}
	if started.ProfileName != "newer" {
		t.Fatalf("expected idempotent start profile new, got %#v", started)
	}
}

func TestRecoverAppliedCommandSequenceWaitsForProfileResult(t *testing.T) {
	command := zotigosession.DisplayItem{
		ID:       "item-profile",
		Sequence: 1,
		Type:     zotigosession.DisplayItemSessionCommand,
		Command:  &zotigosession.DisplayCommand{Type: wire.CommandProfile, Profile: "new"},
	}
	if cursor := workerbase.RecoverAppliedCommandSequence([]zotigosession.DisplayItem{command}); cursor != 0 {
		t.Fatalf("expected pending profile command cursor 0, got %d", cursor)
	}
	failed := zotigosession.DisplayItem{
		Sequence: 2,
		Type:     zotigosession.DisplayItemProfileFailed,
		Profile:  &zotigosession.DisplayProfileChange{CommandID: command.ID, To: "new"},
	}
	if cursor := workerbase.RecoverAppliedCommandSequence([]zotigosession.DisplayItem{command, failed}); cursor != command.Sequence {
		t.Fatalf("expected failed profile command cursor %d, got %d", command.Sequence, cursor)
	}
}

func TestWorkerRuntimeKeepsProfileWhenPersistenceFails(t *testing.T) {
	const oldProviderName = "worker-profile-failure-old"
	const newProviderName = "worker-profile-failure-new"
	providers.Register(oldProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	providers.Register(newProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	projectConfig := fmt.Sprintf("default_profile: old\nprofiles:\n  old:\n    provider: %s\n    model: old\n  new:\n    provider: %s\n    model: new\n", oldProviderName, newProviderName)
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	localExec, err := executor.NewLocalExecutor(workDir)
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: oldProviderName, Model: "old"}, localExec, agent.WithProfileName("old"))
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	runtime := &workerRuntime{
		sessionID: "sess-profile-failure",
		workDir:   workDir,
		store:     unavailableSessionStore{err: errors.New("store unavailable")},
		agent:     ag,
		display:   sessionlog.NewLog("sess-profile-failure", source),
	}
	if err := runtime.HandleCommand(context.Background(), wire.Command{
		ID:      "profile-command",
		Type:    wire.CommandProfile,
		Profile: &wire.ProfileCommandPayload{Name: "new"},
	}); err != nil {
		t.Fatalf("handle profile command: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		items, _, _ := source.LoadItems(context.Background(), "sess-profile-failure")
		if len(items) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	items, _, err := source.LoadItems(context.Background(), "sess-profile-failure")
	if err != nil {
		t.Fatalf("load failure item: %v", err)
	}
	if len(items) != 1 || items[0].Type != zotigosession.DisplayItemProfileFailed || !strings.Contains(items[0].Error, "store unavailable") {
		t.Fatalf("expected persisted profile failure, got %#v", items)
	}
	if got := ag.ActiveProfileName(); got != "old" {
		t.Fatalf("expected active profile old after persistence failure, got %q", got)
	}
}

func TestWorkerRuntimeKeepsProfileWhenProviderBuildFails(t *testing.T) {
	const oldProviderName = "worker-profile-build-failure-old"
	providers.Register(oldProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	projectConfig := fmt.Sprintf("default_profile: old\nprofiles:\n  old:\n    provider: %s\n    model: old\n  broken:\n    provider: provider-that-does-not-exist\n    model: broken\n", oldProviderName)
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID:               "sess-profile-build-failure",
		WorkingDirectory: workDir,
		ProfileName:      "old",
		CreatedAt:        now,
		UpdatedAt:        now,
	}}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	localExec, err := executor.NewLocalExecutor(workDir)
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: oldProviderName, Model: "old"}, localExec, agent.WithProfileName("old"))
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	runtime := &workerRuntime{
		sessionID: "sess-profile-build-failure",
		workDir:   workDir,
		store:     store,
		agent:     ag,
		display:   sessionlog.NewLog("sess-profile-build-failure", source),
	}
	if err := runtime.HandleCommand(context.Background(), wire.Command{
		ID:      "profile-command",
		Type:    wire.CommandProfile,
		Profile: &wire.ProfileCommandPayload{Name: "broken"},
	}); err != nil {
		t.Fatalf("handle profile command: %v", err)
	}
	items, _, err := source.LoadItems(context.Background(), "sess-profile-build-failure")
	if err != nil {
		t.Fatalf("load failure item: %v", err)
	}
	if len(items) != 1 || items[0].Type != zotigosession.DisplayItemProfileFailed || !strings.Contains(items[0].Error, "provider-that-does-not-exist") {
		t.Fatalf("expected provider build failure item, got %#v", items)
	}
	stored, err := store.Get(context.Background(), "sess-profile-build-failure")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if stored.ProfileName != "old" || ag.ActiveProfileName() != "old" {
		t.Fatalf("expected old profile after provider build failure, got stored=%q active=%q", stored.ProfileName, ag.ActiveProfileName())
	}
}

func TestWorkerRuntimeTreatsFailureMarkerWriteAsReplayable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	projectConfig := "default_profile: old\nprofiles:\n  old:\n    provider: openai\n    model: old\n  broken:\n    provider: provider-that-does-not-exist\n    model: broken\n"
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	localExec, err := executor.NewLocalExecutor(workDir)
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: "openai", Model: "old"}, localExec, agent.WithProfileName("old"))
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}, appendErr: func(_ string, item zotigosession.DisplayItem) error {
		if item.Type == zotigosession.DisplayItemProfileFailed {
			return errors.New("display log unavailable")
		}
		return nil
	}}
	runtime := &workerRuntime{sessionID: "sess-marker-failure", workDir: workDir, agent: ag, display: sessionlog.NewLog("sess-marker-failure", source)}
	if err := runtime.HandleCommand(context.Background(), wire.Command{ID: "profile-command", Type: wire.CommandProfile, Profile: &wire.ProfileCommandPayload{Name: "broken"}}); err != nil {
		t.Fatalf("handle profile command: %v", err)
	}
	fatalErr := runtime.currentFatalError()
	var uncertain *workerbase.ProfileCompletionUncertainError
	if !errors.As(fatalErr, &uncertain) || !workerbase.IsExpectedClose(fatalErr) {
		t.Fatalf("expected replayable completion uncertainty, got %v", fatalErr)
	}
}

func TestWorkerRuntimeDoesNotCompleteLaterProfileAfterMarkerFailure(t *testing.T) {
	localExec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: "openai", Model: "old"}, localExec, agent.WithProfileName("old"))
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}, appendErr: func(_ string, item zotigosession.DisplayItem) error {
		if item.Profile != nil && item.Profile.CommandID == "profile-a" {
			return errors.New("display log unavailable")
		}
		return nil
	}}
	runtime := &workerRuntime{sessionID: "sess-marker-order", agent: ag, display: sessionlog.NewLog("sess-marker-order", source)}
	ready := make(chan struct{})
	close(ready)
	orderedA := make(chan struct{})
	resultA := make(chan error, 1)
	resultA <- agent.ErrRuntimeProfileSuperseded
	completionA := make(chan error, 1)
	runtime.finishProfileSwitch(ready, orderedA, "profile-a", "new", resultA, completionA)
	if err := <-completionA; err == nil {
		t.Fatal("expected first marker failure")
	}

	completionB := make(chan error, 1)
	runtime.completeProfileFailure(orderedA, make(chan struct{}), completionB, "profile-b", "newer", errors.New("not applied"))
	if err := <-completionB; err == nil {
		t.Fatal("expected later profile to inherit fatal completion uncertainty")
	}
	items, _, err := source.LoadItems(context.Background(), "sess-marker-order")
	if err != nil {
		t.Fatalf("load items: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("later profile marker crossed failed predecessor: %#v", items)
	}
}

func TestWorkerRuntimeBrokenLatestProfileSupersedesPendingProfile(t *testing.T) {
	const oldProviderName = "worker-profile-supersede-old"
	const newProviderName = "worker-profile-supersede-new"
	oldProvider := &blockingFinishProvider{started: make(chan struct{}), release: make(chan struct{})}
	providers.Register(oldProviderName, func(config.ProfileConfig) (providers.Provider, error) { return oldProvider, nil })
	providers.Register(newProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	projectConfig := fmt.Sprintf("default_profile: old\nprofiles:\n  old:\n    provider: %s\n    model: old\n  new:\n    provider: %s\n    model: new\n  broken:\n    provider: provider-that-does-not-exist\n    model: broken\n", oldProviderName, newProviderName)
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID:               "sess-profile-supersede",
		WorkingDirectory: workDir,
		ProfileName:      "old",
		CreatedAt:        now,
		UpdatedAt:        now,
	}}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	localExec, err := executor.NewLocalExecutor(workDir)
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: oldProviderName, Model: "old"}, localExec, agent.WithProfileName("old"))
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	runtime := &workerRuntime{
		sessionID: "sess-profile-supersede",
		workDir:   workDir,
		store:     store,
		agent:     ag,
		display:   sessionlog.NewLog("sess-profile-supersede", source),
	}
	events, err := ag.Run(context.Background(), "keep the current generation active")
	if err != nil {
		t.Fatalf("run agent: %v", err)
	}
	<-oldProvider.started
	if err := runtime.HandleCommand(context.Background(), wire.Command{
		ID:      "profile-a",
		Type:    wire.CommandProfile,
		Profile: &wire.ProfileCommandPayload{Name: "new"},
	}); err != nil {
		t.Fatalf("queue valid profile: %v", err)
	}
	if err := runtime.HandleCommand(context.Background(), wire.Command{
		ID:      "profile-b",
		Type:    wire.CommandProfile,
		Profile: &wire.ProfileCommandPayload{Name: "broken"},
	}); err != nil {
		t.Fatalf("handle broken latest profile: %v", err)
	}
	close(oldProvider.release)
	for range events {
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		items, _, _ := source.LoadItems(context.Background(), "sess-profile-supersede")
		if len(items) == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	items, _, err := source.LoadItems(context.Background(), "sess-profile-supersede")
	if err != nil {
		t.Fatalf("load profile failures: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected superseded and build failure items, got %#v", items)
	}
	if items[0].Profile == nil || items[0].Profile.CommandID != "profile-a" || items[1].Profile == nil || items[1].Profile.CommandID != "profile-b" {
		t.Fatalf("expected profile completion markers in command order, got %#v", items)
	}
	failures := map[string]string{}
	for _, item := range items {
		if item.Type != zotigosession.DisplayItemProfileFailed || item.Profile == nil {
			t.Fatalf("expected profile failure item, got %#v", item)
		}
		failures[item.Profile.CommandID] = item.Error
	}
	if !strings.Contains(failures["profile-a"], agent.ErrRuntimeProfileSuperseded.Error()) {
		t.Fatalf("expected profile A to be superseded, got %q", failures["profile-a"])
	}
	if !strings.Contains(failures["profile-b"], "provider-that-does-not-exist") {
		t.Fatalf("expected profile B build failure, got %q", failures["profile-b"])
	}
	stored, err := store.Get(context.Background(), "sess-profile-supersede")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if stored.ProfileName != "old" || ag.ActiveProfileName() != "old" {
		t.Fatalf("expected old profile to remain, got stored=%q active=%q", stored.ProfileName, ag.ActiveProfileName())
	}
}

func TestWorkerRuntimeSerializesNewProfileRequestWithDurableCommit(t *testing.T) {
	const oldProviderName = "worker-profile-linear-old"
	const newProviderName = "worker-profile-linear-new"
	providers.Register(oldProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	providers.Register(newProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	projectConfig := fmt.Sprintf("default_profile: old\nprofiles:\n  old:\n    provider: %s\n    model: old\n  new:\n    provider: %s\n    model: new\n  broken:\n    provider: provider-that-does-not-exist\n    model: broken\n", oldProviderName, newProviderName)
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	baseStore, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer baseStore.Close()
	now := time.Now().UTC()
	if err := baseStore.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID:               "sess-profile-linear",
		WorkingDirectory: workDir,
		ProfileName:      "old",
		CreatedAt:        now,
		UpdatedAt:        now,
	}}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	store := &blockingProfileUpdateStore{
		Store:   baseStore,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	localExec, err := executor.NewLocalExecutor(workDir)
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: oldProviderName, Model: "old"}, localExec, agent.WithProfileName("old"))
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	runtime := &workerRuntime{
		sessionID: "sess-profile-linear",
		workDir:   workDir,
		store:     store,
		agent:     ag,
		display:   sessionlog.NewLog("sess-profile-linear", sessionlog.StoredItemSource{Store: store}),
	}
	if err := runtime.HandleCommand(context.Background(), wire.Command{
		ID:      "profile-a",
		Type:    wire.CommandProfile,
		Profile: &wire.ProfileCommandPayload{Name: "new"},
	}); err != nil {
		t.Fatalf("handle profile A: %v", err)
	}
	<-store.started

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- runtime.HandleCommand(context.Background(), wire.Command{
			ID:      "profile-b",
			Type:    wire.CommandProfile,
			Profile: &wire.ProfileCommandPayload{Name: "broken"},
		})
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("profile B crossed the active durable commit: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(store.release)
	if err := <-secondDone; err != nil {
		t.Fatalf("handle profile B: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && ag.ActiveProfileName() != "new" {
		time.Sleep(time.Millisecond)
	}
	stored, err := baseStore.Get(context.Background(), "sess-profile-linear")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if stored.ProfileName != "new" || ag.ActiveProfileName() != "new" {
		t.Fatalf("expected profile A to linearize before B, got stored=%q active=%q", stored.ProfileName, ag.ActiveProfileName())
	}
	items, _, err := baseStore.ListDisplayItems(context.Background(), "sess-profile-linear")
	if err != nil {
		t.Fatalf("load display items: %v", err)
	}
	if len(items) != 2 || items[0].Type != zotigosession.DisplayItemProfileChanged || items[1].Type != zotigosession.DisplayItemProfileFailed {
		t.Fatalf("expected applied A then failed B, got %#v", items)
	}
}

func TestWorkerRuntimeLeavesUncertainProfileCommandForReplay(t *testing.T) {
	const oldProviderName = "worker-profile-uncertain-old"
	const newProviderName = "worker-profile-uncertain-new"
	providers.Register(oldProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	providers.Register(newProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	projectConfig := fmt.Sprintf("default_profile: old\nprofiles:\n  old:\n    provider: %s\n    model: old\n  new:\n    provider: %s\n    model: new\n", oldProviderName, newProviderName)
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	baseStore, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer baseStore.Close()
	now := time.Now().UTC()
	if err := baseStore.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID:               "sess-profile-uncertain",
		WorkingDirectory: workDir,
		ProfileName:      "old",
		CreatedAt:        now,
		UpdatedAt:        now,
	}}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	command := zotigosession.DisplayItem{
		ID:       "profile-command",
		Sequence: 1,
		Type:     zotigosession.DisplayItemSessionCommand,
		Command:  &zotigosession.DisplayCommand{Type: wire.CommandProfile, Profile: "new"},
	}
	messageCommand := zotigosession.DisplayItem{
		ID:       "message-command",
		Sequence: 2,
		Type:     zotigosession.DisplayItemUserMessage,
		Role:     string(protocol.RoleUser),
		Content:  []zotigosession.DisplayContentPart{{Type: string(protocol.ContentTypeText), Text: "must not run"}},
		Command:  &zotigosession.DisplayCommand{Type: wire.CommandMessage, Text: "must not run"},
	}
	var markerAttempts atomic.Int32
	source := &fakeDisplayItemSource{
		items: map[string][]zotigosession.DisplayItem{"sess-profile-uncertain": {command, messageCommand}},
		appendErr: func(_ string, item zotigosession.DisplayItem) error {
			if item.Type == zotigosession.DisplayItemProfileChanged && markerAttempts.Add(1) == 1 {
				return errors.New("display log unavailable")
			}
			return nil
		},
	}
	store := &rollbackFailingProfileStore{Store: baseStore}
	oldExec, err := executor.NewLocalExecutor(workDir)
	if err != nil {
		t.Fatalf("create old executor: %v", err)
	}
	defer oldExec.Close()
	oldAgent, err := agent.New(config.ProfileConfig{Provider: oldProviderName, Model: "old"}, oldExec, agent.WithProfileName("old"))
	if err != nil {
		t.Fatalf("create old agent: %v", err)
	}
	firstRuntime := &workerRuntime{
		sessionID: "sess-profile-uncertain",
		workDir:   workDir,
		store:     store,
		agent:     oldAgent,
		display:   sessionlog.NewLog("sess-profile-uncertain", source),
		fatalCh:   make(chan error, 1),
	}
	messageResponse, err := sessionlog.MessageCommandFromItem(messageCommand, "")
	if err != nil {
		t.Fatalf("build message command: %v", err)
	}
	commandServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		data, marshalErr := sonic.Marshal(wire.CommandsPage{
			Commands:   []wire.Command{sessionlog.ProfileCommandFromItem(command), messageResponse},
			NextOffset: 2,
		})
		if marshalErr != nil {
			http.Error(w, marshalErr.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	defer commandServer.Close()
	cursor, replayErr := replayWorkerCommands(context.Background(), commandServer.Client(), commandServer.URL, "sess-profile-uncertain", firstRuntime, workerbase.CommandCursor{})
	var uncertain *workerbase.ProfileStateUncertainError
	if !errors.As(replayErr, &uncertain) {
		t.Fatalf("expected uncertain replay error, got %v", replayErr)
	}
	if cursor.Sequence != 0 || cursor.Offset != 0 {
		t.Fatalf("expected cursor to remain before uncertain profile, got %#v", cursor)
	}
	select {
	case fatalErr := <-firstRuntime.fatalCh:
		if !errors.As(fatalErr, &uncertain) {
			t.Fatalf("expected uncertain profile error, got %v", fatalErr)
		}
	case <-time.After(time.Second):
		t.Fatal("expected uncertain profile to stop worker runtime")
	}
	items, _, err := source.LoadItems(context.Background(), "sess-profile-uncertain")
	if err != nil {
		t.Fatalf("load pending command: %v", err)
	}
	if len(items) != 2 || workerbase.RecoverAppliedCommandSequence(items) != 0 {
		t.Fatalf("expected uncertain command to remain pending, got %#v", items)
	}
	for _, item := range items {
		if item.Type == zotigosession.DisplayItemTurnStarted {
			t.Fatalf("message after uncertain profile was executed: %#v", items)
		}
	}
	stored, err := baseStore.Get(context.Background(), "sess-profile-uncertain")
	if err != nil || stored.ProfileName != "new" || oldAgent.ActiveProfileName() != "old" {
		t.Fatalf("expected durable/runtime split before replay, stored=%#v active=%q err=%v", stored, oldAgent.ActiveProfileName(), err)
	}

	newExec, err := executor.NewLocalExecutor(workDir)
	if err != nil {
		t.Fatalf("create new executor: %v", err)
	}
	defer newExec.Close()
	newAgent, err := agent.New(config.ProfileConfig{Provider: newProviderName, Model: "new"}, newExec, agent.WithProfileName("new"))
	if err != nil {
		t.Fatalf("create new agent: %v", err)
	}
	secondRuntime := &workerRuntime{
		sessionID: "sess-profile-uncertain",
		workDir:   workDir,
		store:     baseStore,
		agent:     newAgent,
		display:   sessionlog.NewLog("sess-profile-uncertain", source),
	}
	if err := secondRuntime.HandleCommand(context.Background(), sessionlog.ProfileCommandFromItem(command)); err != nil {
		t.Fatalf("replay uncertain profile command: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		items, _, _ = source.LoadItems(context.Background(), "sess-profile-uncertain")
		if workerbase.RecoverAppliedCommandSequence(items) == command.Sequence {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if workerbase.RecoverAppliedCommandSequence(items) != command.Sequence {
		t.Fatalf("expected replay to complete uncertain command, got %#v", items)
	}
}

func TestWorkerRuntimeRollsBackProfileWhenChangedItemFails(t *testing.T) {
	const oldProviderName = "worker-profile-append-failure-old"
	const newProviderName = "worker-profile-append-failure-new"
	providers.Register(oldProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	providers.Register(newProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	projectConfig := fmt.Sprintf("default_profile: old\nprofiles:\n  old:\n    provider: %s\n    model: old\n  new:\n    provider: %s\n    model: new\n", oldProviderName, newProviderName)
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID:               "sess-profile-append-failure",
		WorkingDirectory: workDir,
		ProfileName:      "old",
		CreatedAt:        now,
		UpdatedAt:        now,
	}}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	localExec, err := executor.NewLocalExecutor(workDir)
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: oldProviderName, Model: "old"}, localExec, agent.WithProfileName("old"))
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	source := &fakeDisplayItemSource{
		items: map[string][]zotigosession.DisplayItem{},
		appendErr: func(_ string, item zotigosession.DisplayItem) error {
			if item.Type == zotigosession.DisplayItemProfileChanged {
				return errors.New("display log unavailable")
			}
			return nil
		},
	}
	runtime := &workerRuntime{
		sessionID: "sess-profile-append-failure",
		workDir:   workDir,
		store:     store,
		agent:     ag,
		display:   sessionlog.NewLog("sess-profile-append-failure", source),
	}
	if err := runtime.HandleCommand(context.Background(), wire.Command{
		ID:      "profile-command",
		Type:    wire.CommandProfile,
		Profile: &wire.ProfileCommandPayload{Name: "new"},
	}); err != nil {
		t.Fatalf("handle profile command: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		items, _, _ := source.LoadItems(context.Background(), "sess-profile-append-failure")
		if len(items) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	items, _, err := source.LoadItems(context.Background(), "sess-profile-append-failure")
	if err != nil {
		t.Fatalf("load failure item: %v", err)
	}
	if len(items) != 1 || items[0].Type != zotigosession.DisplayItemProfileFailed || !strings.Contains(items[0].Error, "display log unavailable") {
		t.Fatalf("expected profile failure item, got %#v", items)
	}
	stored, err := store.Get(context.Background(), "sess-profile-append-failure")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if stored.ProfileName != "old" || !stored.UpdatedAt.Equal(now) {
		t.Fatalf("expected stored profile rollback, got profile=%q updated_at=%s", stored.ProfileName, stored.UpdatedAt)
	}
	if got := ag.ActiveProfileName(); got != "old" {
		t.Fatalf("expected active profile old after display append failure, got %q", got)
	}
}

func TestMessageFromCommandIncludesImages(t *testing.T) {
	msg, err := messageFromCommand("item_1", &wire.MessageCommandPayload{
		Text: "describe this",
		Images: []wire.CommandImageData{{
			MimeType:   "image/png",
			DataBase64: tinyPNGBase64(),
		}},
	})
	if err != nil {
		t.Fatalf("messageFromCommand: %v", err)
	}
	if msg.Role != protocol.RoleUser || len(msg.Content) != 2 {
		t.Fatalf("unexpected message: %#v", msg)
	}
	if msg.Content[1].Type != protocol.ContentTypeImage || msg.Content[1].Image == nil ||
		msg.Content[1].Image.MediaType != "image/png" || len(msg.Content[1].Image.Data) == 0 {
		t.Fatalf("expected image content part, got %#v", msg.Content[1])
	}
}

func TestMessageFromCommandRejectsMissingImagePayload(t *testing.T) {
	_, err := messageFromCommand("item_1", &wire.MessageCommandPayload{
		Text: "describe this",
		Images: []wire.CommandImageData{{
			MimeType: "image/png",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "image payload unavailable") {
		t.Fatalf("expected missing image payload error, got %v", err)
	}
}

func TestWorkerRuntimeCloseInterruptsActiveTurn(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := sessionlog.NewLog("sess-close", source)
	if _, err := display.StartTurn(context.Background()); err != nil {
		t.Fatalf("start turn: %v", err)
	}
	runtime := &workerRuntime{
		display:    display,
		turnActive: true,
	}

	runtime.Close()
	if err := display.HandleEvent(context.Background(), protocol.NewFinishEvent(protocol.FinishReasonStop)); err != nil {
		t.Fatalf("late finish event should be ignored: %v", err)
	}

	items := source.items["sess-close"]
	if len(items) != 2 {
		t.Fatalf("expected turn_started and turn_interrupted, got %#v", items)
	}
	if items[1].Type != zotigosession.DisplayItemTurnInterrupted || items[1].Turn == nil {
		t.Fatalf("expected turn_interrupted item, got %#v", items[1])
	}
	if items[1].Turn.Reason != workerbase.ControlChannelClosedReason {
		t.Fatalf("expected reason %q, got %#v", workerbase.ControlChannelClosedReason, items[1].Turn)
	}
	if got := sessionlog.LastOpenTurnID(items); got != "" {
		t.Fatalf("expected no open turn, got %q", got)
	}
}

func TestWorkerRuntimeCloseWaitsForTurnPersistence(t *testing.T) {
	done := make(chan struct{})
	runtime := &workerRuntime{
		turnActive: true,
		turnDone:   done,
	}
	closed := make(chan struct{})
	go func() {
		runtime.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("worker runtime closed before active turn persistence completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(done)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("worker runtime did not close after turn persistence completed")
	}
}

func TestWorkerRuntimeCloseWaitsForIdleProfileApply(t *testing.T) {
	const providerName = "worker-close-idle-profile"
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	defer exec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: providerName}, exec, agent.WithProfileName("old"))
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	applyStarted := make(chan struct{})
	releaseApply := make(chan struct{})
	result := ag.QueueRuntimeProfile(agent.RuntimeProfile{
		Name:     "new",
		Config:   config.ProfileConfig{Provider: providerName},
		Provider: &noopProvider{},
		BeforeApply: func() error {
			close(applyStarted)
			<-releaseApply
			return nil
		},
	})
	<-applyStarted
	runtime := &workerRuntime{agent: ag}
	closed := make(chan struct{})
	go func() {
		runtime.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("worker runtime closed while idle profile apply was active")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseApply)
	if err := <-result; err != nil {
		t.Fatalf("apply runtime profile: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("worker runtime did not close after idle profile apply")
	}
}

func TestWorkerRuntimeCloseWaitsForAgentStreamExit(t *testing.T) {
	const providerName = "worker-close-delayed-stream"
	provider := &blockingFinishProvider{started: make(chan struct{}), release: make(chan struct{})}
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) { return provider, nil })
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID:        "sess-close-stream",
		CreatedAt: now,
		UpdatedAt: now,
	}}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	defer exec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: providerName}, exec)
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	transport := zotigotransport.NewChannelTransport(10)
	defer transport.Close()
	runtime := &workerRuntime{
		sessionID: "sess-close-stream",
		store:     store,
		agent:     ag,
		display:   sessionlog.NewLog("sess-close-stream", source),
	}
	runtime.runner = runner.New(ag, transport)
	if err := runtime.startMessageTurn(context.Background(), "message-command", 1, &wire.MessageCommandPayload{Text: "wait"}); err != nil {
		t.Fatalf("start message turn: %v", err)
	}
	<-provider.started
	closed := make(chan struct{})
	go func() {
		runtime.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("worker runtime closed before Agent stream exited")
	case <-time.After(20 * time.Millisecond):
	}
	close(provider.release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("worker runtime did not close after Agent stream exited")
	}
	stored, err := store.Get(context.Background(), "sess-close-stream")
	if err != nil {
		t.Fatalf("load final snapshot: %v", err)
	}
	if stored.AgentSnapshot.State == agent.StateRunning {
		t.Fatalf("expected final non-running snapshot, got %q", stored.AgentSnapshot.State)
	}
}

func TestWorkerRuntimeSteeringWaitsForTurnReady(t *testing.T) {
	providers.Register("zotigod-ready-test", func(config.ProfileConfig) (providers.Provider, error) {
		return &noopProvider{}, nil
	})
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := sessionlog.NewLog("sess-ready", source)
	turnID, err := display.StartTurn(context.Background())
	if err != nil {
		t.Fatalf("start turn: %v", err)
	}
	localExec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: "zotigod-ready-test"}, localExec)
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	runtime := &workerRuntime{
		sessionID:  "sess-ready",
		workDir:    "/workspace",
		agent:      ag,
		display:    display,
		hooks:      &channelHookDispatcher{events: make(chan hooks.Event, 1)},
		hookModel:  "model-test",
		turnActive: true,
		turnReady:  make(chan struct{}),
		turnDone:   make(chan struct{}),
	}

	result := make(chan error, 1)
	go func() {
		result <- runtime.queueTurnUserInput(context.Background(), wire.Command{
			ID:   "steering-1",
			Type: wire.CommandSteering,
			Steering: &wire.SteeringCommandPayload{
				TurnID: turnID,
				Text:   "use the smaller fix",
			},
		})
	}()

	select {
	case err := <-result:
		t.Fatalf("steering returned before turn was ready: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	ag.Restore(agent.Snapshot{State: agent.StateRunning})
	runtime.markTurnReady()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("queue steering after turn ready: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("expected steering to apply after turn ready")
	}
	dispatcher := runtime.hooks.(*channelHookDispatcher)
	select {
	case event := <-dispatcher.events:
		if event.EventName != hooks.UserPromptSubmit || event.TurnID != turnID || event.Prompt == nil || event.Prompt.Text != "use the smaller fix" {
			t.Fatalf("unexpected steering hook: %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("expected steering UserPromptSubmit hook")
	}
}

func TestWorkerRuntimeAcceptsInputWhileAgentIsPaused(t *testing.T) {
	const providerName = "zotigod-paused-input-test"
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) {
		return &noopProvider{}, nil
	})
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := sessionlog.NewLog("sess-paused-input", source)
	turnID, err := display.StartTurn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	localExec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: providerName}, localExec)
	if err != nil {
		t.Fatal(err)
	}
	ag.Restore(agent.Snapshot{State: agent.StatePaused})
	ready := make(chan struct{})
	close(ready)
	runtime := &workerRuntime{
		sessionID: "sess-paused-input", agent: ag, display: display,
		turnActive: true, turnReady: ready, turnDone: make(chan struct{}), readyDone: true,
	}
	result := runtime.AcceptInput(context.Background(), wire.InputRequest{Command: wire.Command{
		ID: "paused-steering", Type: wire.CommandMessage, Message: &wire.MessageCommandPayload{Text: "change direction"},
	}})
	if result.Error != "" || result.Command == nil || result.Command.Type != wire.CommandSteering || result.Command.Steering.TurnID != turnID {
		t.Fatalf("input result = %#v", result)
	}
	items, _, err := source.LoadItems(context.Background(), "sess-paused-input")
	if err != nil || len(items) != 2 || items[1].ID != "paused-steering" || items[1].Command == nil || items[1].Command.Type != wire.CommandSteering {
		t.Fatalf("durable paused steering = %#v, err=%v", items, err)
	}
}

func TestWorkerRuntimeAcceptInputWaitsForNewTurnAdmission(t *testing.T) {
	const providerName = "zotigod-starting-input-test"
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := sessionlog.NewLog("sess-starting-input", source)
	turnID, err := display.StartTurn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	localExec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: providerName}, localExec)
	if err != nil {
		t.Fatal(err)
	}
	ag.Restore(agent.Snapshot{State: agent.StateRunning})
	runtime := &workerRuntime{
		sessionID: "sess-starting-input", agent: ag, display: display,
		turnActive: true, turnReady: make(chan struct{}), turnDone: make(chan struct{}),
	}
	result := make(chan wire.InputResult, 1)
	go func() {
		result <- runtime.AcceptInput(context.Background(), wire.InputRequest{Command: wire.Command{
			ID: "starting-steering", Type: wire.CommandMessage, Message: &wire.MessageCommandPayload{Text: "add this"},
		}})
	}()
	select {
	case got := <-result:
		t.Fatalf("input admitted before turn readiness: %#v", got)
	case <-time.After(20 * time.Millisecond):
	}
	runtime.markTurnReady()
	got := <-result
	if got.Error != "" || got.Command == nil || got.Command.Type != wire.CommandSteering || got.Command.Steering.TurnID != turnID {
		t.Fatalf("input result = %#v", got)
	}
	items, _, err := source.LoadItems(context.Background(), runtime.sessionID)
	if err != nil || len(items) != 2 || items[1].ID != "starting-steering" || items[1].Command.Type != wire.CommandSteering {
		t.Fatalf("accepted input = %#v, err=%v", items, err)
	}
}

func TestWorkerRuntimeAcceptInputStartsOnceAfterCompletionBoundary(t *testing.T) {
	const providerName = "zotigod-completion-input-test"
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{ID: "sess-completion-input", CreatedAt: now, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	localExec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: providerName}, localExec)
	if err != nil {
		t.Fatal(err)
	}
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := sessionlog.NewLog("sess-completion-input", source)
	if _, err := display.StartTurn(context.Background()); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	close(ready)
	transport := zotigotransport.NewChannelTransport(4)
	defer transport.Close()
	runtime := &workerRuntime{
		sessionID: "sess-completion-input", store: store, agent: ag, display: display,
		turnActive: true, turnReady: ready, turnDone: make(chan struct{}), readyDone: true, fatalCh: make(chan error, 1),
	}
	runtime.runner = runner.New(ag, transport)
	result := make(chan wire.InputResult, 1)
	go func() {
		result <- runtime.AcceptInput(context.Background(), wire.InputRequest{Command: wire.Command{
			ID: "boundary-message", Type: wire.CommandMessage, Message: &wire.MessageCommandPayload{Text: "next turn"},
		}})
	}()
	deadline := time.Now().Add(time.Second)
	for {
		items, _, loadErr := source.LoadItems(context.Background(), runtime.sessionID)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if len(items) == 2 && items[1].ID == "boundary-message" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("message was not durably admitted at completion boundary: %#v", items)
		}
		time.Sleep(time.Millisecond)
	}
	if err := display.HandleEvent(context.Background(), protocol.NewFinishEvent(protocol.FinishReasonStop)); err != nil {
		t.Fatal(err)
	}
	runtime.finishTurn()
	got := <-result
	if got.Error != "" || got.Command == nil || got.Command.Type != wire.CommandMessage || got.Command.ID != "boundary-message" {
		t.Fatalf("input result = %#v", got)
	}
	items, _, err := source.LoadItems(context.Background(), runtime.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, item := range items {
		if item.ID == "boundary-message" && item.Command != nil {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("message accepted %d times: %#v", count, items)
	}
	runtime.Close()
}

func TestWorkerRuntimeIgnoresStaleSteeringAfterAgentStops(t *testing.T) {
	providers.Register("zotigod-stale-steering-test", func(config.ProfileConfig) (providers.Provider, error) {
		return &noopProvider{}, nil
	})
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := sessionlog.NewLog("sess-stale-steering", source)
	turnID, err := display.StartTurn(context.Background())
	if err != nil {
		t.Fatalf("start turn: %v", err)
	}
	localExec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: "zotigod-stale-steering-test"}, localExec)
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	ready := make(chan struct{})
	close(ready)
	runtime := &workerRuntime{
		agent:      ag,
		display:    display,
		turnActive: true,
		turnReady:  ready,
		turnDone:   make(chan struct{}),
		readyDone:  true,
	}

	err = runtime.queueTurnUserInput(context.Background(), wire.Command{
		ID:       "steering-durable",
		Sequence: 2,
		Type:     wire.CommandSteering,
		Steering: &wire.SteeringCommandPayload{
			TurnID: turnID,
			Text:   "must remain pending",
		},
	})
	if err != nil {
		t.Fatalf("accepted durable steering should not poison replay: %v", err)
	}

	err = runtime.queueTurnUserInput(context.Background(), wire.Command{
		ID:   "steering-1",
		Type: wire.CommandSteering,
		Steering: &wire.SteeringCommandPayload{
			TurnID: turnID,
			Text:   "too late",
		},
	})
	if err != nil {
		t.Fatalf("expected stale steering to be ignored, got %v", err)
	}
}

func TestStartMessageTurnRejectsUnrelatedActiveTurn(t *testing.T) {
	runtime := &workerRuntime{turnActive: true, turnDone: make(chan struct{})}
	err := runtime.startMessageTurn(context.Background(), "message", 1, &wire.MessageCommandPayload{Text: "queued"})
	if !errors.Is(err, sessionlog.ErrActiveTurn) {
		t.Fatalf("start message error = %v, want active_turn", err)
	}
}

func TestStartMessageTurnWaitsForCompletingTurnBeforeAcknowledging(t *testing.T) {
	const providerName = "zotigod-stopping-turn-test"
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{ID: "sess-stopping", CreatedAt: now, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	localExecutor, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer localExecutor.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: providerName}, localExecutor)
	if err != nil {
		t.Fatal(err)
	}
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	transport := zotigotransport.NewChannelTransport(4)
	defer transport.Close()
	runtime := &workerRuntime{
		sessionID: "sess-stopping", store: store, agent: ag,
		display:    sessionlog.NewLog("sess-stopping", source),
		turnActive: true, turnReady: make(chan struct{}), turnDone: make(chan struct{}), fatalCh: make(chan error, 1),
	}
	working := make(chan struct{}, 1)
	runtime.notifyWorking = func(context.Context) error {
		working <- struct{}{}
		return errors.New("injected working notification failure")
	}
	runtime.runner = runner.New(ag, transport)
	defer runtime.Close()
	done := make(chan error, 1)
	go func() {
		done <- runtime.startMessageTurn(context.Background(), "message-pending", 2, &wire.MessageCommandPayload{Text: "run next"})
	}()
	select {
	case err := <-done:
		t.Fatalf("pending message acknowledged before old turn stopped: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	runtime.finishTurn()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("durably started message failed with best-effort working notification: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending message did not start after old turn stopped")
	}
	select {
	case <-working:
	case <-time.After(time.Second):
		t.Fatal("native worker did not publish the explicit working barrier")
	}
	items, _, err := source.LoadItems(context.Background(), runtime.sessionID)
	if err != nil || len(items) == 0 || items[0].Type != zotigosession.DisplayItemTurnStarted {
		t.Fatalf("pending message did not produce turn_started: %#v err=%v", items, err)
	}
}
