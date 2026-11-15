package zotigod

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/internal/testutil/sessionlogtest"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/codexapp"
	"github.com/jayyao97/zotigo/internal/zotigod/codexworker"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

func TestCodexWorkerPersistsGeneratedImageAcrossReload(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{ID: "session-image", CreatedAt: now, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	runtime := codexworker.NewRuntimeForTest(codexworker.RuntimeForTest{
		Config: codexworker.Config{Config: workerbase.Config{SessionID: "session-image"}, SessionStoreRoot: root},
		Store:  store, ThreadID: "thread-1", ActiveTurnID: "turn-1",
	})
	generatedPath := filepath.Join(t.TempDir(), "codex-generated.png")
	generatedBytes, _ := base64.StdEncoding.DecodeString(tinyPNGBase64())
	if err := os.WriteFile(generatedPath, generatedBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	params := fmt.Sprintf(`{"threadId":"thread-1","turnId":"turn-1","item":{"id":"image-1","type":"imageGeneration","status":"completed","savedPath":%q,"revisedPrompt":"a blue circle"}}`, generatedPath)
	if err := runtime.HandleNotification(context.Background(), codexapp.Message{Method: "item/completed", Params: []byte(params)}); err != nil {
		t.Fatal(err)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-image")
	if err != nil || len(items) != 1 || len(items[0].Content) != 1 || items[0].Content[0].Image == nil {
		t.Fatalf("generated image display item = %#v, err=%v", items, err)
	}
	image := items[0].Content[0].Image
	if image.MediaType != "image/png" || image.URL == "" || len(image.Data) != 0 {
		t.Fatalf("generated image was not externalized: %#v", image)
	}
	public := publicDisplayItem(items[0])
	if public.Content[0].Image == nil || public.Content[0].Image.URL != image.URL || public.Content[0].Image.MediaType != "image/png" {
		t.Fatalf("sessions/items projection lost generated image: %#v", public)
	}
	name := filepath.Base(image.URL)
	ref, ok, err := store.GetImageRef(context.Background(), "session-image", name)
	if err != nil || !ok {
		t.Fatalf("generated image reference missing: %#v, %v", ref, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replayed, _, err := reopened.ListDisplayItems(context.Background(), "session-image")
	if err != nil || len(replayed) != 1 || replayed[0].Content[0].Image.URL != image.URL {
		t.Fatalf("reloaded image display item = %#v, err=%v", replayed, err)
	}
	data, err := os.ReadFile(filepath.Join(root, ref.BlobPath))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := base64.StdEncoding.DecodeString(tinyPNGBase64())
	if string(data) != string(want) {
		t.Fatal("reloaded generated image bytes differ")
	}
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: reopened}, handlerOptions{store: reopened})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, image.URL, nil))
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "image/png" || recorder.Body.String() != string(want) {
		t.Fatalf("reloaded image endpoint: status=%d type=%q bytes=%d", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.Len())
	}
}

func TestCodexSteeringAnchorsOnlyAlreadyStartedOutput(t *testing.T) {
	t.Parallel()
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const sessionID = "codex-steering-order"
	sessionlogtest.PutStoredSession(t, store, sessionID, t.TempDir())
	writer := workerbase.NewUnstartedWriter(nil, 32)
	runtime := codexworker.NewRuntimeForTest(codexworker.RuntimeForTest{
		Config: codexworker.Config{Config: workerbase.Config{SessionID: sessionID}},
		Store:  store, ThreadID: "thread-1", ActiveTurnID: "turn-1", Writer: writer, Messages: make(map[string]string),
	})
	notify := func(method, item string) {
		t.Helper()
		if err := runtime.HandleNotification(t.Context(), codexapp.Message{
			Method: method, Params: []byte(`{"threadId":"thread-1","turnId":"turn-1","item":` + item + `}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	notify("item/started", `{"id":"thinking","type":"reasoning"}`)
	notify("item/started", `{"id":"answer","type":"agentMessage"}`)
	notify("item/started", `{"id":"shell","type":"commandExecution","command":"echo hello","cwd":"/tmp"}`)
	for _, id := range []string{"steer-1", "steer-2"} {
		if err := runtime.PersistSteering(wire.Command{
			ID: id, Type: wire.CommandSteering, Steering: &wire.SteeringCommandPayload{Text: id},
		}, "turn-1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := runtime.HandleNotification(t.Context(), codexapp.Message{
		Method: "item/agentMessage/delta", Params: []byte(`{"turnId":"turn-1","itemId":"answer","delta":"tail"}`),
	}); err != nil {
		t.Fatal(err)
	}
	boundaryUpdates, textUpdates := 0, 0
	for len(writer.Queue()) > 0 {
		message := <-writer.Queue()
		if message.Delta == nil {
			continue
		}
		if message.Delta.BeforeInputID != "steer-1" {
			t.Fatalf("streaming boundary missing: %+v", message.Delta)
		}
		if message.Delta.Delta == "" {
			boundaryUpdates++
		} else {
			textUpdates++
		}
	}
	if boundaryUpdates != 2 || textUpdates != 1 {
		t.Fatalf("position updates=%d text updates=%d", boundaryUpdates, textUpdates)
	}
	notify("item/completed", `{"id":"thinking","type":"reasoning","summary":["old thinking"]}`)
	notify("item/completed", `{"id":"answer","type":"agentMessage","text":"old answer"}`)
	notify("item/completed", `{"id":"shell","type":"commandExecution","command":"echo hello","status":"completed","aggregatedOutput":"hello","exitCode":0}`)
	notify("item/started", `{"id":"new-answer","type":"agentMessage"}`)
	notify("item/completed", `{"id":"new-answer","type":"agentMessage","text":"new answer"}`)
	items, _, err := store.ListDisplayItems(t.Context(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	marked := 0
	for _, item := range items {
		if item.ID == "thinking" || item.ID == "answer" || (len(item.Content) > 0 && item.Content[0].ToolResult != nil) {
			marked++
			if publicDisplayItem(item).BeforeInputID != "steer-1" {
				t.Fatalf("old output lost first steering boundary: %+v", item)
			}
		}
		if item.ID == "new-answer" && item.BeforeInputID != "" {
			t.Fatal("new output incorrectly moved before steering")
		}
	}
	if marked != 3 || runtime.ActiveDisplayItemCountForTest() != 0 {
		t.Fatalf("marked=%d active items=%+v", marked, runtime.ActiveDisplayItemCountForTest())
	}
}

func TestWorkerInputErrorMatchesActiveTurn(t *testing.T) {
	t.Parallel()
	err := &workerInputError{Code: "active_turn", Message: "a turn is already active"}
	if !errors.Is(err, sessionlog.ErrActiveTurn) {
		t.Fatalf("worker input error %v did not match sessionlog.ErrActiveTurn", err)
	}
}
