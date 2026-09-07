package zotigod

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/codexapp"
	"github.com/jayyao97/zotigo/internal/hooks"
)

type codexWorkerRPC struct {
	methods        []string
	resumeApproval string
	turnApproval   string
	turnID         string
	err            error
	methodErrors   map[string]error
	activeTurnID   string
	responseID     string
	response       any
	responseCalls  int
}

func newCodexInputTestRuntime(t *testing.T, sessionID string, rpc *codexWorkerRPC) (*codexWorkerRuntime, *zotigosession.FileStore) {
	t.Helper()
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: sessionID, Agent: "codex", ConversationID: "thread-1", Model: "gpt-test", CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	return &codexWorkerRuntime{
		cfg:   codexWorkerConfig{workerClientConfig: workerClientConfig{SessionID: sessionID}, SessionStoreRoot: store.RootDir()},
		store: store, app: rpc, threadID: "thread-1", messages: make(map[string]string), toolNames: make(map[string]string),
		toolArguments: make(map[string]string), toolNativeNames: make(map[string]string),
	}, store
}

func TestCodexAcceptInputUsesTurnStartForAtomicStartOrSteer(t *testing.T) {
	rpc := &codexWorkerRPC{turnID: "turn-active"}
	runtime, store := newCodexInputTestRuntime(t, "session-input-steer", rpc)
	runtime.activeTurnID = "turn-active"
	request := workerInputRequest{Command: commandResponse{
		ID: "client-1", Type: sessionCommandMessage, Message: &messageCommandPayload{Text: "change direction"},
	}}
	result := runtime.AcceptInput(context.Background(), request, nil, nil)
	if result.Error != "" || result.Command == nil || result.Command.Type != sessionCommandSteering || result.Command.Steering.TurnID != "turn-active" {
		t.Fatalf("input result = %#v", result)
	}
	result = runtime.AcceptInput(context.Background(), request, nil, nil)
	if result.Error != "" || result.Command == nil || result.Command.ID != "client-1" {
		t.Fatalf("idempotent result = %#v", result)
	}
	if got := fmt.Sprint(rpc.methods); got != "[turn/start]" {
		t.Fatalf("RPC methods = %s", got)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-input-steer")
	if err != nil || len(items) != 2 || items[0].Command == nil || items[0].Command.Type != sessionCommandSteering || items[1].Type != zotigosession.DisplayItemSteeringMessage {
		t.Fatalf("accepted steering items = %#v, err=%v", items, err)
	}
}

func TestCodexAcceptInputStartsMessageAfterConcurrentCompletion(t *testing.T) {
	rpc := &codexWorkerRPC{turnID: "turn-new"}
	runtime, store := newCodexInputTestRuntime(t, "session-input-start", rpc)
	runtime.activeTurnID = "turn-old"
	runtime.turnStarted = time.Now().UTC()
	notifications := make(chan codexapp.Message, 1)
	notifications <- codexapp.Message{Method: "turn/completed", Params: []byte(`{"turn":{"id":"turn-old","status":"completed"}}`)}
	result := runtime.AcceptInput(context.Background(), workerInputRequest{Command: commandResponse{
		ID: "client-2", Type: sessionCommandMessage, Message: &messageCommandPayload{Text: "next work"},
	}}, nil, notifications)
	if result.Error != "" || result.Command == nil || result.Command.Type != sessionCommandMessage || runtime.activeTurnID != "turn-new" {
		t.Fatalf("input result = %#v, active=%q", result, runtime.activeTurnID)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-input-start")
	if err != nil {
		t.Fatal(err)
	}
	messageCount := 0
	for _, item := range items {
		if item.ID == "client-2" && item.Command != nil && item.Command.Type == sessionCommandMessage {
			messageCount++
		}
	}
	if messageCount != 1 {
		t.Fatalf("message accepted %d times: %#v", messageCount, items)
	}
}

func TestCodexExplicitSteeringChecksExpectedTurnBeforeDelivery(t *testing.T) {
	rpc := &codexWorkerRPC{}
	runtime, store := newCodexInputTestRuntime(t, "session-input-mismatch", rpc)
	runtime.activeTurnID = "turn-current"
	result := runtime.AcceptInput(context.Background(), workerInputRequest{
		Command:      commandResponse{ID: "client-3", Type: sessionCommandSteering, Steering: &steeringCommandPayload{Text: "wrong"}},
		SteeringOnly: true, ExpectedTurnID: "turn-old",
	}, nil, nil)
	if result.ErrorCode != "turn_mismatch" || len(rpc.methods) != 0 {
		t.Fatalf("input result = %#v, methods=%#v", result, rpc.methods)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-input-mismatch")
	if err != nil || len(items) != 0 {
		t.Fatalf("mismatched input was persisted: %#v, err=%v", items, err)
	}
}

func TestCodexExplicitSteeringReportsCompletionRaceStructurally(t *testing.T) {
	rpc := &codexWorkerRPC{methodErrors: map[string]error{
		"turn/steer": &codexapp.RPCError{Code: -32600, Message: "invalid request"},
	}}
	runtime, store := newCodexInputTestRuntime(t, "session-input-race", rpc)
	runtime.activeTurnID = "turn-current"
	result := runtime.AcceptInput(context.Background(), workerInputRequest{
		Command:      commandResponse{ID: "client-4", Type: sessionCommandSteering, Steering: &steeringCommandPayload{Text: "too late"}},
		SteeringOnly: true, ExpectedTurnID: "turn-current",
	}, nil, nil)
	if result.ErrorCode != "no_active_turn" || fmt.Sprint(rpc.methods) != "[turn/steer thread/turns/list]" {
		t.Fatalf("input result = %#v, methods=%#v", result, rpc.methods)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-input-race")
	if err != nil || len(items) != 0 {
		t.Fatalf("failed steering was persisted: %#v, err=%v", items, err)
	}
}

func TestCodexWorkerCloseInterruptsActiveTurnInDisplayLog(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "session-1", WorkingDirectory: t.TempDir(), CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	rpc := &codexWorkerRPC{}
	runtime := &codexWorkerRuntime{
		cfg:   codexWorkerConfig{workerClientConfig: workerClientConfig{SessionID: "session-1"}},
		store: store, app: rpc, threadID: "thread-1", activeTurnID: "turn-1", turnStarted: now,
		messages: map[string]string{"message-1": "partial"}, messageOrder: []string{"message-1"},
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[1].Type != zotigosession.DisplayItemTurnInterrupted || items[1].Turn == nil || items[1].Turn.ID != "turn-1" {
		t.Fatalf("display items = %#v", items)
	}
	if got := fmt.Sprint(rpc.methods); got != "[turn/interrupt]" {
		t.Fatalf("methods = %s", got)
	}
}

func TestCodexWorkerCloseCompletesPendingToolAsInterrupted(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "session-pending", CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), "session-pending", zotigosession.DisplayItem{
		ID: "tool-1", Type: zotigosession.DisplayItemAssistantMessage, Role: string(protocol.RoleAssistant),
		Content: []zotigosession.DisplayContentPart{{Type: "tool_call", ToolCall: &zotigosession.DisplayToolCall{ID: "tool-1", Name: "shell"}}},
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &codexWorkerRuntime{
		cfg: codexWorkerConfig{workerClientConfig: workerClientConfig{SessionID: "session-pending"}}, store: store, app: &codexWorkerRPC{},
		threadID: "thread-1", activeTurnID: "turn-1", turnStarted: now, messages: make(map[string]string),
		toolNames: map[string]string{"tool-1": "shell"}, toolOrder: []string{"tool-1"},
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-pending")
	if err != nil || len(items) != 3 {
		t.Fatalf("display items = %#v, err=%v", items, err)
	}
	result := items[1].Content[0].ToolResult
	if result == nil || result.ToolCallID != "tool-1" || !result.IsError || result.Reason != "codex worker disconnected" {
		t.Fatalf("interrupted tool result = %#v", items[1])
	}
	if items[2].Type != zotigosession.DisplayItemTurnInterrupted {
		t.Fatalf("terminal item = %#v", items[2])
	}
}

func TestCodexWorkerPersistsContextUsageNotification(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{ID: "session-usage", CreatedAt: now, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	runtime := &codexWorkerRuntime{cfg: codexWorkerConfig{workerClientConfig: workerClientConfig{SessionID: "session-usage"}}, store: store, threadID: "thread-1"}
	if err := runtime.handleNotification(context.Background(), codexapp.Message{
		Method: "thread/tokenUsage/updated",
		Params: []byte(`{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"total":{"totalTokens":8000,"inputTokens":6500,"cachedInputTokens":4000,"cacheWriteInputTokens":500,"outputTokens":1500},"last":{"totalTokens":2400,"inputTokens":2200,"outputTokens":200},"modelContextWindow":128000}}`),
	}); err != nil {
		t.Fatal(err)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-usage")
	if err != nil || len(items) != 1 || items[0].ContextUsage == nil || items[0].ContextUsage.Tokens != 2200 || items[0].ContextUsage.Window != 128000 || items[0].ContextUsage.Source != "provider" {
		t.Fatalf("context usage items = %#v, err=%v", items, err)
	}
	if got, want := runtime.totalUsage, (protocol.Usage{
		InputTokens: 2000, OutputTokens: 1500, TotalTokens: 8000, CacheCreationInputTokens: 500, CacheReadInputTokens: 4000,
	}); got != want {
		t.Fatalf("total usage = %#v, want %#v", got, want)
	}
}

func TestCodexWorkerMissingUsageFieldsDoNotReplaceLastValidSnapshot(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{ID: "session-usage-stable", CreatedAt: now, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	runtime := &codexWorkerRuntime{cfg: codexWorkerConfig{workerClientConfig: workerClientConfig{SessionID: "session-usage-stable"}}, store: store, threadID: "thread-1"}
	for _, params := range []string{
		`{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"total":{"totalTokens":1000},"last":{"totalTokens":900,"inputTokens":800,"outputTokens":100},"modelContextWindow":512000}}`,
		`{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"total":{"totalTokens":1000},"last":{"totalTokens":200},"modelContextWindow":null}}`,
	} {
		if err := runtime.handleNotification(context.Background(), codexapp.Message{Method: "thread/tokenUsage/updated", Params: []byte(params)}); err != nil {
			t.Fatal(err)
		}
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-usage-stable")
	if err != nil || len(items) != 1 || items[0].ContextUsage == nil || items[0].ContextUsage.Tokens != 800 {
		t.Fatalf("last valid context snapshot was replaced: items=%#v err=%v", items, err)
	}
}

func TestCodexWorkerPersistsGeneratedImageAcrossReload(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{ID: "session-image", CreatedAt: now, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	runtime := &codexWorkerRuntime{
		cfg:   codexWorkerConfig{workerClientConfig: workerClientConfig{SessionID: "session-image"}, SessionStoreRoot: root},
		store: store, threadID: "thread-1", activeTurnID: "turn-1",
	}
	generatedPath := filepath.Join(t.TempDir(), "codex-generated.png")
	generatedBytes, _ := base64.StdEncoding.DecodeString(tinyPNGBase64())
	if err := os.WriteFile(generatedPath, generatedBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	params := fmt.Sprintf(`{"threadId":"thread-1","turnId":"turn-1","item":{"id":"image-1","type":"imageGeneration","status":"completed","savedPath":%q,"revisedPrompt":"a blue circle"}}`, generatedPath)
	if err := runtime.handleNotification(context.Background(), codexapp.Message{Method: "item/completed", Params: []byte(params)}); err != nil {
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
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: reopened}, handlerOptions{store: reopened})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, image.URL, nil))
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "image/png" || recorder.Body.String() != string(want) {
		t.Fatalf("reloaded image endpoint: status=%d type=%q bytes=%d", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.Len())
	}
}

func TestCodexWorkerExternalizesDynamicToolImageContent(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{ID: "session-tool-image", CreatedAt: now, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	runtime := &codexWorkerRuntime{
		cfg:   codexWorkerConfig{workerClientConfig: workerClientConfig{SessionID: "session-tool-image"}, SessionStoreRoot: root},
		store: store, threadID: "thread-1", activeTurnID: "turn-1", toolNames: map[string]string{"tool-1": "image_tool"},
	}
	dataURL := "data:image/png;base64," + tinyPNGBase64()
	params := fmt.Sprintf(`{"threadId":"thread-1","turnId":"turn-1","item":{"id":"tool-1","type":"dynamicToolCall","tool":"image_tool","status":"completed","success":true,"contentItems":[{"type":"inputImage","imageUrl":%q}]}}`, dataURL)
	if err := runtime.handleNotification(context.Background(), codexapp.Message{Method: "item/completed", Params: []byte(params)}); err != nil {
		t.Fatal(err)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-tool-image")
	if err != nil || len(items) != 1 {
		t.Fatalf("tool image items=%#v err=%v", items, err)
	}
	result := items[0].Content[0].ToolResult
	if result == nil || len(result.Content) != 1 || result.Content[0].Image == nil || strings.HasPrefix(result.Content[0].Image.URL, "data:") {
		t.Fatalf("tool image was not stored as structured media: %#v", result)
	}
	encoded, err := sonic.Marshal(items[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), tinyPNGBase64()) || strings.Contains(string(encoded), "data:image") {
		t.Fatalf("tool image payload remained inline in WAL item: %s", encoded)
	}
}

func TestCodexWorkerExternalizesMCPImageContent(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{ID: "session-mcp-image", CreatedAt: now, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	runtime := &codexWorkerRuntime{
		cfg:   codexWorkerConfig{workerClientConfig: workerClientConfig{SessionID: "session-mcp-image"}, SessionStoreRoot: root},
		store: store, threadID: "thread-1", activeTurnID: "turn-1", toolNames: map[string]string{"mcp-1": "image_tool"},
	}
	params := fmt.Sprintf(`{"threadId":"thread-1","turnId":"turn-1","item":{"id":"mcp-1","type":"mcpToolCall","tool":"image_tool","status":"completed","result":{"content":[{"type":"image","data":%q,"mimeType":"image/png"}],"isError":false}}}`, tinyPNGBase64())
	if err := runtime.handleNotification(context.Background(), codexapp.Message{Method: "item/completed", Params: []byte(params)}); err != nil {
		t.Fatal(err)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-mcp-image")
	if err != nil || len(items) != 1 {
		t.Fatalf("MCP image items=%#v err=%v", items, err)
	}
	result := items[0].Content[0].ToolResult
	if result == nil || len(result.Content) != 1 || result.Content[0].Image == nil || result.Content[0].Image.MediaType != "image/png" {
		t.Fatalf("MCP image was not stored as structured media: %#v", result)
	}
	encoded, err := sonic.Marshal(items[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), tinyPNGBase64()) {
		t.Fatalf("MCP image payload remained inline in WAL item: %s", encoded)
	}
}

func TestCodexImagePayloadsRejectOversizedBase64(t *testing.T) {
	oversized := strings.Repeat("A", base64.StdEncoding.EncodedLen(maxMessageTotalImageBytes+1))
	if _, err := codexGeneratedImageBytes(codexThreadItem{ID: "large-image", Result: oversized}); err == nil {
		t.Fatal("imageGeneration accepted oversized encoded payload")
	}
	content, err := codexMCPToolResultContent(map[string]any{"content": []any{
		map[string]any{"type": "text", "text": "valid text"},
		map[string]any{"type": "image", "data": oversized, "mimeType": "image/png"},
	}})
	if err == nil {
		t.Fatal("MCP image accepted oversized encoded payload")
	}
	if len(content) != 2 || content[0].Text != "valid text" || content[1].Text != "[image content is unavailable]" {
		t.Fatalf("MCP valid content was not preserved: %#v", content)
	}
}

func TestCodexWorkerContinuesPastUnavailableDynamicToolImage(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{ID: "session-bad-tool-image", CreatedAt: now, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	runtime := &codexWorkerRuntime{
		cfg:   codexWorkerConfig{workerClientConfig: workerClientConfig{SessionID: "session-bad-tool-image"}, SessionStoreRoot: root},
		store: store, threadID: "thread-1", activeTurnID: "turn-1", toolNames: map[string]string{"tool-bad-image": "image_tool"},
	}
	badTool := `{"threadId":"thread-1","turnId":"turn-1","item":{"id":"tool-bad-image","type":"dynamicToolCall","tool":"image_tool","status":"completed","success":true,"contentItems":[{"type":"inputImage","imageUrl":"data:image/png;base64,not-valid!"}]}}`
	if err := runtime.handleNotification(context.Background(), codexapp.Message{Method: "item/completed", Params: []byte(badTool)}); err != nil {
		t.Fatal(err)
	}
	later := `{"threadId":"thread-1","turnId":"turn-1","item":{"id":"after-bad-image","type":"agentMessage","text":"still running"}}`
	if err := runtime.handleNotification(context.Background(), codexapp.Message{Method: "item/completed", Params: []byte(later)}); err != nil {
		t.Fatal(err)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-bad-tool-image")
	if err != nil || len(items) != 2 {
		t.Fatalf("items after invalid tool media = %#v, err=%v", items, err)
	}
	result := items[0].Content[0].ToolResult
	if result == nil || len(result.Content) != 1 || result.Content[0].Text != "[image content is unavailable]" || items[1].ID != "after-bad-image" {
		t.Fatalf("invalid tool media degradation = %#v", items)
	}
}

func TestCodexWorkerContinuesPastUnavailableGeneratedImage(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{ID: "session-image-missing", CreatedAt: now, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	runtime := &codexWorkerRuntime{
		cfg:   codexWorkerConfig{workerClientConfig: workerClientConfig{SessionID: "session-image-missing"}},
		store: store, threadID: "thread-1", activeTurnID: "turn-1",
	}
	missingPath := filepath.Join(t.TempDir(), "missing.png")
	params := fmt.Sprintf(`{"threadId":"thread-1","turnId":"turn-1","item":{"id":"image-missing","type":"imageGeneration","status":"completed","savedPath":%q}}`, missingPath)
	if err := runtime.handleNotification(context.Background(), codexapp.Message{Method: "item/completed", Params: []byte(params)}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.handleNotification(context.Background(), codexapp.Message{Method: "item/completed", Params: []byte(`{"threadId":"thread-1","turnId":"turn-1","item":{"id":"image-no-source","type":"imageGeneration","status":"completed"}}`)}); err != nil {
		t.Fatal(err)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-image-missing")
	if err != nil || len(items) != 2 || items[0].Type != zotigosession.DisplayItemError || items[1].Type != zotigosession.DisplayItemError || runtime.activeTurnID != "turn-1" {
		t.Fatalf("runtime after unavailable image: items=%#v turn=%q err=%v", items, runtime.activeTurnID, err)
	}
}

func TestCodexWorkerDoesNotAttributeUsageFromAnotherTurn(t *testing.T) {
	runtime := &codexWorkerRuntime{threadID: "thread-1", activeTurnID: "turn-active"}
	if err := runtime.handleNotification(context.Background(), codexapp.Message{
		Method: "thread/tokenUsage/updated",
		Params: []byte(`{"threadId":"thread-1","turnId":"turn-stale","tokenUsage":{"total":{"totalTokens":20,"inputTokens":15,"outputTokens":5},"last":{"totalTokens":20,"inputTokens":15,"outputTokens":5}}}`),
	}); err != nil {
		t.Fatal(err)
	}
	if runtime.turnUsage != (protocol.Usage{}) {
		t.Fatalf("turn usage = %#v", runtime.turnUsage)
	}
	if runtime.totalUsage.TotalTokens != 20 || !runtime.hasTotalUsage {
		t.Fatalf("cumulative usage baseline = %#v, known=%v", runtime.totalUsage, runtime.hasTotalUsage)
	}
}

func TestCodexWorkerDispatchesPromptHooksForInitialMessageAndSteering(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "session-prompts", Model: "gpt-5.6-luna", CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	hookDispatcher := &capturingHookDispatcher{}
	runtime := &codexWorkerRuntime{
		cfg:   codexWorkerConfig{workerClientConfig: workerClientConfig{SessionID: "session-prompts"}, WorkingDirectory: "/workspace"},
		store: store, app: &codexWorkerRPC{}, threadID: "thread-1", messages: make(map[string]string), hooks: hookDispatcher,
	}
	if err := runtime.handleCommand(context.Background(), commandResponse{
		ID: "message-1", Type: sessionCommandMessage, Message: &messageCommandPayload{Text: "first"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := runtime.handleCommand(context.Background(), commandResponse{
		ID: "steering-1", Type: sessionCommandSteering, Steering: &steeringCommandPayload{Text: "correction"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if len(hookDispatcher.events) != 3 {
		t.Fatalf("hook events = %#v", hookDispatcher.events)
	}
	if hookDispatcher.events[0].EventName != hooks.TurnStart || hookDispatcher.events[0].TurnID != "turn-1" || hookDispatcher.events[0].Turn == nil || hookDispatcher.events[0].Turn.Model != "gpt-5.6-luna" {
		t.Fatalf("TurnStart event = %#v", hookDispatcher.events[0])
	}
	for index, text := range []string{"first", "correction"} {
		event := hookDispatcher.events[index+1]
		if event.EventName != hooks.UserPromptSubmit || event.TurnID != "turn-1" || event.Prompt == nil || event.Prompt.Text != text {
			t.Fatalf("prompt event %d = %#v", index, event)
		}
	}
}

func TestCodexWorkerPersistsAppliedSteering(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "session-steering", CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	rpc := &codexWorkerRPC{}
	runtime := &codexWorkerRuntime{
		cfg:   codexWorkerConfig{workerClientConfig: workerClientConfig{SessionID: "session-steering"}},
		store: store, app: rpc, threadID: "thread-1", activeTurnID: "turn-1",
	}
	command := commandResponse{
		ID: "steering-1", Type: sessionCommandSteering, CreatedAt: now,
		Steering: &steeringCommandPayload{Text: "use lite_downgrade", TurnID: "turn-1"},
	}
	if err := runtime.handleCommand(context.Background(), command, nil); err != nil {
		t.Fatal(err)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-steering")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("display items = %#v", items)
	}
	item := items[0]
	if item.ID != command.ID || item.Type != zotigosession.DisplayItemSteeringMessage || item.Role != string(protocol.RoleUser) {
		t.Fatalf("steering item = %#v", item)
	}
	if item.Turn == nil || item.Turn.ID != "turn-1" || len(item.Content) != 1 || item.Content[0].Text != "use lite_downgrade" {
		t.Fatalf("steering content = %#v", item)
	}
	if item.Command == nil || item.Command.Type != sessionCommandSteering || item.Command.Text != "use lite_downgrade" {
		t.Fatalf("steering command = %#v", item.Command)
	}
	if got := fmt.Sprint(rpc.methods); got != "[turn/steer]" {
		t.Fatalf("methods = %s", got)
	}
}

func TestCodexWorkerPersistsCompletedItemsInProtocolOrder(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "session-items", CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	writer := &workerClientWriter{sendCh: make(chan workerMessage, 16), done: make(chan struct{})}
	hookDispatcher := &capturingHookDispatcher{}
	runtime := &codexWorkerRuntime{
		cfg: codexWorkerConfig{workerClientConfig: workerClientConfig{SessionID: "session-items"}}, store: store, writer: writer,
		threadID: "thread-1", activeTurnID: "turn-1", turnStarted: now, messages: make(map[string]string), toolNames: make(map[string]string), hooks: hookDispatcher,
	}
	notifications := []codexapp.Message{
		{Method: "item/completed", Params: []byte(`{"threadId":"thread-1","turnId":"turn-1","completedAtMs":0,"item":{"type":"userMessage","id":"user-1","content":[{"type":"text","text":"Read AGENTS.md"}]}}`)},
		{Method: "item/agentMessage/delta", Params: []byte(`{"threadId":"thread-1","turnId":"turn-1","itemId":"message-1","delta":"Checking files."}`)},
		{Method: "item/completed", Params: []byte(`{"threadId":"thread-1","turnId":"turn-1","completedAtMs":1,"item":{"type":"agentMessage","id":"message-1","text":"Checking files."}}`)},
		{Method: "item/started", Params: []byte(`{"threadId":"thread-1","turnId":"turn-1","startedAtMs":2,"item":{"type":"commandExecution","id":"tool-1","command":"sed -n '1,20p' AGENTS.md","cwd":"/tmp/workspace","status":"inProgress","commandActions":[{"type":"read","command":"sed -n '1,20p' AGENTS.md","name":"AGENTS.md","path":"/tmp/workspace/AGENTS.md"}]}}`)},
		{Method: "item/commandExecution/outputDelta", Params: []byte(`{"threadId":"thread-1","turnId":"turn-1","itemId":"tool-1","delta":"# Instructions\n"}`)},
		{Method: "item/completed", Params: []byte(`{"threadId":"thread-1","turnId":"turn-1","completedAtMs":3,"item":{"type":"commandExecution","id":"tool-1","command":"sed -n '1,20p' AGENTS.md","cwd":"/tmp/workspace","status":"completed","commandActions":[{"type":"read","command":"sed -n '1,20p' AGENTS.md","name":"AGENTS.md","path":"/tmp/workspace/AGENTS.md"}],"aggregatedOutput":"# Instructions\n","exitCode":0,"durationMs":12}}`)},
		{Method: "item/completed", Params: []byte(`{"threadId":"thread-1","turnId":"turn-1","completedAtMs":4,"item":{"type":"contextCompaction","id":"compaction-1"}}`)},
		{Method: "item/completed", Params: []byte(`{"threadId":"thread-1","turnId":"turn-1","completedAtMs":5,"item":{"type":"reasoning","id":"reasoning-1","summary":["The instructions apply."],"content":[]}}`)},
		{Method: "thread/tokenUsage/updated", Params: []byte(`{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"total":{"totalTokens":30,"inputTokens":24,"cachedInputTokens":10,"cacheWriteInputTokens":4,"outputTokens":6},"last":{"totalTokens":30,"inputTokens":24,"cachedInputTokens":10,"cacheWriteInputTokens":4,"outputTokens":6},"modelContextWindow":128000}}`)},
		{Method: "turn/completed", Params: []byte(`{"turn":{"id":"turn-1","status":"completed","durationMs":20}}`)},
	}
	for _, notification := range notifications {
		if err := runtime.handleNotification(context.Background(), notification); err != nil {
			t.Fatalf("handle %s: %v", notification.Method, err)
		}
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-items")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 7 {
		t.Fatalf("display items = %#v", items)
	}
	if items[0].ID != "message-1" || items[0].Content[0].Text != "Checking files." {
		t.Fatalf("message item = %#v", items[0])
	}
	call := items[1].Content[0].ToolCall
	if call == nil || call.ID != "tool-1" || call.Name != "read_file" || !strings.Contains(call.Arguments, `"path":"/tmp/workspace/AGENTS.md"`) {
		t.Fatalf("tool call = %#v", items[1])
	}
	result := items[2].Content[0].ToolResult
	if result == nil || result.ToolCallID != "tool-1" || result.ToolName != "read_file" || result.Text != "# Instructions\n" || result.IsError {
		t.Fatalf("tool result = %#v", items[2])
	}
	if items[3].ID != "compaction-1" || items[3].Type != zotigosession.DisplayItemContextCompacted || items[3].Turn == nil || items[3].Turn.ID != "turn-1" {
		t.Fatalf("compaction item = %#v", items[3])
	}
	if items[4].Content[0].Type != string(protocol.ContentTypeReasoning) || items[4].Content[0].Text != "The instructions apply." {
		t.Fatalf("reasoning item = %#v", items[4])
	}
	if items[6].Type != zotigosession.DisplayItemTurnCompleted {
		t.Fatalf("turn item = %#v", items[6])
	}
	if len(hookDispatcher.events) != 2 {
		t.Fatalf("hook events = %#v", hookDispatcher.events)
	}
	hookEvent := hookDispatcher.events[0]
	if hookEvent.EventName != hooks.PostToolUse || hookEvent.Tool == nil || hookEvent.Tool.Name != "read_file" || hookEvent.Tool.NativeName != "commandExecution" || hookEvent.Tool.Result == nil || hookEvent.Tool.Result.Status != "succeeded" {
		t.Fatalf("PostToolUse event = %#v", hookEvent)
	}
	turnEnd := hookDispatcher.events[1]
	if turnEnd.EventName != hooks.TurnEnd || turnEnd.Turn == nil || turnEnd.Turn.Status != "completed" || turnEnd.Turn.Usage == nil ||
		turnEnd.Turn.Usage.InputTokens != 10 || turnEnd.Turn.Usage.OutputTokens != 6 || turnEnd.Turn.Usage.TotalTokens != 30 ||
		turnEnd.Turn.Usage.CacheCreationInputTokens != 4 || turnEnd.Turn.Usage.CacheReadInputTokens != 10 {
		t.Fatalf("TurnEnd event = %#v", turnEnd)
	}
	firstDelta := nextWorkerDelta(t, writer.sendCh)
	secondDelta := nextWorkerDelta(t, writer.sendCh)
	if firstDelta.Delta == nil || firstDelta.Delta.PartType != string(protocol.ContentTypeText) {
		t.Fatalf("first delta = %#v", firstDelta)
	}
	if secondDelta.Delta == nil || secondDelta.Delta.PartType != "tool_progress" || secondDelta.Delta.ToolCallID != "tool-1" || secondDelta.Delta.ToolName != "read_file" {
		t.Fatalf("second delta = %#v", secondDelta)
	}
}

func nextWorkerDelta(t *testing.T, messages <-chan workerMessage) workerMessage {
	t.Helper()
	for {
		select {
		case message := <-messages:
			if message.Delta != nil {
				return message
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for worker delta")
		}
	}
}

func TestCodexToolAdaptersCoverFileAndExternalTools(t *testing.T) {
	tests := []struct {
		name       string
		item       codexThreadItem
		toolName   string
		isError    bool
		hookStatus string
	}{
		{name: "file change", item: codexThreadItem{ID: "file-1", Type: "fileChange", Status: "completed", Changes: []any{map[string]any{"path": "a.go"}}}, toolName: "apply_patch"},
		{name: "file change declined", item: codexThreadItem{ID: "file-declined", Type: "fileChange", Status: "declined"}, toolName: "apply_patch", isError: true, hookStatus: "denied"},
		{name: "command failed without exit", item: codexThreadItem{ID: "command-1", Type: "commandExecution", Status: "failed"}, toolName: "shell", isError: true},
		{name: "command declined", item: codexThreadItem{ID: "command-2", Type: "commandExecution", Status: "declined"}, toolName: "shell", isError: true, hookStatus: "denied"},
		{name: "mcp", item: codexThreadItem{ID: "mcp-1", Type: "mcpToolCall", Tool: "search", Status: "completed", Arguments: map[string]any{"query": "zotigo"}, Result: map[string]any{"content": []any{"ok"}}}, toolName: "search"},
		{name: "mcp declined", item: codexThreadItem{ID: "mcp-declined", Type: "mcpToolCall", Tool: "search", Status: "declined"}, toolName: "search", isError: true, hookStatus: "denied"},
		{name: "mcp failed with result", item: codexThreadItem{ID: "mcp-failed", Type: "mcpToolCall", Tool: "search", Status: "failed", Result: map[string]any{"content": []any{"error"}}}, toolName: "search", isError: true},
		{name: "mcp error", item: codexThreadItem{ID: "mcp-2", Type: "mcpToolCall", Tool: "fetch", Error: &struct {
			Message string `json:"message"`
		}{Message: "unavailable"}}, toolName: "fetch", isError: true},
		{name: "dynamic", item: codexThreadItem{ID: "dynamic-1", Type: "dynamicToolCall", Tool: "lookup", Arguments: map[string]any{"id": 1}, Success: boolPointer(false)}, toolName: "lookup", isError: true},
		{name: "dynamic declined", item: codexThreadItem{ID: "dynamic-declined", Type: "dynamicToolCall", Tool: "lookup", Status: "declined"}, toolName: "lookup", isError: true, hookStatus: "denied"},
		{name: "dynamic failed without success", item: codexThreadItem{ID: "dynamic-2", Type: "dynamicToolCall", Tool: "lookup", Status: "failed"}, toolName: "lookup", isError: true},
		{name: "spawn subagent thread", item: codexThreadItem{ID: "spawn-1", Type: "collabAgentToolCall", Tool: "spawnAgent", Status: "completed", Prompt: stringPointer("review code"), ReceiverThreadIDs: []string{"thread-child"}, AgentsStates: map[string]codexAgentState{"thread-child": {Status: "running"}}}, toolName: "spawn_agent"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			name, _, ok, err := codexToolCall(test.item)
			if err != nil || !ok || name != test.toolName {
				t.Fatalf("tool call = name %q, ok %v, err %v", name, ok, err)
			}
			result, ok, err := codexToolResult(test.item, name)
			if err != nil {
				t.Fatal(err)
			}
			if !ok || result.ToolName != test.toolName || result.IsError != test.isError {
				t.Fatalf("tool result = %#v, ok=%v", result, ok)
			}
			wantStatus := test.hookStatus
			if wantStatus == "" {
				wantStatus = "succeeded"
				if test.isError {
					wantStatus = "failed"
				}
			}
			if status := codexToolResultStatus(test.item.Status, result); status != wantStatus {
				t.Fatalf("hook status = %q, want %q", status, wantStatus)
			}
		})
	}
	spawn := codexThreadItem{
		ID: "spawn-details", Type: "collabAgentToolCall", Tool: "spawnAgent", Status: "completed",
		Prompt: stringPointer("review code"), ReceiverThreadIDs: []string{"thread-child"},
		AgentsStates: map[string]codexAgentState{"thread-child": {Status: "completed"}},
	}
	_, arguments, _, err := codexToolCall(spawn)
	if err != nil || !strings.Contains(arguments, `"description":"review code"`) {
		t.Fatalf("spawn arguments = %q, err=%v", arguments, err)
	}
	result, ok, err := codexToolResult(spawn, "spawn_agent")
	if err != nil {
		t.Fatal(err)
	}
	resultJSON, isMap := result.JSON.(map[string]any)
	if !ok || !isMap || resultJSON["agents_states"] == nil || result.Metadata != nil {
		t.Fatalf("spawn result = %#v, ok=%v", result, ok)
	}
}

func TestCodexFileChangePreservesStructuredChanges(t *testing.T) {
	item := codexThreadItem{
		ID: "file-1", Type: "fileChange",
		Changes: []any{
			map[string]any{"path": "a.go", "kind": map[string]any{"type": "update"}, "diff": "*** Begin Patch\n*** Update File: a.go\n@@\n-old\n+new\n*** End Patch"},
			map[string]any{"path": "b.go", "kind": map[string]any{"type": "add"}, "diff": "*** Begin Patch\n*** Add File: b.go\n+package b\n*** End Patch"},
		},
	}
	name, arguments, ok, err := codexToolCall(item)
	if err != nil || !ok || name != "apply_patch" {
		t.Fatalf("tool call = %q %q ok=%v err=%v", name, arguments, ok, err)
	}
	var input map[string]any
	if err := sonic.UnmarshalString(arguments, &input); err != nil {
		t.Fatal(err)
	}
	changes, exists := input["changes"].([]any)
	if !exists || len(changes) != 2 {
		t.Fatalf("structured file changes missing from arguments: %#v", input)
	}
	result, ok, err := codexToolResult(item, name)
	if err != nil || !ok {
		t.Fatalf("file change result ok=%v err=%v", ok, err)
	}
	resultJSON, ok := result.JSON.(map[string]any)
	if !ok || resultJSON["changes"] == nil {
		t.Fatalf("structured file changes missing from tool result: %#v", result)
	}
}

func boolPointer(value bool) *bool { return &value }

func stringPointer(value string) *string { return &value }

func (r *codexWorkerRPC) Call(_ context.Context, method string, params any, result any) error {
	r.methods = append(r.methods, method)
	if err := r.methodErrors[method]; err != nil {
		return err
	}
	request := params.(map[string]any)
	if method == "thread/resume" {
		r.resumeApproval, _ = request["approvalPolicy"].(string)
	}
	if method == "turn/start" && r.err == nil {
		r.turnApproval, _ = request["approvalPolicy"].(string)
		turnID := r.turnID
		if turnID == "" {
			turnID = "turn-1"
		}
		payload, err := sonic.Marshal(map[string]any{"turn": map[string]any{"id": turnID}})
		if err != nil {
			return err
		}
		if err := sonic.Unmarshal(payload, result); err != nil {
			return err
		}
	}
	if method == "thread/turns/list" {
		turns := make([]codexTurn, 0, 1)
		if r.activeTurnID != "" {
			turns = append(turns, codexTurn{ID: r.activeTurnID, Status: "inProgress"})
		}
		payload, err := sonic.Marshal(codexTurnList{Data: turns})
		if err != nil {
			return err
		}
		if err := sonic.Unmarshal(payload, result); err != nil {
			return err
		}
	}
	return r.err
}

func TestCodexPauseThenMessageWaitsForCompletedTurnBeforeAcknowledging(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "session-pause-message", Agent: "codex", ConversationID: "thread-1", Model: "gpt-test", CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	rpc := &codexWorkerRPC{turnID: "turn-new"}
	runtime := &codexWorkerRuntime{
		cfg:   codexWorkerConfig{workerClientConfig: workerClientConfig{SessionID: "session-pause-message"}},
		store: store, app: rpc, threadID: "thread-1", activeTurnID: "turn-old", turnStarted: now,
		messages: make(map[string]string), toolNames: make(map[string]string), toolArguments: make(map[string]string),
		toolNativeNames: make(map[string]string),
	}
	pause := commandResponse{ID: "pause", Sequence: 2, Type: sessionCommandPause, Pause: &pauseCommandPayload{TurnID: "turn-old"}}
	if err := runtime.handleCommand(context.Background(), pause, nil); err != nil {
		t.Fatal(err)
	}
	message := commandResponse{ID: "message", Sequence: 3, Type: sessionCommandMessage, Message: &messageCommandPayload{Text: "continue"}}
	if runtime.canApplyCommand(message) || runtime.commandSequence != pause.Sequence {
		t.Fatalf("message applied before turn barrier: runtime=%#v", runtime)
	}
	runtime.messagePending = true
	if err := runtime.handleNotification(context.Background(), codexapp.Message{
		Method: "turn/completed",
		Params: []byte(`{"turn":{"id":"turn-old","status":"interrupted","durationMs":10}}`),
	}); err != nil {
		t.Fatal(err)
	}
	if !runtime.canApplyCommand(message) || runtime.commandSequence != pause.Sequence {
		t.Fatalf("turn barrier advanced pending message cursor: runtime=%#v", runtime)
	}
	if err := runtime.handleCommand(context.Background(), message, nil); err != nil {
		t.Fatal(err)
	}
	if runtime.activeTurnID != "turn-new" || runtime.commandSequence != message.Sequence {
		t.Fatalf("pending message did not start after barrier: runtime=%#v", runtime)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-pause-message")
	if err != nil || len(items) != 2 || items[0].Type != zotigosession.DisplayItemTurnInterrupted || items[1].Type != zotigosession.DisplayItemTurnStarted {
		t.Fatalf("display lifecycle = %#v, err=%v", items, err)
	}
}

func TestCodexRestartReplaysPauseAndMessageAcrossOpenTurn(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "session-replay", Agent: "codex", ConversationID: "thread-1", Model: "gpt-test", CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	for _, item := range []zotigosession.DisplayItem{
		{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-old"}, CreatedAt: now},
		{ID: "pause", Type: zotigosession.DisplayItemSessionCommand, Command: &zotigosession.DisplayCommand{Type: sessionCommandPause, TurnID: "turn-old"}},
		{ID: "message", Type: zotigosession.DisplayItemUserMessage, Command: &zotigosession.DisplayCommand{Type: sessionCommandMessage, Text: "replay me"}},
	} {
		if _, err := store.AppendDisplayItem(context.Background(), "session-replay", item); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = sonic.ConfigDefault.NewEncoder(w).Encode(commandsResponse{Commands: []commandResponse{
			{ID: "pause", Sequence: 2, Type: sessionCommandPause, Pause: &pauseCommandPayload{TurnID: "turn-old"}},
			{ID: "message", Sequence: 3, Type: sessionCommandMessage, Message: &messageCommandPayload{Text: "replay me"}},
		}})
	}))
	defer server.Close()
	cursor, err := recoverWorkerCommandCursor(context.Background(), store, "session-replay")
	if err != nil || cursor.Sequence != 0 {
		t.Fatalf("recovered cursor = %#v, err=%v", cursor, err)
	}
	pending, err := loadCodexPendingCommands(context.Background(), server.Client(), server.URL, "session-replay", cursor.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].ID != "pause" || pending[1].ID != "message" {
		t.Fatalf("pending commands = %#v", pending)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-replay")
	if err != nil {
		t.Fatal(err)
	}
	rpc := &codexWorkerRPC{turnID: "turn-new"}
	runtime := &codexWorkerRuntime{
		cfg:   codexWorkerConfig{workerClientConfig: workerClientConfig{SessionID: "session-replay"}},
		store: store, app: rpc, threadID: "thread-1", messages: make(map[string]string),
		toolNames: make(map[string]string), toolArguments: make(map[string]string), toolNativeNames: make(map[string]string),
	}
	runtime.activeTurnID, runtime.turnStarted = lastOpenTurn(items)
	if runtime.activeTurnID != "turn-old" {
		t.Fatalf("open turn was not restored: %#v", runtime)
	}
	if err := runtime.handleCommand(context.Background(), pending[0], nil); err != nil {
		t.Fatal(err)
	}
	if runtime.canApplyCommand(pending[1]) {
		t.Fatal("replayed message bypassed open turn barrier")
	}
	runtime.messagePending = true
	if err := runtime.handleNotification(context.Background(), codexapp.Message{
		Method: "turn/completed", Params: []byte(`{"turn":{"id":"turn-old","status":"interrupted"}}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.handleCommand(context.Background(), pending[1], nil); err != nil {
		t.Fatal(err)
	}
	if runtime.commandSequence != 3 || runtime.activeTurnID != "turn-new" || fmt.Sprint(rpc.methods) != "[turn/interrupt turn/start]" {
		t.Fatalf("replayed runtime = %#v, methods=%v", runtime, rpc.methods)
	}
}

func TestResumeCodexThreadClassifiesAnotherAppOwnership(t *testing.T) {
	rpc := &codexWorkerRPC{err: &codexapp.RPCError{Code: -32600, Message: "thread thread-1 already has an active writer"}}
	err := resumeCodexThread(context.Background(), rpc, codexWorkerConfig{
		ThreadID: "thread-1", WorkingDirectory: t.TempDir(), Model: "gpt-5.6-luna",
	})
	if !errors.Is(err, errRuntimeOccupied) {
		t.Fatalf("resume error = %v, want runtime occupied", err)
	}
}

func (*codexWorkerRPC) Notify(string, any) error { return nil }

func (r *codexWorkerRPC) RespondResult(id json.RawMessage, result any) error {
	r.responseCalls++
	r.responseID = string(id)
	r.response = result
	return r.err
}

func TestResumeCodexThreadUsesCWDWithoutUpdatingProject(t *testing.T) {
	rpc := &codexWorkerRPC{}
	err := resumeCodexThread(context.Background(), rpc, codexWorkerConfig{
		ThreadID: "thread-1", WorkingDirectory: t.TempDir(), Model: "gpt-5.6-luna",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(rpc.methods); got != "[thread/resume]" {
		t.Fatalf("methods = %s", got)
	}
	if rpc.resumeApproval != "on-request" {
		t.Fatalf("resume approval policy = %q", rpc.resumeApproval)
	}
}

func TestCodexApprovalPolicyMapping(t *testing.T) {
	if got := codexApprovalPolicy(string(agent.ApprovalPolicyAuto)); got != "on-request" {
		t.Fatalf("auto policy = %q", got)
	}
	if got := codexApprovalPolicy(string(agent.ApprovalPolicyBypass)); got != "never" {
		t.Fatalf("bypass policy = %q", got)
	}
}

func TestCodexApprovalPolicyCommandPersistsAndCompletesBeforeNextTurn(t *testing.T) {
	rpc := &codexWorkerRPC{}
	runtime, store := newCodexInputTestRuntime(t, "session-codex-policy", rpc)
	stored, err := store.Get(context.Background(), "session-codex-policy")
	if err != nil {
		t.Fatal(err)
	}
	stored.ApprovalPolicy = agent.ApprovalPolicyBypass
	stored.Model = "gpt-test"
	if err := store.Put(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	runtime.cfg.ApprovalPolicy = string(agent.ApprovalPolicyBypass)
	commandItem, err := store.AppendDisplayItem(context.Background(), "session-codex-policy", zotigosession.DisplayItem{
		ID: "policy-command", Type: zotigosession.DisplayItemSessionCommand,
		Command: &zotigosession.DisplayCommand{Type: sessionCommandApprovalPolicy, ApprovalPolicy: string(agent.ApprovalPolicyAuto)},
	})
	if err != nil {
		t.Fatal(err)
	}
	command := commandResponse{
		ID: "policy-command", Sequence: commandItem.Sequence, Type: sessionCommandApprovalPolicy,
		ApprovalPolicy: &approvalPolicyCommandPayload{Policy: agent.ApprovalPolicyAuto},
	}
	if err := runtime.handleCommand(context.Background(), command, nil); err != nil {
		t.Fatal(err)
	}
	stored, err = store.Get(context.Background(), "session-codex-policy")
	if err != nil || stored.ApprovalPolicy != agent.ApprovalPolicyAuto || runtime.cfg.ApprovalPolicy != string(agent.ApprovalPolicyAuto) {
		t.Fatalf("persisted policy=%#v runtime=%q err=%v", stored, runtime.cfg.ApprovalPolicy, err)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-codex-policy")
	if err != nil || len(items) != 2 || items[1].ApprovalPolicy == nil || items[1].ApprovalPolicy.CommandID != "policy-command" || items[1].ID == "policy-command" {
		t.Fatalf("policy completion items=%#v err=%v", items, err)
	}
	cursor, err := recoverWorkerCommandCursor(context.Background(), store, "session-codex-policy")
	if err != nil || cursor.Sequence != commandItem.Sequence {
		t.Fatalf("recovered cursor=%#v err=%v, want %d", cursor, err, commandItem.Sequence)
	}
	_, _, err = runtime.callTurnStart(context.Background(), commandResponse{
		ID: "message", Message: &messageCommandPayload{Text: "continue"},
	}, nil)
	if err != nil || rpc.turnApproval != "on-request" {
		t.Fatalf("next turn approval=%q err=%v", rpc.turnApproval, err)
	}
}
