package zotigod

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/update"
)

func testUpdater(t *testing.T) *update.Manager {
	t.Helper()
	prefix := t.TempDir()
	current := filepath.Join(prefix, "daemon", "current")
	if err := os.MkdirAll(current, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(current, "INSTALLATION"), []byte("version=1.0.0\ncommit=test\ndirty=false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := update.New(prefix, "daemon")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(prefix, "updates", "daemon")
	if err := os.WriteFile(filepath.Join(dir, "status.json"), []byte(`{"channel":"stable","phase":"waiting"}`), 0600); err != nil {
		t.Fatal(err)
	}
	// Simulate the handoff window; recovery must not treat this fixture as dead.
	if err := os.WriteFile(filepath.Join(dir, "starting"), []byte("starting"), 0600); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestUpdateDrainClosesAdmissionAndWaitsForTurns(t *testing.T) {
	for _, busy := range []bool{true, false} {
		t.Run(map[bool]string{true: "busy", false: "idle"}[busy], func(t *testing.T) {
			m := testUpdater(t)
			registry := newSessionRegistry()
			registry.Add(Session{ID: "session", State: SessionStateRunning, Working: busy})
			h := &handler{updater: m, registry: registry, items: &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}}
			rec := httptest.NewRecorder()
			h.handleUpdateDrain(rec, httptest.NewRequest("POST", "/updates/drain", nil))
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), map[bool]string{true: `"data":false`, false: `"data":true`}[busy]) {
				t.Fatal(rec.Body.String())
			}
			if !m.Draining() {
				t.Fatal("admission still open")
			}
			if _, err := h.acceptSessionInputCommand(context.Background(), "session", "", "new task", nil, nil, "", false, false, nil); !errors.Is(err, errSessionUnavailable) {
				t.Fatalf("new task admitted: %v", err)
			}
			if _, err := h.ensureSessionRunningInWorkspace(context.Background(), "new-session"); !errors.Is(err, errSessionUnavailable) {
				t.Fatalf("new worker admitted: %v", err)
			}
		})
	}
}

func TestUpdateDrainWaitsForDurableHumanRequest(t *testing.T) {
	m := testUpdater(t)
	registry := newSessionRegistry()
	registry.Add(Session{ID: "session", State: SessionStateRunning})
	h := &handler{updater: m, registry: registry, items: &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{"session": {{ID: "turn", Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn"}}}}}}
	rec := httptest.NewRecorder()
	h.handleUpdateDrain(rec, httptest.NewRequest("POST", "/updates/drain", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"data":false`) {
		t.Fatal(rec.Body.String())
	}
}

func TestUpdateAPIRejectsWorkerTokenAndUnmanagedInstall(t *testing.T) {
	h := newHandler(newSessionRegistry(), &fakeDisplayItemSource{}, handlerOptions{publicAuthToken: "owner", workerAuthToken: "worker"})
	for token, want := range map[string]int{"worker": 401, "owner": 501} {
		req := httptest.NewRequest("GET", "/updates", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s: %d %s", token, rec.Code, rec.Body.String())
		}
	}
}
