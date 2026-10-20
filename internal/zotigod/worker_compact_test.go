package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

func TestCompactingInputRejectionIsStructured(t *testing.T) {
	for _, endpoint := range []string{"messages", "steering"} {
		t.Run(endpoint, func(t *testing.T) {
			source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
			api := newHandler(newSessionRegistry(), source)
			server := httptest.NewServer(api)
			defer server.Close()
			created := createSession(t, api)
			startSession(t, api, created.ID)
			worker := dialWorker(t, server, created.ID)
			defer worker.Close()
			appendTurnStarted(t, source, created.ID, "compact")
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				rec := httptest.NewRecorder()
				api.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/"+endpoint, strings.NewReader(`{"text":"continue"}`)))
				done <- rec
			}()
			if err := worker.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			var input workerMessage
			if err := worker.ReadJSON(&input); err != nil {
				t.Fatal(err)
			}
			if input.InputRequest == nil {
				t.Fatal("missing input request")
			}
			code, message := workerInputErrorDetails(fmt.Errorf("%w: context compaction is in progress", errActiveTurn))
			if err := worker.WriteJSON(workerMessage{Type: workerMessageInputResult, InputResult: &workerInputResult{
				RequestID: input.InputRequest.RequestID, ErrorCode: code, Error: message,
			}}); err != nil {
				t.Fatal(err)
			}
			select {
			case rec := <-done:
				assertAPIError(t, rec, http.StatusConflict, "active_turn", "context compaction is in progress")
			case <-time.After(3 * time.Second):
				t.Fatal("rejection not returned")
			}
		})
	}
}

func TestNativeCompactCommandPersistsWithoutModelRequest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := &workerPromptCaptureProvider{}
	const providerName = "compact-command-test"
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) { return p, nil })
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(fmt.Sprintf("default_profile: test\nprofiles:\n  test:\n    provider: %s\n    model: test\n", providerName)), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const id = "compact-command"
	putStoredSession(t, store, id, workDir)
	ctx := context.Background()
	sess, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	sess.AgentSnapshot.State = agent.StateIdle
	for range 20 {
		sess.AgentSnapshot.History = append(sess.AgentSnapshot.History,
			protocol.NewUserMessage(strings.Repeat("old question detail ", 100)),
			protocol.NewAssistantMessage(strings.Repeat("old response detail ", 100)))
	}
	if err := store.Put(ctx, sess); err != nil {
		t.Fatal(err)
	}
	runtime, err := newWorkerRuntime(ctx, workerRuntimeConfig{SessionID: id, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	request := workerInputRequest{Command: commandResponse{
		ID: "compact-1", Type: sessionCommandMessage, Message: &messageCommandPayload{Text: "/compact"},
	}}
	if _, err := runtime.acceptInput(ctx, request); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	done := runtime.turnDone
	runtime.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("compact did not finish")
		}
	}
	// Duplicate delivery must not compact again.
	if _, err := runtime.acceptInput(ctx, request); err != nil {
		t.Fatal(err)
	}
	items, _, err := store.ListDisplayItems(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	compactions, completed := 0, 0
	for _, item := range items {
		if item.Type == zotigosession.DisplayItemContextCompacted {
			compactions++
		}
		if item.Type == zotigosession.DisplayItemTurnCompleted {
			completed++
		}
	}
	if compactions != 1 || completed != 1 {
		t.Fatalf("compact=%d completed=%d", compactions, completed)
	}
	saved, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.AgentSnapshot.History) >= len(sess.AgentSnapshot.History) {
		t.Fatal("compacted history was not saved")
	}
	if got := p.joinedPrompts(); got != "" {
		t.Fatalf("command sent to model: %s", got)
	}
	// Reject an active-turn compact before admitting it to the transcript.
	if _, err := runtime.display.StartTurn(ctx); err != nil {
		t.Fatal(err)
	}
	request.Command.ID = "compact-2"
	if _, err := runtime.acceptInput(ctx, request); err == nil || !strings.Contains(err.Error(), "before /compact") {
		t.Fatalf("active compact: %v", err)
	}
	request.Command = steeringCommandForRequest(request, runtime.display.CurrentTurnID())
	request.SteeringOnly = true
	if _, err := runtime.acceptInput(ctx, request); err == nil || !strings.Contains(err.Error(), "before /compact") {
		t.Fatalf("explicit steering compact: %v", err)
	}
	// Guard admission before touching the agent while its summary holds the lock.
	runtime.mu.Lock()
	runtime.turnCompacting = true
	runtime.mu.Unlock()
	request.Command = commandResponse{ID: "during-compact", Type: sessionCommandMessage, Message: &messageCommandPayload{Text: "please continue"}}
	request.RequiredApprovalPolicy = "auto"
	if _, err := runtime.acceptInput(ctx, request); !errors.Is(err, errActiveTurn) {
		t.Fatalf("input during compact: %v", err)
	}
	items, _, err = store.ListDisplayItems(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.ID == "during-compact" {
			t.Fatal("rejected input was admitted")
		}
	}
}

func TestCompactCommandOnlyMatchesBareLocalInput(t *testing.T) {
	for _, text := range []string{"/compact", " /compress\n"} {
		if !isCompactCommand(&messageCommandPayload{Text: text}) {
			t.Fatalf("not recognized: %q", text)
		}
	}
	for _, command := range []*messageCommandPayload{
		nil,
		{Text: "/compact this paragraph"},
		{Text: "/compact", Images: []commandImageData{{MimeType: "image/png"}}},
		{Text: "/compact", Skills: []string{"review"}},
		{Text: "/compact", RequestContext: &protocol.RequestContext{Source: "feishu"}},
	} {
		if isCompactCommand(command) {
			t.Fatalf("treated normal input as a command: %+v", command)
		}
	}
}
