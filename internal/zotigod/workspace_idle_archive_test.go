package zotigod

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
)

func TestWorkspaceArchiveIdleAndBusySessions(t *testing.T) {
	for _, mode := range []string{"idle", "open turn", "approval", "pending message", "queued input", "starting", "working", "paused", "failed pending message", "failed open turn"} {
		t.Run(mode, func(t *testing.T) {
			source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
			workers := newWorkerRegistry()
			api, registry, catalog, workspace := newCatalogSessionFixtureWithRuntime(t, newSessionOperationLocks(), source, workers)
			writeTestProfileConfig(t, workspace.RootPath)
			created := requestCatalog(t, api, http.MethodPost, "/sessions", `{"workspace_id":`+quotedJSON(t, workspace.ID)+`}`)
			var session Session
			decodeCatalogData(t, created, &session)
			if _, err := registry.Start(session.ID); err != nil {
				t.Fatal(err)
			}
			if mode != "starting" {
				if _, err := registry.MarkRunning(session.ID); err != nil {
					t.Fatal(err)
				}
			}
			worker := newWorkerConnection(session.ID, "test-generation", nil, workers)
			workers.workers[session.ID] = worker
			switch mode {
			case "open turn", "approval", "failed open turn":
				source.items[session.ID] = []zotigosession.DisplayItem{{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn"}}}
				if mode == "approval" {
					source.items[session.ID] = append(source.items[session.ID], zotigosession.DisplayItem{Type: zotigosession.DisplayItemApprovalRequest, Approval: &zotigosession.DisplayApproval{ID: "approval"}})
				}
			case "pending message", "failed pending message":
				source.items[session.ID] = []zotigosession.DisplayItem{{Type: zotigosession.DisplayItemUserMessage, Command: &zotigosession.DisplayCommand{Type: sessionCommandMessage, Text: "pending"}}}
			case "queued input":
				if err := catalog.QueueInput(t.Context(), session.ID, "queued", []byte(`{"text":"pending"}`)); err != nil {
					t.Fatal(err)
				}
			case "working":
				registry.MarkWorking(session.ID, "shell")
			case "paused":
				registry.mu.Lock()
				s := registry.sessions[session.ID]
				s.State = SessionStatePaused
				registry.sessions[session.ID] = s
				registry.mu.Unlock()
			}
			failed := mode == "failed pending message" || mode == "failed open turn"
			if failed {
				if _, err := registry.Fail(session.ID, "worker failed"); err != nil {
					t.Fatal(err)
				}
				closeDetachedWorker(workers.Detach(session.ID))
			}
			response := requestCatalog(t, api, http.MethodPost, "/workspaces/"+workspace.ID+"/archive", "")
			want := http.StatusConflict
			if mode == "idle" || failed {
				want = http.StatusOK
			}
			if response.Code != want {
				t.Fatalf("archive = %d, want %d: %s", response.Code, want, response.Body.String())
			}
			runtime, _ := registry.Get(session.ID)
			if mode == "idle" {
				if runtime.State != SessionStateCreated || workers.Has(session.ID) {
					t.Fatalf("idle worker not released: %+v", runtime)
				}
				select {
				case <-worker.doneCh:
				default:
					t.Fatal("worker connection not closed")
				}
				restore := requestCatalog(t, api, http.MethodPost, "/workspaces/"+workspace.ID+"/unarchive", "")
				if restore.Code != http.StatusOK {
					t.Fatalf("unarchive: %s", restore.Body.String())
				}
				if _, err := registry.Start(session.ID); err != nil {
					t.Fatalf("restart: %v", err)
				}
			} else if failed {
				if runtime.State != SessionStateFailed {
					t.Fatalf("historical failure was changed: %+v", runtime)
				}
			} else if !workers.Has(session.ID) {
				t.Fatal("busy worker was closed")
			}
		})
	}
}

// A closed transport does not mean the worker has finished saving its session.
type archiveLockStore struct {
	zotigosession.Store
	unlocked <-chan struct{}
}

func (s archiveLockStore) IsLocked(context.Context, string) (bool, error) {
	select {
	case <-s.unlocked:
		return false, nil
	default:
		return true, nil
	}
}

func TestWorkspaceArchiveWaitsForWorkerLock(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	workers := newWorkerRegistry()
	api, registry, catalog, workspace := newCatalogSessionFixtureWithRuntime(t, newSessionOperationLocks(), source, workers)
	writeTestProfileConfig(t, workspace.RootPath)
	created := requestCatalog(t, api, http.MethodPost, "/sessions", `{"workspace_id":`+quotedJSON(t, workspace.ID)+`}`)
	var session Session
	decodeCatalogData(t, created, &session)
	if _, err := registry.Start(session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.MarkRunning(session.ID); err != nil {
		t.Fatal(err)
	}
	worker := newWorkerConnection(session.ID, "generation", nil, workers)
	workers.workers[session.ID] = worker
	unlocked := make(chan struct{})
	h := &handler{registry: registry, catalog: catalog, items: source, workers: workers, sessionOps: newSessionOperationLocks(), store: archiveLockStore{unlocked: unlocked}}
	done := make(chan error, 1)
	go func() {
		release, blocked, err := h.lockWorkspaceSessions(t.Context(), workspace.ID)
		defer release()
		if blocked && err == nil {
			err = errSessionBusy
		}
		done <- err
	}()
	select {
	case <-worker.doneCh:
	case <-time.After(time.Second):
		t.Fatal("worker not closed")
	}
	select {
	case err := <-done:
		t.Fatalf("returned before lock release: %v", err)
	default:
	}
	close(unlocked)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("lock release not observed")
	}
}

func TestWorkspaceArchiveKeepsUnknownLockAndHonorsCancellation(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(map[bool]string{false: "external lock", true: "worker did not release"}[owned], func(t *testing.T) {
			source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
			workers := newWorkerRegistry()
			api, registry, catalog, workspace := newCatalogSessionFixtureWithRuntime(t, newSessionOperationLocks(), source, workers)
			writeTestProfileConfig(t, workspace.RootPath)
			response := requestCatalog(t, api, http.MethodPost, "/sessions", `{"workspace_id":`+quotedJSON(t, workspace.ID)+`}`)
			var session Session
			decodeCatalogData(t, response, &session)
			if _, err := registry.Start(session.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := registry.MarkRunning(session.ID); err != nil {
				t.Fatal(err)
			}
			if owned {
				workers.workers[session.ID] = newWorkerConnection(session.ID, "generation", nil, workers)
			}
			h := &handler{registry: registry, catalog: catalog, items: source, workers: workers, sessionOps: newSessionOperationLocks(), store: archiveLockStore{unlocked: make(chan struct{})}}
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			release, blocked, err := h.lockWorkspaceSessions(ctx, workspace.ID)
			release()
			if owned && (err == nil || !strings.Contains(err.Error(), "idle workers were released")) {
				t.Fatalf("missing partial-release explanation: %v", err)
			}
			if owned {
				rec := httptest.NewRecorder()
				h.writeCatalogError(rec, err)
				if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "workspace was not removed") {
					t.Fatalf("archive retry explanation hidden: %d %s", rec.Code, rec.Body.String())
				}
			}
			if !owned && (err != nil || !blocked) {
				t.Fatalf("locked workspace allowed: blocked=%v err=%v", blocked, err)
			}
			state, _ := registry.Get(session.ID)
			if !owned && state.State != SessionStateRunning {
				t.Fatal("external worker state modified")
			}
			current, err := catalog.GetWorkspace(t.Context(), workspace.ID)
			if err != nil || current.Status != "ready" {
				t.Fatalf("workspace changed: %+v %v", current, err)
			}
		})
	}
}
