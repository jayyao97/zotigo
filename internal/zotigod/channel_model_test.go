package zotigod

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/channels"
	"github.com/jayyao97/zotigo/internal/testutil/sessionlogtest"
)

func TestChannelModelResolvesCodexChoices(t *testing.T) {
	h := &handler{runtimes: newRuntimeRegistry(nativeRuntimeAdapter{}, &fakeCodexRuntime{})}
	for _, test := range []struct {
		choice  []string
		current string
		model   string
		effort  string
	}{
		// Model names match without regard to case; an unsupported current effort falls back.
		{[]string{"GPT-5.6-luna"}, "high", "gpt-5.6-luna", "medium"},
		{[]string{"gpt-5.6-luna", "Medium"}, "", "gpt-5.6-luna", "medium"},
	} {
		model, effort, err := h.resolveCodexChoice(context.Background(), test.choice, test.current)
		if err != nil || model != test.model || effort != test.effort {
			t.Fatalf("%v: model=%q effort=%q err=%v", test.choice, model, effort, err)
		}
	}
	for _, choice := range [][]string{nil, {"gpt-5.6-luna", "high"}, {"unknown"}, {"a", "b", "c"}} {
		if _, _, err := h.resolveCodexChoice(context.Background(), choice, ""); err == nil {
			t.Fatalf("accepted %v", choice)
		}
	}
}

// newChannelModelFixture stores a Zotigo and a Codex Session, both bound to
// Feishu topics. It returns the handler that /model calls, and the HTTP API
// over the same stores.
func newChannelModelFixture(t *testing.T) (*handler, http.Handler, *zotigosession.FileStore, *sessionlogtest.ItemSource) {
	t.Helper()
	ctx := context.Background()
	sessionStore, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessionStore.Close() })
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.ProjectConfig), []byte("profiles:\n  old:\n    provider: openai\n    model: old-model\n  new:\n    provider: openai\n    model: new-model\n"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, session := range []*zotigosession.Session{
		{Metadata: zotigosession.Metadata{ID: "channel-zotigo", Agent: channels.SessionAgentZotigo, WorkingDirectory: dir, ProfileName: "old", ApprovalPolicy: agent.ApprovalPolicyAuto, CreatedAt: now, UpdatedAt: now}},
		{Metadata: zotigosession.Metadata{ID: "channel-codex", Agent: channels.SessionAgentCodex, Model: "gpt-old", ReasoningEffort: "high", ApprovalPolicy: agent.ApprovalPolicyAuto, CreatedAt: now, UpdatedAt: now}},
	} {
		if err := sessionStore.Put(ctx, session); err != nil {
			t.Fatal(err)
		}
	}
	channelStore, err := channels.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = channelStore.Close() })
	if _, err := channelStore.PutConnection(ctx, channels.Connection{ID: "connection-1", Provider: channels.ProviderFeishu, Name: "test", AppID: "app"}); err != nil {
		t.Fatal(err)
	}
	for index, sessionID := range []string{"channel-zotigo", "channel-codex"} {
		conversation, err := channelStore.EnsureConversationRoot(ctx, "connection-1", "chat-1", fmt.Sprintf("root-%d", index), "", "group", now)
		if err != nil {
			t.Fatal(err)
		}
		conversation.SessionID, conversation.Enabled = sessionID, true
		if _, err := channelStore.PutConversation(ctx, conversation); err != nil {
			t.Fatal(err)
		}
	}
	secretStore, err := channels.NewSecretStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := channels.NewService(channelStore, secretStore, log.New(io.Discard, "", 0), map[string]channels.AdapterFactory{channels.ProviderFeishu: noopChannelFactory{}})
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
	registry := newSessionRegistry()
	runtimes := newRuntimeRegistry(nativeRuntimeAdapter{}, &fakeCodexRuntime{})
	h := &handler{registry: registry, items: source, store: sessionStore, workers: newWorkerRegistry(), runtimes: runtimes, channels: service, sessionOps: newSessionOperationLocks(), workspaceOps: newSessionOperationLocks()}
	api := newHandler(registry, source, handlerOptions{store: sessionStore, channels: service, runtimes: runtimes})
	return h, api, sessionStore, source
}

// /model must not change a Session with work in progress or queued, for either
// agent, so the queued work keeps the model it was sent with.
func TestChannelModelSwitchRequiresAnIdleSession(t *testing.T) {
	h, _, sessionStore, source := newChannelModelFixture(t)
	ctx := context.Background()
	for _, test := range []struct {
		id     string
		choice []string
		want   func(*zotigosession.Session) bool
	}{
		{"channel-zotigo", []string{"new"}, func(s *zotigosession.Session) bool { return s.ProfileName == "new" }},
		{"channel-codex", []string{"gpt-5.6-luna"}, func(s *zotigosession.Session) bool {
			return s.Model == "gpt-5.6-luna" && s.ReasoningEffort == "medium"
		}},
	} {
		source.Items[test.id] = []zotigosession.DisplayItem{{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "active"}}}
		_, _, err := h.SwitchChannelSessionRuntime(ctx, test.id, test.choice, holdNothing)
		var chatErr *channels.ChatError
		if !errors.As(err, &chatErr) || !strings.Contains(err.Error(), "turn to finish") {
			t.Fatalf("%s busy switch: %v", test.id, err)
		}
		if stored, _ := sessionStore.Get(ctx, test.id); test.want(stored) {
			t.Fatalf("%s changed while busy: %+v", test.id, stored.Metadata)
		}

		source.Items[test.id] = nil
		if _, _, err := h.SwitchChannelSessionRuntime(ctx, test.id, test.choice, holdNothing); err != nil {
			t.Fatalf("%s idle switch: %v", test.id, err)
		}
		if stored, _ := sessionStore.Get(ctx, test.id); !test.want(stored) {
			t.Fatalf("%s after switch: %+v", test.id, stored.Metadata)
		}
	}
}

func holdNothing() (func(), error) { return func() {}, nil }

// /model holds the Channel binding through the change: hold runs once the
// choice is resolved and before the Session lock, and a refusal leaves the
// Session as it was.
func TestChannelModelSwitchHoldsTheBinding(t *testing.T) {
	h, _, sessionStore, _ := newChannelModelFixture(t)
	ctx := context.Background()
	refused := channels.NewChatError(nil, "this group was unbound or disabled while the command ran")
	for _, test := range []struct {
		id     string
		choice []string
		bad    []string
	}{
		{"channel-zotigo", []string{"new"}, []string{"a", "b"}},
		{"channel-codex", []string{"gpt-5.6-luna"}, []string{"no-such-model"}},
	} {
		before, _ := sessionStore.Get(ctx, test.id)
		held := 0
		hold := func() (func(), error) {
			held++
			// The Session lock must still be free, or hold would invert the
			// configuration -> Session lock order.
			acquired := make(chan struct{})
			go func() { unlock := h.sessionOps.lock(test.id); unlock(); close(acquired) }()
			select {
			case <-acquired:
			case <-time.After(5 * time.Second):
				t.Errorf("%s: hold ran under the Session lock", test.id)
			}
			return nil, refused
		}
		if _, _, err := h.SwitchChannelSessionRuntime(ctx, test.id, test.bad, hold); err == nil || held != 0 {
			t.Fatalf("%s: invalid choice err=%v held=%d", test.id, err, held)
		}
		if _, _, err := h.SwitchChannelSessionRuntime(ctx, test.id, test.choice, hold); !errors.Is(err, refused) || held != 1 {
			t.Fatalf("%s: refused hold err=%v held=%d", test.id, err, held)
		}
		after, _ := sessionStore.Get(ctx, test.id)
		if after.ProfileName != before.ProfileName || after.Model != before.Model || after.ReasoningEffort != before.ReasoningEffort {
			t.Fatalf("%s changed although hold refused: %+v", test.id, after.Metadata)
		}
	}

	// Codex is probed before hold, not again while the binding is held: a probe
	// that would fail once hold returns does not stop the switch.
	defer func() { h.codexUnavailable = nil }()
	hold := func() (func(), error) {
		h.codexUnavailable = errors.New("probed while the binding was held")
		return func() {}, nil
	}
	if _, _, err := h.SwitchChannelSessionRuntime(ctx, "channel-codex", []string{"gpt-5.6-luna"}, hold); err != nil {
		t.Fatalf("switch probed Codex under hold: %v", err)
	}
	if stored, _ := sessionStore.Get(ctx, "channel-codex"); stored.Model != "gpt-5.6-luna" {
		t.Fatalf("after switch: %+v", stored.Metadata)
	}
}

// The HTTP endpoints share their logic with /model; their status codes must
// stay as they were.
func TestSessionRuntimeEndpointsReportStatusCodes(t *testing.T) {
	h, api, sessionStore, _ := newChannelModelFixture(t)
	ended := &zotigosession.Session{Metadata: zotigosession.Metadata{ID: "ended-codex", Agent: channels.SessionAgentCodex, Model: "gpt-old", ReasoningEffort: "high", ApprovalPolicy: agent.ApprovalPolicyAuto, CreatedAt: time.Now(), UpdatedAt: time.Now()}}
	if err := sessionStore.Put(context.Background(), ended); err != nil {
		t.Fatal(err)
	}
	h.registry.Add(Session{ID: "ended-codex", Agent: channels.SessionAgentCodex, State: SessionStateEnded})
	for _, test := range []struct {
		path, body string
		status     int
	}{
		{"/sessions/channel-zotigo/profile", `{"profile":" "}`, http.StatusBadRequest},
		{"/sessions/missing/profile", `{"profile":"new"}`, http.StatusNotFound},
		{"/sessions/channel-codex/profile", `{"profile":"new"}`, http.StatusBadRequest},
		{"/sessions/channel-zotigo/profile", `{"profile":"absent"}`, http.StatusBadRequest},
		{"/sessions/channel-codex/codex-settings", `{"model":"","reasoning_effort":"medium"}`, http.StatusBadRequest},
		{"/sessions/missing/codex-settings", `{"model":"gpt-5.6-luna","reasoning_effort":"medium"}`, http.StatusNotFound},
		{"/sessions/channel-zotigo/codex-settings", `{"model":"gpt-5.6-luna","reasoning_effort":"medium"}`, http.StatusBadRequest},
		{"/sessions/ended-codex/codex-settings", `{"model":"gpt-5.6-luna","reasoning_effort":"medium"}`, http.StatusConflict},
		{"/sessions/channel-codex/codex-settings", `{"model":"unknown","reasoning_effort":"medium"}`, http.StatusBadRequest},
		{"/sessions/channel-codex/codex-settings", `{"model":"gpt-5.6-luna","reasoning_effort":"medium"}`, http.StatusOK},
		{"/sessions/channel-zotigo/profile", `{"profile":"new"}`, http.StatusOK},
	} {
		recorder := httptest.NewRecorder()
		api.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, test.path, strings.NewReader(test.body)))
		if recorder.Code != test.status {
			t.Errorf("PUT %s %s: status=%d want %d body=%s", test.path, test.body, recorder.Code, test.status, recorder.Body.String())
		}
	}
}

// Only failures marked public reach the chat; a private failure wrapping a
// ChatError shows that error's public part.
func TestChatRuntimeErrorFollowsThePublicMark(t *testing.T) {
	probe := errors.New("exec /opt/codex/bin/codex: no such file")
	for _, test := range []struct {
		err    error
		public string
	}{
		{publicRuntimeError(http.StatusServiceUnavailable, "profile change requires an online worker"), "profile change requires an online worker"},
		{runtimeChangeError(http.StatusBadRequest, "load profiles: open /home/owner/.zotigo/config.yaml"), ""},
	} {
		var chatErr *channels.ChatError
		got := ""
		if errors.As(chatRuntimeError(test.err), &chatErr) {
			got = chatErr.Public()
		}
		if got != test.public {
			t.Errorf("%v: public %q, want %q", test.err, got, test.public)
		}
	}

	// A failed Codex probe keeps its error for the API and shows a summary in chat.
	h, _, _, _ := newChannelModelFixture(t)
	h.codexUnavailable = probe
	_, err := h.changeSessionCodexSettings(context.Background(), "channel-codex", "gpt-5.6-luna", "medium", codexSettingsOptions{requireIdle: true})
	if err == nil || err.Error() != "codex is unavailable: "+probe.Error() {
		t.Fatalf("API error = %v", err)
	}
	var chatErr *channels.ChatError
	if !errors.As(chatRuntimeError(err), &chatErr) || chatErr.Public() != "codex is unavailable" {
		t.Fatalf("chat error = %v", chatRuntimeError(err))
	}
}
