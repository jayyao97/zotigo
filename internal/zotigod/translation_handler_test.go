package zotigod

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/config"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

func TestTranslationDoesNotCreateSessionOrHistory(t *testing.T) {
	var input map[string]any
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	cwd := t.TempDir()
	cfg := fmt.Sprintf("default_profile: translate\nprofiles:\n  translate:\n    provider: openai\n    model: translation-test\n    api_key: fixture\n    base_url: %s/v1\n", provider.URL)
	if err := os.WriteFile(filepath.Join(cwd, config.ProjectConfig), []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	registry := newSessionRegistry()
	// Stored profile metadata can be newer than the daemon's runtime registry.
	session := registry.Add(newSession(cwd, "stale-profile"))
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{ID: session.ID, WorkingDirectory: cwd, ProfileName: "translate"}}); err != nil {
		t.Fatal(err)
	}
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(registry, source, handlerOptions{store: store})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+session.ID+"/translate", strings.NewReader(`{"text":"hello","target_language":"zh-CN"}`)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "你好") {
		t.Fatalf("response: %d %s", rec.Code, rec.Body.String())
	}
	if len(source.items) != 0 {
		t.Fatal("translation wrote conversation history")
	}
	if calls := source.loadCalls.Load(); calls != 0 {
		t.Fatalf("translation loaded conversation history %d times", calls)
	}
	messages, ok := input["messages"].([]any)
	if !ok || len(messages) != 2 || input["tools"] != nil {
		t.Fatalf("unexpected model input: %#v", input)
	}
	offline := newHandler(newSessionRegistry(), source, handlerOptions{store: store})
	rec = httptest.NewRecorder()
	offline.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+session.ID+"/translate", strings.NewReader(`{"text":"hello","target_language":"zh-CN"}`)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "你好") {
		t.Fatalf("offline response: %d %s", rec.Code, rec.Body.String())
	}
	if calls := source.loadCalls.Load(); calls != 0 {
		t.Fatalf("offline translation loaded conversation history %d times", calls)
	}
	list := httptest.NewRecorder()
	handler.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	if strings.Count(list.Body.String(), session.ID) != 1 {
		t.Fatalf("unexpected sessions: %s", list.Body.String())
	}
}

func TestTranslationRejectsInvalidRequest(t *testing.T) {
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{})
	for _, body := range []string{`{}`, `{"text":"hello","target_language":"de"}`, `{"text":"hello","target_language":"en","extra":true}`} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/missing/translate", strings.NewReader(body)))
		if rec.Code != 400 {
			t.Fatalf("status = %d", rec.Code)
		}
	}
}
