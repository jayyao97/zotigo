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

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

func TestFirstCompletedTurnTitleSource(t *testing.T) {
	items := []zotigosession.DisplayItem{
		titleMessageItem(zotigosession.DisplayItemUserMessage, "帮我看看这个"),
		{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
		{Type: zotigosession.DisplayItemAssistantMessage, Content: []zotigosession.DisplayContentPart{
			{Type: string(protocol.ContentTypeReasoning), Text: "private"},
			{Type: string(protocol.ContentTypeToolCall), ToolCall: &zotigosession.DisplayToolCall{Name: "read_file"}},
		}},
		{Type: zotigosession.DisplayItemTurnPaused, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
		titleMessageItem(zotigosession.DisplayItemAssistantMessage, "先读配置"),
		titleMessageItem(zotigosession.DisplayItemAssistantMessage, "定位到了 Profile 大小写问题"),
		{Type: zotigosession.DisplayItemTurnCompleted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
		titleMessageItem(zotigosession.DisplayItemUserMessage, "later turn"),
	}

	userMessage, assistantMessage, err := firstCompletedTurnTitleSource(items)
	if err != nil {
		t.Fatalf("firstCompletedTurnTitleSource: %v", err)
	}
	if userMessage != "帮我看看这个" || assistantMessage != "定位到了 Profile 大小写问题" {
		t.Fatalf("source = (%q, %q)", userMessage, assistantMessage)
	}
}

func TestFirstCompletedTurnTitleSourceRejectsFailedOrIncompleteTurn(t *testing.T) {
	for _, terminal := range []zotigosession.DisplayItemType{
		zotigosession.DisplayItemTurnFailed,
		zotigosession.DisplayItemTurnInterrupted,
		zotigosession.DisplayItemTurnPaused,
	} {
		t.Run(string(terminal), func(t *testing.T) {
			items := []zotigosession.DisplayItem{
				titleMessageItem(zotigosession.DisplayItemUserMessage, "user"),
				{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
				{Type: terminal, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
			}
			_, _, err := firstCompletedTurnTitleSource(items)
			if !errors.Is(err, errTitleSourceNotReady) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestFirstCompletedTurnTitleSourceSkipsUnsuccessfulTurn(t *testing.T) {
	for _, terminal := range []zotigosession.DisplayItemType{
		zotigosession.DisplayItemTurnFailed,
		zotigosession.DisplayItemTurnInterrupted,
	} {
		t.Run(string(terminal), func(t *testing.T) {
			items := []zotigosession.DisplayItem{
				titleMessageItem(zotigosession.DisplayItemUserMessage, "first attempt"),
				{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
				titleMessageItem(zotigosession.DisplayItemAssistantMessage, "partial response"),
				{Type: terminal, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
				titleMessageItem(zotigosession.DisplayItemUserMessage, "retry attempt"),
				{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-2"}},
				titleMessageItem(zotigosession.DisplayItemAssistantMessage, "successful response"),
				{Type: zotigosession.DisplayItemTurnCompleted, Turn: &zotigosession.DisplayTurn{ID: "turn-2"}},
			}

			userMessage, assistantMessage, err := firstCompletedTurnTitleSource(items)
			if err != nil {
				t.Fatalf("firstCompletedTurnTitleSource: %v", err)
			}
			if userMessage != "retry attempt" || assistantMessage != "successful response" {
				t.Fatalf("source = (%q, %q)", userMessage, assistantMessage)
			}
		})
	}
}

func TestFirstCompletedTurnTitleSourceAllowsImageOnlyPrompt(t *testing.T) {
	items := []zotigosession.DisplayItem{
		{Type: zotigosession.DisplayItemUserMessage, Content: []zotigosession.DisplayContentPart{{Type: string(protocol.ContentTypeImage), Image: &zotigosession.DisplayMediaPart{MediaType: "image/png"}}}},
		{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
		titleMessageItem(zotigosession.DisplayItemAssistantMessage, "分析登录页面截图"),
		{Type: zotigosession.DisplayItemTurnCompleted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
	}
	userMessage, assistantMessage, err := firstCompletedTurnTitleSource(items)
	if err != nil || userMessage != "" || assistantMessage != "分析登录页面截图" {
		t.Fatalf("source = (%q, %q, %v)", userMessage, assistantMessage, err)
	}
}

func TestSessionTitleSuggestion(t *testing.T) {
	workDir := t.TempDir()
	writeTitleTestConfig(t, workDir)
	registry := newSessionRegistry()
	created := registry.Add(newSession(workDir, "title-test"))
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{
		created.ID: {
			titleMessageItem(zotigosession.DisplayItemUserMessage, "帮我看看这个"),
			{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
			titleMessageItem(zotigosession.DisplayItemAssistantMessage, "定位到了配置问题"),
			{Type: zotigosession.DisplayItemTurnCompleted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
		},
	}}
	var gotProfile config.ProfileConfig
	var gotUser, gotAssistant string
	handler := newHandler(registry, source, handlerOptions{
		titleSuggestion: func(_ context.Context, profile config.ProfileConfig, userMessage string, assistantMessage string) (string, error) {
			gotProfile = profile
			gotUser = userMessage
			gotAssistant = assistantMessage
			return "修复配置问题", nil
		},
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/title-suggestion", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Title string `json:"title"`
	}
	if err := decodeAPIData(t, rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Title != "修复配置问题" || gotProfile.Model != "title-model" || gotUser != "帮我看看这个" || gotAssistant != "定位到了配置问题" {
		t.Fatalf("response/profile/source = %#v %#v (%q, %q)", response, gotProfile, gotUser, gotAssistant)
	}
	if session, _ := registry.Get(created.ID); session.State != SessionStateCreated {
		t.Fatalf("title request changed session state to %q", session.State)
	}
}

func TestStoredSessionTitleSuggestionDoesNotStartWorker(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	writeTitleTestConfig(t, workDir)
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()

	createHandler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})
	createRec := httptest.NewRecorder()
	createHandler.ServeHTTP(createRec, httptest.NewRequest(
		http.MethodPost,
		"/sessions",
		strings.NewReader(fmt.Sprintf(`{"working_directory":%q,"profile":"title-test"}`, workDir)),
	))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", createRec.Code, createRec.Body.String())
	}
	var created Session
	if err := decodeAPIData(t, createRec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	for _, item := range completedTitleItems() {
		if _, err := store.AppendDisplayItem(context.Background(), created.ID, item); err != nil {
			t.Fatalf("append display item: %v", err)
		}
	}

	restartedRegistry := newSessionRegistry()
	restarted := newHandler(restartedRegistry, storedDisplayItemSource{store: store}, handlerOptions{
		store: store,
		titleSuggestion: func(context.Context, config.ProfileConfig, string, string) (string, error) {
			return "Stored title", nil
		},
	})
	rec := httptest.NewRecorder()
	restarted.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/title-suggestion", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(restartedRegistry.List()) != 0 {
		t.Fatalf("title suggestion loaded offline session into live registry: %#v", restartedRegistry.List())
	}
}

func TestSessionTitleSuggestionErrors(t *testing.T) {
	workDir := t.TempDir()
	writeTitleTestConfig(t, workDir)
	registry := newSessionRegistry()
	created := registry.Add(newSession(workDir, "title-test"))

	t.Run("not completed", func(t *testing.T) {
		source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{
			created.ID: {
				titleMessageItem(zotigosession.DisplayItemUserMessage, "user"),
				{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
			},
		}}
		handler := newHandler(registry, source, handlerOptions{})
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/title-suggestion", nil))
		assertAPIError(t, rec, http.StatusConflict, "conflict", "no turn has completed")
	})

	t.Run("timeout", func(t *testing.T) {
		source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{created.ID: completedTitleItems()}}
		handler := newHandler(registry, source, handlerOptions{
			titleTimeout: 5 * time.Millisecond,
			titleSuggestion: func(ctx context.Context, _ config.ProfileConfig, _, _ string) (string, error) {
				<-ctx.Done()
				return "", ctx.Err()
			},
		})
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/title-suggestion", nil))
		assertAPIError(t, rec, http.StatusGatewayTimeout, "internal_error", "timed out")
	})

	t.Run("generation failure", func(t *testing.T) {
		source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{created.ID: completedTitleItems()}}
		handler := newHandler(registry, source, handlerOptions{
			titleSuggestion: func(context.Context, config.ProfileConfig, string, string) (string, error) {
				return "", errors.New("provider unavailable")
			},
		})
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/title-suggestion", nil))
		assertAPIError(t, rec, http.StatusBadGateway, "internal_error", "title generation failed")
		if strings.Contains(rec.Body.String(), "provider unavailable") {
			t.Fatalf("provider error leaked through public response: %s", rec.Body.String())
		}
	})
}

func titleMessageItem(itemType zotigosession.DisplayItemType, text string) zotigosession.DisplayItem {
	return zotigosession.DisplayItem{Type: itemType, Content: []zotigosession.DisplayContentPart{{Type: string(protocol.ContentTypeText), Text: text}}}
}

func completedTitleItems() []zotigosession.DisplayItem {
	return []zotigosession.DisplayItem{
		titleMessageItem(zotigosession.DisplayItemUserMessage, "user"),
		{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
		titleMessageItem(zotigosession.DisplayItemAssistantMessage, "assistant"),
		{Type: zotigosession.DisplayItemTurnCompleted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
	}
}

func writeTitleTestConfig(t *testing.T, workDir string) {
	t.Helper()
	configData := "default_profile: title-test\nprofiles:\n  title-test:\n    provider: openai\n    model: title-model\n"
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(configData), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}
