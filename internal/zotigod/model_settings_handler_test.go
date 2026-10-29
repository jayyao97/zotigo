package zotigod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

func TestModelSettingsAPI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h := NewHandler()
	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest("GET", "/config/models", nil))
	if get.Code != 200 {
		t.Fatal(get.Body.String())
	}
	var response struct {
		Data config.ModelSettings `json:"data"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	snapshot := response.Data
	key := "test-secret"
	change := config.ModelSettingsChange{Revision: snapshot.Revision, Action: "create", Profile: config.ModelSettingsProfile{Name: "test", Provider: "openai", Model: "test-model"}, APIKey: &key}
	body, _ := json.Marshal(change)
	put := httptest.NewRecorder()
	h.ServeHTTP(put, httptest.NewRequest("PUT", "/config/models", bytes.NewReader(body)))
	if put.Code != 200 {
		t.Fatal(put.Body.String())
	}
	if strings.Contains(put.Body.String(), key) || !strings.Contains(put.Body.String(), `"has_api_key":true`) {
		t.Fatal("secret projection incorrect")
	}
	stale := httptest.NewRecorder()
	h.ServeHTTP(stale, httptest.NewRequest("PUT", "/config/models", bytes.NewReader(body)))
	if stale.Code != 409 {
		t.Fatalf("stale update: %d", stale.Code)
	}
	profiles := httptest.NewRecorder()
	h.ServeHTTP(profiles, httptest.NewRequest("GET", "/config/profiles?scope=global", nil))
	if profiles.Code != 200 || !strings.Contains(profiles.Body.String(), "test-model") {
		t.Fatalf("saved model not visible: %s", profiles.Body.String())
	}
}

func TestModelConnectionTestDoesNotSaveOrLeakProviderBody(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing credential")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"message":"secret-in-error","type":"authentication_error"}}`))
	}))
	defer server.Close()
	snapshot, err := config.NewManager().ReadModelSettings()
	if err != nil {
		t.Fatal(err)
	}
	key := "test-key"
	change := config.ModelSettingsChange{Revision: snapshot.Revision, Action: "create", Profile: config.ModelSettingsProfile{Name: "test", Provider: "openai", Model: "example", BaseURL: server.URL, Mode: "chat"}, APIKey: &key}
	body, _ := json.Marshal(change)
	recorder := httptest.NewRecorder()
	NewHandler().ServeHTTP(recorder, httptest.NewRequest("POST", "/config/models/test", bytes.NewReader(body)))
	if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), `"authentication"`) || strings.Contains(recorder.Body.String(), "secret-in-error") {
		t.Fatalf("unexpected test response: %d %s", recorder.Code, recorder.Body.String())
	}
	after, err := config.NewManager().ReadModelSettings()
	if err != nil || len(after.Profiles) != 0 {
		t.Fatal("connection test persisted configuration")
	}
}

type cleanupModelTestProvider struct {
	started chan struct{}
	exited  chan struct{}
	finish  bool
}

func (*cleanupModelTestProvider) Name() string { return "cleanup-test" }
func (p *cleanupModelTestProvider) StreamChat(ctx context.Context, _ []protocol.Message, _ []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	events := make(chan protocol.Event)
	go func() {
		defer close(p.exited)
		defer close(events)
		close(p.started)
		if p.finish {
			events <- protocol.NewFinishEvent(protocol.FinishReasonStop)
		}
		<-ctx.Done()
		events <- protocol.NewErrorEvent(ctx.Err())
	}()
	return events, nil
}
func TestModelConnectionDrainsProviderOnSuccessAndCancellation(t *testing.T) {
	for _, finish := range []bool{false, true} {
		t.Run(fmt.Sprint(finish), func(t *testing.T) {
			p := &cleanupModelTestProvider{started: make(chan struct{}), exited: make(chan struct{}), finish: finish}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- testModelConnection(ctx, p) }()
			<-p.started
			if !finish {
				cancel()
			}
			select {
			case err := <-result:
				if finish && err != nil {
					t.Fatal(err)
				}
				if !finish && !errors.Is(err, context.Canceled) {
					t.Fatalf("got %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("model test did not finish")
			}
			select {
			case <-p.exited:
			case <-time.After(2 * time.Second):
				t.Fatal("provider sender leaked")
			}
		})
	}
}
