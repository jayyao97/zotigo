package zotigod

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/gorilla/websocket"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

func TestWorkerCommandReaderFailsWhenBufferIsFull(t *testing.T) {
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

	_, _, _, _, errCh := readWorkerMessages(clientConn, nil)
	for idx := uint64(1); idx <= workerCommandBufferSize+1; idx++ {
		msg := wire.Message{
			Type: wire.MessageCommand,
			Command: &wire.Command{
				ID:       fmt.Sprintf("cmd-%d", idx),
				Sequence: idx,
				Type:     wire.CommandPause,
			},
		}
		if err := serverConn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatalf("set write deadline: %v", err)
		}
		if err := serverConn.WriteJSON(msg); err != nil {
			t.Fatalf("write command %d: %v", idx, err)
		}
	}

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "buffer full") {
			t.Fatalf("expected buffer full error, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("expected worker command reader to fail")
	}
}

func TestSessionMessageCreatesDisplayItemAndWorkerCommand(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	requests := serveTestWorkerInputs(t, worker, source, created.ID)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"build the runtime"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	var command publicCommandResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &command); err != nil {
		t.Fatalf("decode message response: %v", err)
	}
	if command.Type != wire.CommandMessage || command.Text != "build the runtime" {
		t.Fatalf("unexpected message command: %#v", command)
	}

	input := <-requests
	if input.Command.Type != wire.CommandMessage || input.Command.Message == nil || input.Command.Message.Text != "build the runtime" {
		t.Fatalf("unexpected worker input request: %#v", input)
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 1 || items.Items[0].Type != string(zotigosession.DisplayItemUserMessage) ||
		items.Items[0].Command == nil || items.Items[0].Command.Type != wire.CommandMessage {
		t.Fatalf("expected user message display item, got %#v", items.Items)
	}

	commands := getCommands(t, handler, "/internal/sessions/"+created.ID+"/commands?after=0")
	if len(commands.Commands) != 1 || commands.Commands[0].Type != wire.CommandMessage {
		t.Fatalf("expected one message command, got %#v", commands)
	}
}

func TestSessionMessageAllowsConversationBindingBeforeInputResult(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()

	const sessionID = "sess-first-codex-message"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: sessionID, Agent: "codex", WorkingDirectory: t.TempDir(), CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	registry := newSessionRegistry()
	registry.Add(Session{
		ID: sessionID, State: SessionStateRunning, Agent: "codex",
		WorkingDirectory: t.TempDir(), CreatedAt: now,
	})
	source := sessionlog.StoredItemSource{Store: store}
	handler := newHandler(registry, source, handlerOptions{store: store})
	server := httptest.NewServer(handler)
	defer server.Close()
	worker, generation := connectWorkerForActivation(t, server, sessionID, "0")
	markWorkerReady(t, server, sessionID, generation)
	defer worker.Close()

	messageDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+sessionID+"/messages", strings.NewReader(`{"text":"first message","client_message_id":"client-first"}`)))
		messageDone <- rec
	}()

	input := readWorkerMessage(t, worker)
	if input.Type != wire.MessageInputRequest || input.InputRequest == nil {
		t.Fatalf("expected input request, got %#v", input)
	}
	if err := worker.WriteJSON(wire.Message{
		Type:              wire.MessageConversationBound,
		ConversationBound: &wire.ConversationBound{ConversationID: "thread-first"},
	}); err != nil {
		t.Fatalf("send conversation binding: %v", err)
	}
	if err := worker.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	bound := readWorkerMessage(t, worker)
	if bound.Type != wire.MessageConversationBoundResult || bound.ConversationBoundResult == nil || bound.ConversationBoundResult.ErrorCode != "" {
		t.Fatalf("expected successful conversation binding, got %#v", bound)
	}
	if err := worker.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}

	accepted, err := appendAcceptedSessionInput(context.Background(), source, sessionID, input.InputRequest.Command)
	if err != nil {
		t.Fatalf("append accepted input: %v", err)
	}
	if err := worker.WriteJSON(wire.Message{Type: wire.MessageInputResult, InputResult: &wire.InputResult{
		RequestID: input.InputRequest.RequestID, Command: &accepted,
	}}); err != nil {
		t.Fatalf("send input result: %v", err)
	}

	response := <-messageDone
	if response.Code != http.StatusCreated {
		t.Fatalf("message status = %d: %s", response.Code, response.Body.String())
	}
	stored, err := store.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("load bound session: %v", err)
	}
	if stored.ConversationID != "thread-first" {
		t.Fatalf("conversation id = %q, want thread-first", stored.ConversationID)
	}
	items, _, err := source.LoadItems(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != "client-first" {
		t.Fatalf("accepted items = %#v, want one client-first command", items)
	}
}

func TestSessionMessageAllowsApprovalCallbackBeforeInputResult(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	registry := newSessionRegistry()
	handler := newHandler(registry, source)
	server := httptest.NewServer(handler)
	defer server.Close()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	messageDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"message before approval"}`)))
		messageDone <- rec
	}()
	input := readWorkerMessage(t, worker)
	if input.Type != wire.MessageInputRequest || input.InputRequest == nil {
		t.Fatalf("expected input request, got %#v", input)
	}
	if err := worker.WriteJSON(wire.Message{
		Type:            wire.MessageApprovalRequest,
		ApprovalRequest: &wire.ApprovalRequest{ID: "approval-during-input", Status: sessionlog.ApprovalStatusPending},
	}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Second); ; {
		if session, _ := registry.Get(created.ID); session.State == SessionStatePaused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("approval callback did not acquire the session lock during input admission")
		}
		time.Sleep(time.Millisecond)
	}

	accepted, err := appendAcceptedSessionInput(context.Background(), source, created.ID, input.InputRequest.Command)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.WriteJSON(wire.Message{Type: wire.MessageInputResult, InputResult: &wire.InputResult{
		RequestID: input.InputRequest.RequestID, Command: &accepted,
	}}); err != nil {
		t.Fatal(err)
	}
	if response := <-messageDone; response.Code != http.StatusCreated {
		t.Fatalf("message status = %d: %s", response.Code, response.Body.String())
	}
}

func TestSessionMessageAutoResumesStoredSession(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	workDir := t.TempDir()
	putStoredSession(t, store, "sess-stored", workDir)
	var server *httptest.Server
	workers := newWorkerRegistry()
	workerReady := make(chan *websocket.Conn, 1)
	var launchedWorkDir string
	launcher := workerLauncherFunc(func(_ context.Context, sessionID string, workingDirectory string) error {
		launchedWorkDir = workingDirectory
		go func() {
			conn, err := connectReadyWorker(server, sessionID)
			if err != nil {
				workerReady <- nil
				return
			}
			serveTestWorkerInputs(t, conn, sessionlog.StoredItemSource{Store: store}, sessionID)
			workerReady <- conn
		}()
		return nil
	})
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{
		store:                store,
		launcher:             launcher,
		workers:              workers,
		workerConnectTimeout: time.Second,
	})
	server = httptest.NewServer(handler)
	defer server.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/sess-stored/messages", strings.NewReader(`{"text":"continue this session"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	if launchedWorkDir != workDir {
		t.Fatalf("expected launcher workdir %q, got %q", workDir, launchedWorkDir)
	}
	worker := <-workerReady
	if worker == nil {
		t.Fatal("expected worker websocket connection")
	}
	defer worker.Close()

	got := getSession(t, handler, "sess-stored")
	if got.State != SessionStateRunning || !got.Live {
		t.Fatalf("expected resumed running session, got %#v", got)
	}
}

func TestSessionMessageReloadsDisplayLogAfterResume(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	putStoredSession(t, store, "sess-stored", t.TempDir())
	if _, err := store.AppendDisplayItem(context.Background(), "sess-stored", zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-stale"},
	}); err != nil {
		t.Fatalf("append stale turn: %v", err)
	}
	var server *httptest.Server
	workers := newWorkerRegistry()
	workerReady := make(chan *websocket.Conn, 1)
	launcher := workerLauncherFunc(func(_ context.Context, sessionID string, _ string) error {
		if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
			Type: zotigosession.DisplayItemTurnInterrupted,
			Turn: &zotigosession.DisplayTurn{
				ID:     "turn-stale",
				Status: "interrupted",
				Reason: sessionlog.WorkerRestartedReason,
			},
		}); err != nil {
			return err
		}
		go func() {
			conn, err := connectReadyWorker(server, sessionID)
			if err != nil {
				workerReady <- nil
				return
			}
			serveTestWorkerInputs(t, conn, sessionlog.StoredItemSource{Store: store}, sessionID)
			workerReady <- conn
		}()
		return nil
	})
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{
		store:                store,
		launcher:             launcher,
		workers:              workers,
		workerConnectTimeout: time.Second,
	})
	server = httptest.NewServer(handler)
	defer server.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/sess-stored/messages", strings.NewReader(`{"text":"continue after restart"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	worker := <-workerReady
	if worker == nil {
		t.Fatal("expected worker websocket connection")
	}
	defer worker.Close()

	items := getItems(t, handler, "/sessions/sess-stored/items?limit=10")
	if len(items.Items) != 3 {
		t.Fatalf("expected stale start, interruption, and message, got %#v", items.Items)
	}
	if items.Items[1].Type != string(zotigosession.DisplayItemTurnInterrupted) ||
		items.Items[1].Turn == nil || items.Items[1].Turn.Reason != sessionlog.WorkerRestartedReason {
		t.Fatalf("expected worker restart interruption, got %#v", items.Items[1])
	}
	if items.Items[2].Type != string(zotigosession.DisplayItemUserMessage) ||
		items.Items[2].Command == nil || items.Items[2].Command.Type != wire.CommandMessage {
		t.Fatalf("expected accepted message after interruption, got %#v", items.Items[2])
	}
}

func TestSessionMessageAcceptsImageInput(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	requests := serveTestWorkerInputs(t, worker, sessionlog.StoredItemSource{Store: store}, created.ID)

	imageBase64 := tinyPNGBase64()
	body := fmt.Sprintf(`{"text":"describe this","images":[{"mime_type":"image/png","data_base64":%q}]}`, imageBase64)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	var publicCommand publicCommandResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &publicCommand); err != nil {
		t.Fatalf("decode message response: %v", err)
	}
	if len(publicCommand.Images) != 1 {
		t.Fatalf("expected one public image metadata, got %#v", publicCommand.Images)
	}
	if publicCommand.Images[0].URL == "" {
		t.Fatalf("expected public image URL, got %#v", publicCommand.Images[0])
	}
	if strings.Contains(rec.Body.String(), "data_base64") || strings.Contains(rec.Body.String(), imageBase64) {
		t.Fatalf("public message response leaked image payload: %s", rec.Body.String())
	}

	input := <-requests
	if input.Command.Message == nil || len(input.Command.Message.Images) != 1 {
		t.Fatalf("expected worker image command, got %#v", input)
	}
	if input.Command.Message.Images[0].DataBase64 == "" {
		t.Fatalf("worker command did not include image data")
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 1 {
		t.Fatalf("expected one display item, got %#v", items.Items)
	}
	item := items.Items[0]
	if len(item.Content) != 2 || item.Content[1].Image == nil {
		t.Fatalf("expected text and image display parts, got %#v", item.Content)
	}
	if item.Content[1].Image.MediaType != "image/png" || item.Content[1].Image.SizeBytes == 0 ||
		item.Content[1].Image.Width != 1 || item.Content[1].Image.Height != 1 {
		t.Fatalf("unexpected image metadata: %#v", item.Content[1].Image)
	}
	if item.Content[1].Image.URL == "" || item.Command == nil || len(item.Command.Images) != 1 || item.Command.Images[0].URL == "" {
		t.Fatalf("expected item image URLs, got content=%#v command=%#v", item.Content[1].Image, item.Command)
	}
	encodedItems, err := sonic.MarshalString(items)
	if err != nil {
		t.Fatalf("encode items: %v", err)
	}
	if strings.Contains(encodedItems, imageBase64) {
		t.Fatalf("items response leaked image base64")
	}
	if err := os.Remove(filepath.Join(root, "sessions", created.ID+".display.jsonl")); err != nil {
		t.Fatalf("remove display log: %v", err)
	}

	imageRec := httptest.NewRecorder()
	handler.ServeHTTP(imageRec, httptest.NewRequest(http.MethodGet, item.Content[1].Image.URL, nil))
	if imageRec.Code != http.StatusOK {
		t.Fatalf("expected image status %d, got %d: %s", http.StatusOK, imageRec.Code, imageRec.Body.String())
	}
	expectedImage, err := base64.StdEncoding.DecodeString(imageBase64)
	if err != nil {
		t.Fatalf("decode image fixture: %v", err)
	}
	if imageRec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("expected image/png, got %q", imageRec.Header().Get("Content-Type"))
	}
	if !bytes.Equal(imageRec.Body.Bytes(), expectedImage) {
		t.Fatalf("unexpected image bytes")
	}
}

func TestSessionMessageAcceptsImageOnlyInput(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	requests := serveTestWorkerInputs(t, worker, sessionlog.StoredItemSource{Store: store}, created.ID)

	imageBase64 := tinyPNGBase64()
	body := fmt.Sprintf(`{"images":[{"mime_type":"image/png","data_base64":%q}]}`, imageBase64)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	var publicCommand publicCommandResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &publicCommand); err != nil {
		t.Fatalf("decode message response: %v", err)
	}
	if publicCommand.Text != "" {
		t.Fatalf("expected empty public text, got %q", publicCommand.Text)
	}
	if len(publicCommand.Images) != 1 {
		t.Fatalf("expected one public image metadata, got %#v", publicCommand.Images)
	}

	input := <-requests
	if input.Command.Message == nil {
		t.Fatalf("expected worker message command, got %#v", input)
	}
	if input.Command.Message.Text != "" {
		t.Fatalf("expected empty worker text, got %q", input.Command.Message.Text)
	}
	if len(input.Command.Message.Images) != 1 || input.Command.Message.Images[0].DataBase64 == "" {
		t.Fatalf("expected worker image payload, got %#v", input.Command.Message.Images)
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 1 {
		t.Fatalf("expected one display item, got %#v", items.Items)
	}
	item := items.Items[0]
	if len(item.Content) != 1 || item.Content[0].Text != "" || item.Content[0].Image == nil {
		t.Fatalf("expected image-only display item, got %#v", item.Content)
	}
	if strings.Contains(rec.Body.String(), "data_base64") || strings.Contains(rec.Body.String(), imageBase64) {
		t.Fatalf("public message response leaked image payload: %s", rec.Body.String())
	}
}

func TestSessionImageReadRejectsUnreferencedBlob(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})

	created := createSession(t, handler)
	dir := filepath.Join(root, "sessions", created.ID+".images")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("create image dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "orphan.png"), []byte("orphan"), 0600); err != nil {
		t.Fatalf("write orphan image: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/"+created.ID+"/images/orphan.png", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNotFound, rec.Code, rec.Body.String())
	}
}

func TestSessionImageReadRejectsNestedImageName(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})

	created := createSession(t, handler)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/"+created.ID+"/images/nested/secret.png", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNotFound, rec.Code, rec.Body.String())
	}
}

func TestSessionImageReadBackfillsLegacyDisplayLogReference(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})

	created := createSession(t, handler)
	imageData, err := base64.StdEncoding.DecodeString(tinyPNGBase64())
	if err != nil {
		t.Fatalf("decode image fixture: %v", err)
	}
	blobPath := filepath.Join("sessions", created.ID+".images", "legacy.png")
	if err := os.MkdirAll(filepath.Join(root, "sessions", created.ID+".images"), 0700); err != nil {
		t.Fatalf("create image dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, blobPath), imageData, 0600); err != nil {
		t.Fatalf("write legacy image: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemUserMessage,
		Command: &zotigosession.DisplayCommand{
			Type: wire.CommandMessage,
			Images: []zotigosession.DisplayCommandImage{{
				MimeType:  "image/png",
				SizeBytes: len(imageData),
				Width:     1,
				Height:    1,
				BlobPath:  blobPath,
			}},
		},
	}); err != nil {
		t.Fatalf("append legacy image display item: %v", err)
	}

	imageURL := "/sessions/" + created.ID + "/images/legacy.png"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, imageURL, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), imageData) {
		t.Fatalf("unexpected image bytes")
	}

	if err := os.Remove(filepath.Join(root, "sessions", created.ID+".display.jsonl")); err != nil {
		t.Fatalf("remove display log: %v", err)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, imageURL, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected indexed image status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), imageData) {
		t.Fatalf("unexpected indexed image bytes")
	}
}

func TestSessionMessageRejectsEmptyTextWithoutImages(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})

	created := createSession(t, handler)
	startSession(t, handler, created.ID)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"   "}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "message requires text or images") {
		t.Fatalf("unexpected response: %s", rec.Body.String())
	}
}

func TestSessionMessageRejectsImageInputWithoutBlobPersistence(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	body := fmt.Sprintf(`{"text":"describe this","images":[{"mime_type":"image/png","data_base64":%q}]}`, tinyPNGBase64())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d: %s", http.StatusInternalServerError, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "image persistence is not configured") {
		t.Fatalf("expected image persistence error, got %q", rec.Body.String())
	}
}

func TestSessionMessageImageCommandReplaysFromBlob(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	serveTestWorkerInputs(t, worker, sessionlog.StoredItemSource{Store: store}, created.ID)

	imageBase64 := tinyPNGBase64()
	body := fmt.Sprintf(`{"text":"describe this","images":[{"mime_type":"image/png","data_base64":%q}]}`, imageBase64)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	rawLog, err := os.ReadFile(filepath.Join(root, "sessions", created.ID+".display.jsonl"))
	if err != nil {
		t.Fatalf("read display log: %v", err)
	}
	if strings.Contains(string(rawLog), imageBase64) {
		t.Fatalf("display log leaked image base64")
	}
	if strings.Contains(string(rawLog), root) {
		t.Fatalf("display log leaked absolute store root: %s", string(rawLog))
	}

	commands := getCommands(t, handler, "/internal/sessions/"+created.ID+"/commands?after=0")
	if len(commands.Commands) != 1 || commands.Commands[0].Message == nil || len(commands.Commands[0].Message.Images) != 1 {
		t.Fatalf("expected replayable image command, got %#v", commands)
	}
	if commands.Commands[0].Message.Images[0].DataBase64 == "" {
		t.Fatalf("replayed command did not hydrate image payload")
	}
}

func TestSessionMessageFailedDuplicateImageDoesNotDeleteAcceptedBlob(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	serveTestWorkerInputs(t, worker, sessionlog.StoredItemSource{Store: store}, created.ID)

	body := fmt.Sprintf(`{"text":"describe this","images":[{"mime_type":"image/png","data_base64":%q}]}`, tinyPNGBase64())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, rec.Code, rec.Body.String())
	}
	assertAPIError(t, rec, http.StatusConflict, "command_pending", "already pending")

	commands := getCommands(t, handler, "/internal/sessions/"+created.ID+"/commands?after=0")
	if len(commands.Commands) != 1 || commands.Commands[0].Message == nil || len(commands.Commands[0].Message.Images) != 1 ||
		commands.Commands[0].Message.Images[0].DataBase64 == "" {
		t.Fatalf("expected accepted image command to remain replayable, got %#v", commands)
	}
}

func TestWorkerCommandsSkipsMissingImageBlob(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	serveTestWorkerInputs(t, worker, sessionlog.StoredItemSource{Store: store}, created.ID)

	body := fmt.Sprintf(`{"text":"describe this","images":[{"mime_type":"image/png","data_base64":%q}]}`, tinyPNGBase64())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	if err := os.RemoveAll(filepath.Join(root, "sessions", created.ID+".images")); err != nil {
		t.Fatalf("remove image dir: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemSessionCommand,
		Command: &zotigosession.DisplayCommand{
			Type:   wire.CommandPause,
			Reason: wire.UserPauseReason,
		},
	}); err != nil {
		t.Fatalf("append pause command: %v", err)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/internal/sessions/"+created.ID+"/commands?after=0", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var commands wire.CommandsPage
	if err := sonic.Unmarshal(rec.Body.Bytes(), &commands); err != nil {
		t.Fatalf("decode commands: %v", err)
	}
	if len(commands.Commands) != 1 || commands.Commands[0].Type != wire.CommandPause {
		t.Fatalf("expected missing image command to be skipped, got %#v", commands)
	}
}

func TestSessionMessageRejectsInvalidImages(t *testing.T) {
	tests := []struct {
		name string
		body string
		code int
	}{
		{
			name: "invalid base64",
			body: `{"text":"hello","images":[{"mime_type":"image/png","data_base64":"not-base64"}]}`,
			code: http.StatusBadRequest,
		},
		{
			name: "unsupported mime type",
			body: fmt.Sprintf(`{"text":"hello","images":[{"mime_type":"image/gif","data_base64":%q}]}`, tinyPNGBase64()),
			code: http.StatusBadRequest,
		},
		{
			name: "too many images",
			body: tooManyImagesBody(),
			code: http.StatusBadRequest,
		},
		{
			name: "image too large",
			body: fmt.Sprintf(`{"text":"hello","images":[{"mime_type":"image/png","data_base64":%q}]}`,
				base64.StdEncoding.EncodeToString(make([]byte, maxMessageImageBytes+1))),
			code: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
			handler := newHandler(newSessionRegistry(), source)
			server := httptest.NewServer(handler)
			defer server.Close()

			created := createSession(t, handler)
			startSession(t, handler, created.ID)
			worker := dialWorker(t, server, created.ID)
			defer worker.Close()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(tt.body))
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.code {
				t.Fatalf("expected status %d, got %d: %s", tt.code, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestSessionMessageRejectsOversizedRequestBody(t *testing.T) {
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}})
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	rec := httptest.NewRecorder()
	body := `{"text":"` + strings.Repeat("x", maxMessageRequestBytes) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected status %d, got %d: %s", http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	}
}

func TestSessionSteeringAcceptsImageOnlyInput(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	requests := serveTestWorkerInputs(t, worker, sessionlog.StoredItemSource{Store: store}, created.ID)
	if _, err := store.AppendDisplayItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
	}); err != nil {
		t.Fatalf("append turn started: %v", err)
	}

	imageBase64 := tinyPNGBase64()
	rec := httptest.NewRecorder()
	body := fmt.Sprintf(`{"images":[{"mime_type":"image/png","data_base64":%q}]}`, imageBase64)
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/steering", strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	var publicCommand publicCommandResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &publicCommand); err != nil {
		t.Fatalf("decode steering response: %v", err)
	}
	if publicCommand.Text != "" || publicCommand.TurnID != "turn-1" || len(publicCommand.Images) != 1 {
		t.Fatalf("unexpected public steering response: %#v", publicCommand)
	}
	if publicCommand.Images[0].URL == "" {
		t.Fatalf("expected public steering image URL, got %#v", publicCommand.Images[0])
	}
	if strings.Contains(rec.Body.String(), "data_base64") || strings.Contains(rec.Body.String(), imageBase64) {
		t.Fatalf("public steering response leaked image payload: %s", rec.Body.String())
	}

	input := <-requests
	if input.Command.Steering == nil || len(input.Command.Steering.Images) != 1 {
		t.Fatalf("expected worker steering image command, got %#v", input)
	}
	if input.Command.Steering.Images[0].DataBase64 == "" {
		t.Fatalf("worker steering command did not include image data")
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 2 || items.Items[1].Type != string(zotigosession.DisplayItemSessionCommand) {
		t.Fatalf("steering command was not persisted: %#v", items.Items)
	}

	commands := getCommands(t, handler, "/internal/sessions/"+created.ID+"/commands?after=0")
	if len(commands.Commands) != 1 || commands.Commands[0].Type != wire.CommandSteering {
		t.Fatalf("steering did not enter the durable command queue: %#v", commands)
	}

	imageRec := httptest.NewRecorder()
	handler.ServeHTTP(imageRec, httptest.NewRequest(http.MethodGet, publicCommand.Images[0].URL, nil))
	if imageRec.Code != http.StatusOK {
		t.Fatalf("expected image status %d, got %d: %s", http.StatusOK, imageRec.Code, imageRec.Body.String())
	}
	expectedImage, err := base64.StdEncoding.DecodeString(imageBase64)
	if err != nil {
		t.Fatalf("decode image fixture: %v", err)
	}
	if !bytes.Equal(imageRec.Body.Bytes(), expectedImage) {
		t.Fatalf("unexpected image bytes")
	}
}

func TestSessionSteeringRejectsOversizedRequestBody(t *testing.T) {
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}})
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	rec := httptest.NewRecorder()
	body := `{"text":"` + strings.Repeat("x", maxMessageRequestBytes) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/steering", strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected status %d, got %d: %s", http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	}
}

func TestSessionMessageDoesNotReturnAcceptedWhenWorkerDisconnectsBeforeAdmission(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	workers := newWorkerRegistry()
	registry := newSessionRegistry()
	handler := newHandler(registry, source, handlerOptions{workers: workers})
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	go func() {
		var message wire.Message
		if worker.ReadJSON(&message) == nil && message.Type == wire.MessageInputRequest {
			_ = worker.Close()
		}
	}()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"persist this"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status %d, got %d: %s", http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	}

	commands := getCommands(t, handler, "/internal/sessions/"+created.ID+"/commands?after=0")
	if len(commands.Commands) != 0 {
		t.Fatalf("unexpected command accepted before worker admission: %#v", commands)
	}
	session, _ := registry.Get(created.ID)
	if session.State != SessionStateRunning {
		t.Fatalf("worker disconnect rewrote lifecycle state: %#v", session)
	}
}

func TestSessionPauseInterruptsAndDrainsPendingInput(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registry := newSessionRegistry()
	handler := newHandler(registry, source, handlerOptions{store: store})
	server := httptest.NewServer(handler)
	defer server.Close()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	stored, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.Agent = string(zotigoruntime.AgentCodex)
	stored.ConversationID = "thread-1"
	if err := store.Put(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	registry.mu.Lock()
	live := registry.sessions[created.ID]
	live.Agent = string(zotigoruntime.AgentCodex)
	registry.sessions[created.ID] = live
	registry.mu.Unlock()
	appendTurnStarted(t, source, created.ID, "turn-1")

	messageDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"pending steer","client_message_id":"pending-once"}`))
		handler.ServeHTTP(rec, req)
		messageDone <- rec
	}()
	input := readWorkerMessage(t, worker)
	if input.Type != wire.MessageInputRequest || input.InputRequest == nil {
		t.Fatalf("expected input request, got %#v", input)
	}

	pauseDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/pause", nil))
		pauseDone <- rec
	}()
	interrupt := readWorkerMessage(t, worker)
	if interrupt.Type != wire.MessageInterruptTurn || interrupt.InterruptTurn == nil || interrupt.InterruptTurn.TurnID != "turn-1" {
		t.Fatalf("expected urgent turn interrupt, got %#v", interrupt)
	}

	late := httptest.NewRecorder()
	lateRequest := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"too late"}`))
	handler.ServeHTTP(late, lateRequest)
	if late.Code != http.StatusConflict || !strings.Contains(late.Body.String(), "turn_stopping") {
		t.Fatalf("late message status = %d: %s", late.Code, late.Body.String())
	}

	accepted, err := appendAcceptedSessionInput(context.Background(), source, created.ID, steeringCommandForRequest(*input.InputRequest, "turn-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.WriteJSON(wire.Message{Type: wire.MessageInputResult, InputResult: &wire.InputResult{
		RequestID: input.InputRequest.RequestID, Command: &accepted,
	}}); err != nil {
		t.Fatal(err)
	}
	if message := <-messageDone; message.Code != http.StatusCreated {
		t.Fatalf("pending message status = %d: %s", message.Code, message.Body.String())
	}
	pauseCommand := readWorkerMessage(t, worker)
	if pauseCommand.Type != wire.MessageCommand || pauseCommand.Command == nil || pauseCommand.Command.Type != wire.CommandPause {
		t.Fatalf("expected durable pause command, got %#v", pauseCommand)
	}
	if pause := <-pauseDone; pause.Code != http.StatusAccepted {
		t.Fatalf("pause status = %d: %s", pause.Code, pause.Body.String())
	}

	items, _, err := source.LoadItems(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	acceptedCount := 0
	for _, item := range items {
		if item.ID == "pending-once" {
			acceptedCount++
		}
	}
	if acceptedCount != 1 {
		t.Fatalf("pending input persisted %d times: %#v", acceptedCount, items)
	}
}

func TestSessionPauseTimeoutKeepsPendingInputAndWorker(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	workers := newWorkerRegistry()
	handler := newHandler(newSessionRegistry(), source, handlerOptions{workers: workers, inputStopTimeout: 20 * time.Millisecond})
	server := httptest.NewServer(handler)
	defer server.Close()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	appendTurnStarted(t, source, created.ID, "turn-1")

	messageDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"late durable input","client_message_id":"late-durable"}`))
		handler.ServeHTTP(rec, req)
		messageDone <- rec
	}()
	input := readWorkerMessage(t, worker)
	if input.Type != wire.MessageInputRequest || input.InputRequest == nil {
		t.Fatalf("expected input request, got %#v", input)
	}

	pause := httptest.NewRecorder()
	handler.ServeHTTP(pause, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/pause", nil))
	if pause.Code != http.StatusGatewayTimeout || !strings.Contains(pause.Body.String(), "input_stop_timeout") {
		t.Fatalf("pause status = %d: %s", pause.Code, pause.Body.String())
	}
	if !workers.Has(created.ID) || workers.InputStopping(created.ID) {
		t.Fatalf("timeout replaced worker or left stop gate set")
	}

	accepted, err := appendAcceptedSessionInput(context.Background(), source, created.ID, steeringCommandForRequest(*input.InputRequest, "turn-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.WriteJSON(wire.Message{Type: wire.MessageInputResult, InputResult: &wire.InputResult{
		RequestID: input.InputRequest.RequestID, Command: &accepted,
	}}); err != nil {
		t.Fatal(err)
	}
	if message := <-messageDone; message.Code != http.StatusCreated {
		t.Fatalf("late input status = %d: %s", message.Code, message.Body.String())
	}
	items, _, err := source.LoadItems(context.Background(), created.ID)
	if err != nil || len(items) != 2 || items[1].ID != "late-durable" {
		t.Fatalf("late input was not retained: %#v, err=%v", items, err)
	}
}

func TestSessionMessageSteersActiveTurn(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	serveTestWorkerInputs(t, worker, source, created.ID)
	appendTurnStarted(t, source, created.ID, "turn-1")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"second message"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	var command publicCommandResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &command); err != nil {
		t.Fatal(err)
	}
	if command.Type != wire.CommandSteering || command.TurnID != "turn-1" {
		t.Fatalf("message was not routed to steering: %#v", command)
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 2 || items.Items[1].Command == nil || items.Items[1].Command.Type != wire.CommandSteering {
		t.Fatalf("expected one steering command, got %#v", items.Items)
	}
}

func TestSessionMessageRejectsPendingMessageCommand(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	serveTestWorkerInputs(t, worker, source, created.ID)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"first message"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"second message"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, rec.Code, rec.Body.String())
	}
	assertAPIError(t, rec, http.StatusConflict, "command_pending", "already pending")

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 1 || items.Items[0].Command == nil || items.Items[0].Command.Type != wire.CommandMessage {
		t.Fatalf("expected only the pending first message command, got %#v", items.Items)
	}
}

func TestSessionMessageAdmissionIsSerialized(t *testing.T) {
	var appendCalls atomic.Int32
	appendEntered := make(chan struct{})
	releaseAppend := make(chan struct{})
	source := &fakeDisplayItemSource{
		items: map[string][]zotigosession.DisplayItem{},
		appendErr: func(_ string, item zotigosession.DisplayItem) error {
			if item.Command == nil || item.Command.Type != wire.CommandMessage {
				return nil
			}
			if appendCalls.Add(1) == 1 {
				close(appendEntered)
			}
			<-releaseAppend
			return nil
		},
	}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	serveTestWorkerInputs(t, worker, source, created.ID)

	results := make(chan int, 2)
	postMessage := func(text string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(fmt.Sprintf(`{"text":%q}`, text)))
		handler.ServeHTTP(rec, req)
		results <- rec.Code
	}
	go postMessage("first message")
	select {
	case <-appendEntered:
	case <-time.After(time.Second):
		t.Fatal("expected first append to block")
	}
	go postMessage("second message")
	time.Sleep(20 * time.Millisecond)
	close(releaseAppend)

	got := []int{<-results, <-results}
	createdCount := 0
	conflictCount := 0
	for _, code := range got {
		switch code {
		case http.StatusCreated:
			createdCount++
		case http.StatusConflict:
			conflictCount++
		default:
			t.Fatalf("unexpected status codes %#v", got)
		}
	}
	if createdCount != 1 || conflictCount != 1 {
		t.Fatalf("expected one created and one conflict, got %#v", got)
	}
	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 1 || items.Items[0].Command == nil || items.Items[0].Command.Type != wire.CommandMessage {
		t.Fatalf("expected one message command, got %#v", items.Items)
	}
}

func TestSessionMessageClientIDIsIdempotent(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	registry := newSessionRegistry()
	handler := newHandler(registry, source)
	server := httptest.NewServer(handler)
	defer server.Close()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	serveTestWorkerInputs(t, worker, source, created.ID)

	post := func() publicCommandResponse {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"once","client_message_id":"client-1"}`)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("message status = %d: %s", rec.Code, rec.Body.String())
		}
		var command publicCommandResponse
		if err := decodeAPIData(t, rec.Body.Bytes(), &command); err != nil {
			t.Fatal(err)
		}
		return command
	}
	first := post()
	appendTurnStarted(t, source, created.ID, "turn-finished")
	if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnCompleted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-finished", Status: "completed"},
	}); err != nil {
		t.Fatal(err)
	}
	registry.MarkIdle(created.ID)
	second := post()
	if first.ID != "client-1" || second.ID != first.ID || second.Sequence != first.Sequence || second.Type != first.Type {
		t.Fatalf("idempotent responses differ: first=%#v second=%#v", first, second)
	}
	conflict := httptest.NewRecorder()
	handler.ServeHTTP(conflict, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"different","client_message_id":"client-1"}`)))
	assertAPIError(t, conflict, http.StatusConflict, "command_id_conflict", "different input")
	commands := getCommands(t, handler, "/internal/sessions/"+created.ID+"/commands?after=0")
	if len(commands.Commands) != 1 || commands.Commands[0].ID != "client-1" {
		t.Fatalf("duplicate client ID created commands: %#v", commands)
	}
	if session := getSession(t, handler, created.ID); session.Working {
		t.Fatalf("idempotent retry marked completed session working: %#v", session)
	}
}

func TestSessionPauseCreatesWorkerCommand(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	registry := newSessionRegistry()
	handler := newHandler(registry, source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	serveTestWorkerInputs(t, worker, source, created.ID)
	if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
	}); err != nil {
		t.Fatalf("append turn started: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/pause", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, rec.Code, rec.Body.String())
	}
	var command publicCommandResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &command); err != nil {
		t.Fatalf("decode pause response: %v", err)
	}
	if command.Type != wire.CommandPause || command.TurnID != "turn-1" || command.Reason != wire.UserPauseReason {
		t.Fatalf("unexpected pause command: %#v", command)
	}
	if got := getSession(t, handler, created.ID); got.State != SessionStatePausing || !got.Working {
		t.Fatalf("expected session to be pausing and working, got %#v", got)
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 2 {
		t.Fatalf("expected turn_started and pause command items, got %#v", items.Items)
	}
	if items.Items[1].Type != string(zotigosession.DisplayItemSessionCommand) || items.Items[1].Command == nil {
		t.Fatalf("expected pause command item, got %#v", items.Items[1])
	}

	commands := getCommands(t, handler, "/internal/sessions/"+created.ID+"/commands?after=0")
	if len(commands.Commands) != 1 || commands.Commands[0].Type != wire.CommandPause {
		t.Fatalf("expected one pause command, got %#v", commands)
	}

	secondPause := httptest.NewRecorder()
	handler.ServeHTTP(secondPause, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/pause", nil))
	assertAPIError(t, secondPause, http.StatusConflict, "turn_stopping", "already stopping")
	steering := httptest.NewRecorder()
	handler.ServeHTTP(steering, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/steering", strings.NewReader(`{"text":"one more correction"}`)))
	if steering.Code != http.StatusCreated {
		t.Fatalf("steering during pause status = %d: %s", steering.Code, steering.Body.String())
	}

	if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnInterrupted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1", Status: "interrupted", Reason: wire.UserPauseReason},
	}); err != nil {
		t.Fatal(err)
	}
	registry.MarkIdle(created.ID)
	if got := getSession(t, handler, created.ID); got.State != SessionStateRunning || got.Working {
		t.Fatalf("worker idle did not complete pausing state: %#v", got)
	}
}

func TestMessageWhileTurnIsStoppingBecomesDurableSteering(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	registry := newSessionRegistry()
	handler := newHandler(registry, source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	serveTestWorkerInputs(t, worker, source, created.ID)
	appendTurnStarted(t, source, created.ID, "turn-old")

	pause := httptest.NewRecorder()
	handler.ServeHTTP(pause, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/pause", nil))
	if pause.Code != http.StatusAccepted {
		t.Fatalf("pause status = %d: %s", pause.Code, pause.Body.String())
	}
	message := httptest.NewRecorder()
	handler.ServeHTTP(message, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"run after pause"}`)))
	if message.Code != http.StatusCreated {
		t.Fatalf("steering message status = %d: %s", message.Code, message.Body.String())
	}
	var command publicCommandResponse
	if err := decodeAPIData(t, message.Body.Bytes(), &command); err != nil {
		t.Fatal(err)
	}
	if command.Type != wire.CommandSteering || command.TurnID != "turn-old" {
		t.Fatalf("message was not routed to the stopping turn: %#v", command)
	}

	items, _, err := source.LoadItems(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[2].Command == nil || items[2].Command.Type != wire.CommandSteering || items[2].Command.TurnID != "turn-old" {
		t.Fatalf("steering message is not durable: %#v", items)
	}
}

func TestSessionPauseAcceptsTurnCompletedDuringAdmission(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	var completed atomic.Bool
	source.appendErr = func(sessionID string, item zotigosession.DisplayItem) error {
		if item.Command == nil || item.Command.Type != wire.CommandPause || !completed.CompareAndSwap(false, true) {
			return nil
		}
		_, err := source.AppendItem(context.Background(), sessionID, zotigosession.DisplayItem{
			Type: zotigosession.DisplayItemTurnCompleted,
			Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
		})
		return err
	}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	appendTurnStarted(t, source, created.ID, "turn-1")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/pause", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, rec.Code, rec.Body.String())
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 3 || items.Items[1].Type != string(zotigosession.DisplayItemTurnCompleted) || items.Items[2].Command == nil || items.Items[2].Command.Type != wire.CommandPause {
		t.Fatalf("expected lifecycle completion followed by an idempotent pause, got %#v", items.Items)
	}
}

func TestSessionPauseRejectsPendingApprovalBeforeRegistryPause(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	appendTurnStarted(t, source, created.ID, "turn-1")
	appendPendingApprovalTurn(t, source, created.ID, "turn-1", "approval-1")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/pause", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "approval is pending") {
		t.Fatalf("expected pending approval conflict, got %q", rec.Body.String())
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 3 {
		t.Fatalf("expected no pause command to be appended, got %#v", items.Items)
	}
}

func TestSessionPauseLaunchesMissingWorker(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	var server *httptest.Server
	workers := newWorkerRegistry()
	workerReady := make(chan *websocket.Conn, 1)
	launcher := workerLauncherFunc(func(_ context.Context, sessionID string, _ string) error {
		go func() {
			conn, err := connectReadyWorker(server, sessionID)
			if err != nil {
				workerReady <- nil
				return
			}
			workerReady <- conn
		}()
		return nil
	})
	handler := newHandler(newSessionRegistry(), source, handlerOptions{
		launcher:             launcher,
		workers:              workers,
		workerConnectTimeout: time.Second,
	})
	server = httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := <-workerReady
	if worker == nil {
		t.Fatal("expected initial worker websocket connection")
	}
	worker.Close()
	for deadline := time.Now().Add(time.Second); workers.Has(created.ID) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if workers.Has(created.ID) {
		t.Fatal("expected closed worker to unregister")
	}
	appendTurnStarted(t, source, created.ID, "turn-1")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/pause", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, rec.Code, rec.Body.String())
	}
	restarted := <-workerReady
	if restarted == nil {
		t.Fatal("expected restarted worker websocket connection")
	}
	defer restarted.Close()
	msg := readWorkerMessage(t, restarted)
	if msg.Command == nil || msg.Command.Type != wire.CommandPause {
		t.Fatalf("expected pause command after worker restart, got %#v", msg)
	}
}

func TestWorkerTurnInterruptedConfirmsPauseLifecycle(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
	}); err != nil {
		t.Fatalf("append turn started: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/pause", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, rec.Code, rec.Body.String())
	}
	var pauseCommand publicCommandResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &pauseCommand); err != nil {
		t.Fatalf("decode pause response: %v", err)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/turn/interrupted", strings.NewReader(`{
		"turn_id":"turn-1",
		"reason":"user_pause",
		"duration_ms":123
	}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	got := items.Items[len(items.Items)-1]
	if got.Type != string(zotigosession.DisplayItemTurnInterrupted) || got.Turn == nil {
		t.Fatalf("expected confirmed turn_interrupted item, got %#v", got)
	}
	if got.Turn.ID != "turn-1" || got.Turn.Reason != wire.UserPauseReason || got.Turn.DurationMS != 123 {
		t.Fatalf("unexpected interrupted turn: %#v", got.Turn)
	}

	commands := getCommands(t, handler, fmt.Sprintf("/internal/sessions/%s/commands?after=%d", created.ID, pauseCommand.Sequence))
	if len(commands.Commands) != 0 {
		t.Fatalf("expected lifecycle confirmation not to be returned as command, got %#v", commands)
	}
}

func TestWorkerTurnInterruptedRejectsMissingOrStaleTurn(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)
	appendTurnStarted(t, source, created.ID, "turn-1")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/turn/interrupted", strings.NewReader(`{
		"reason":"user_pause"
	}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/turn/interrupted", strings.NewReader(`{
		"turn_id":"turn-stale",
		"reason":"user_pause"
	}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, rec.Code, rec.Body.String())
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 1 || items.Items[0].Type != string(zotigosession.DisplayItemTurnStarted) {
		t.Fatalf("expected no interrupted item, got %#v", items.Items)
	}
}

func TestSessionSteeringPersistsAndForwardsCommand(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	requests := serveTestWorkerInputs(t, worker, source, created.ID)
	appendTurnStarted(t, source, created.ID, "turn-1")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/steering", strings.NewReader(`{"text":"use the smaller fix"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	var command publicCommandResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &command); err != nil {
		t.Fatalf("decode steering response: %v", err)
	}
	if command.Type != wire.CommandSteering || command.Text != "use the smaller fix" || command.TurnID != "turn-1" {
		t.Fatalf("unexpected steering command: %#v", command)
	}
	input := <-requests
	if input.Command.ID != command.ID || input.Command.Steering == nil || input.Command.Steering.Text != "use the smaller fix" {
		t.Fatalf("unexpected worker steering input: %#v", input)
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 2 || items.Items[1].Type != string(zotigosession.DisplayItemSessionCommand) {
		t.Fatalf("steering command was not persisted: %#v", items.Items)
	}

	commands := getCommands(t, handler, "/internal/sessions/"+created.ID+"/commands?after=0")
	if len(commands.Commands) != 1 || commands.Commands[0].ID != command.ID || commands.Commands[0].Type != wire.CommandSteering {
		t.Fatalf("steering command was not replayable: %#v", commands)
	}
}

func TestSessionSteeringRequiresActiveTurn(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	serveTestWorkerInputs(t, worker, source, created.ID)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/steering", strings.NewReader(`{"text":"late"}`)))
	assertAPIError(t, rec, http.StatusConflict, "no_active_turn", "requires an active turn")
}

func TestSessionSteeringExpectedTurnMismatchDoesNotDeliver(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	serveTestWorkerInputs(t, worker, source, created.ID)
	appendTurnStarted(t, source, created.ID, "turn-current")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/steering", strings.NewReader(`{"text":"wrong turn","expected_turn_id":"turn-old"}`)))
	assertAPIError(t, rec, http.StatusConflict, "turn_mismatch", "does not match")
	commands := getCommands(t, handler, "/internal/sessions/"+created.ID+"/commands?after=0")
	if len(commands.Commands) != 0 {
		t.Fatalf("mismatched steering was delivered: %#v", commands)
	}
}

func TestSessionMessageSteersPausedApprovalTurn(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	registry := newSessionRegistry()
	handler := newHandler(registry, source)
	server := httptest.NewServer(handler)
	defer server.Close()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	serveTestWorkerInputs(t, worker, source, created.ID)
	appendTurnStarted(t, source, created.ID, "turn-1")
	appendPendingApprovalTurn(t, source, created.ID, "turn-1", "approval-1")
	if _, err := registry.Pause(created.ID); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"use another command"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 4 || items.Items[3].Command == nil || items.Items[3].Command.Type != wire.CommandSteering {
		t.Fatalf("expected durable steering after pending approval, got %#v", items.Items)
	}
}

func TestWorkerCommandsDoNotReplayAppliedSteeringItems(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	appendTurnStarted(t, source, created.ID, "turn-1")

	if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{
		ID:      "applied-steering",
		Type:    zotigosession.DisplayItemSteeringMessage,
		Role:    string(protocol.RoleUser),
		Content: []zotigosession.DisplayContentPart{{Type: string(protocol.ContentTypeText), Text: "correction"}},
		Turn:    &zotigosession.DisplayTurn{ID: "turn-1"},
		Command: &zotigosession.DisplayCommand{Type: wire.CommandSteering, Text: "correction", TurnID: "turn-1"},
	}); err != nil {
		t.Fatalf("append applied steering: %v", err)
	}

	commands := getCommands(t, handler, "/internal/sessions/"+created.ID+"/commands?after=0")
	if len(commands.Commands) != 0 {
		t.Fatalf("applied steering item was replayed as a command: %#v", commands)
	}
}

func TestWorkerCommandsSupportDisplayLogOffset(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	source := sessionlog.StoredItemSource{Store: store}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)

	if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{Type: zotigosession.DisplayItemTurnStarted}); err != nil {
		t.Fatalf("append non-command: %v", err)
	}
	if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemSessionCommand,
		Command: &zotigosession.DisplayCommand{
			Type: wire.CommandMessage,
			Text: "first",
		},
	}); err != nil {
		t.Fatalf("append first command: %v", err)
	}
	if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemSessionCommand,
		Command: &zotigosession.DisplayCommand{
			Type: wire.CommandPause,
		},
	}); err != nil {
		t.Fatalf("append second command: %v", err)
	}

	first := getCommands(t, handler, "/internal/sessions/"+created.ID+"/commands?offset=0&limit=1")
	if len(first.Commands) != 1 || first.Commands[0].Type != wire.CommandMessage {
		t.Fatalf("expected first command, got %#v", first)
	}
	if first.NextOffset <= 0 {
		t.Fatalf("expected positive next offset, got %d", first.NextOffset)
	}

	second := getCommands(t, handler, fmt.Sprintf("/internal/sessions/%s/commands?offset=%d&limit=1", created.ID, first.NextOffset))
	if len(second.Commands) != 1 || second.Commands[0].Type != wire.CommandPause {
		t.Fatalf("expected second command, got %#v", second)
	}
	if second.NextOffset <= first.NextOffset {
		t.Fatalf("expected offset to advance, got %d after %d", second.NextOffset, first.NextOffset)
	}

	if _, _, _, err := store.ListDisplayItemsFromOffset(context.Background(), created.ID, second.NextOffset, 10); err != nil {
		t.Fatalf("offset should point to a valid display log boundary: %v", err)
	}
}

func TestWorkerCommandsOffsetScansInBatches(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	for idx := 0; idx < 20; idx++ {
		if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{Type: zotigosession.DisplayItemAssistantMessage}); err != nil {
			t.Fatalf("append non-command: %v", err)
		}
	}
	if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemSessionCommand,
		Command: &zotigosession.DisplayCommand{
			Type: wire.CommandMessage,
			Text: "batched",
		},
	}); err != nil {
		t.Fatalf("append command: %v", err)
	}

	commands := getCommands(t, handler, "/internal/sessions/"+created.ID+"/commands?offset=0&limit=1")
	if len(commands.Commands) != 1 || commands.Commands[0].Message == nil || commands.Commands[0].Message.Text != "batched" {
		t.Fatalf("expected command after batched scan, got %#v", commands)
	}
	if got := source.offsetCalls.Load(); got != 1 {
		t.Fatalf("expected one batched offset read, got %d", got)
	}
	if len(source.offsetMaxLines) != 1 || source.offsetMaxLines[0] != commandOffsetScanLines {
		t.Fatalf("expected maxLines %d, got %#v", commandOffsetScanLines, source.offsetMaxLines)
	}
}

func TestSessionControlsRejectInvalidRequests(t *testing.T) {
	handler := newTestHandler()
	created := createSession(t, handler)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/pause", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d", http.StatusConflict, rec.Code)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/steering", strings.NewReader(`{"text":"later"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d", http.StatusConflict, rec.Code)
	}

	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/pause", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d", http.StatusConflict, rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/steering", strings.NewReader(`{"text":"   "}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/steering", strings.NewReader(`{"text":"later"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d", http.StatusConflict, rec.Code)
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 0 {
		t.Fatalf("expected offline steering to leave no display item, got %#v", items.Items)
	}
}

func TestTurnScopedControlsRejectStoredOfflineSession(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	putStoredSession(t, store, "sess-stored", t.TempDir())
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/sess-stored/pause", nil))
	assertAPIError(t, rec, http.StatusConflict, "session_not_live", "pause requires a live session")

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/sess-stored/steering", strings.NewReader(`{"text":"adjust"}`))
	handler.ServeHTTP(rec, req)
	assertAPIError(t, rec, http.StatusConflict, "no_active_turn", "steering requires an active turn")
}
