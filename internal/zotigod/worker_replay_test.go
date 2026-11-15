package zotigod

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/providers"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

func TestReplayWorkerCommandsSkipsAppliedMessageAfterCursorGap(t *testing.T) {
	for _, gapType := range []string{wire.CommandPause, wire.CommandSteering} {
		t.Run(gapType, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			providerName := "replay-gap-" + gapType
			providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
			workDir := t.TempDir()
			configText := fmt.Sprintf("default_profile: test\nprofiles:\n  test:\n    provider: %s\n    model: test\n", providerName)
			if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(configText), 0600); err != nil {
				t.Fatal(err)
			}
			store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			const sessionID = "replay-gap"
			putStoredSession(t, store, sessionID, workDir)
			ctx := context.Background()
			for _, item := range []zotigosession.DisplayItem{
				{ID: "gap", Type: zotigosession.DisplayItemSessionCommand, Command: &zotigosession.DisplayCommand{Type: gapType, TurnID: "old-turn"}},
				{ID: "message", Type: zotigosession.DisplayItemUserMessage, Command: &zotigosession.DisplayCommand{Type: wire.CommandMessage, Text: "already executed"}},
				{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "message-turn"}},
				{Type: zotigosession.DisplayItemTurnCompleted, Turn: &zotigosession.DisplayTurn{ID: "message-turn"}},
			} {
				if _, err := store.AppendDisplayItem(ctx, sessionID, item); err != nil {
					t.Fatal(err)
				}
			}
			if err := workerbase.SaveCommandCursor(sessionID, workerbase.CommandCursor{Sequence: 2}); err != nil {
				t.Fatal(err)
			}
			cursor, err := workerbase.LoadCommandCursor(ctx, store, sessionID)
			if err != nil || cursor.Sequence != 0 {
				t.Fatalf("pending command must remain replayable: cursor=%+v err=%v", cursor, err)
			}
			runtime, err := newWorkerRuntime(ctx, workerRuntimeConfig{SessionID: sessionID, Store: store})
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			gap := wire.Command{ID: "gap", Sequence: 1, Type: gapType}
			if gapType == wire.CommandPause {
				gap.Pause = &wire.PauseCommandPayload{TurnID: "old-turn", Reason: wire.UserPauseReason}
			} else {
				gap.Steering = &wire.SteeringCommandPayload{TurnID: "old-turn", Text: "stale"}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, wire.CommandsPage{Commands: []wire.Command{
					gap,
					{ID: "message", Sequence: 2, Type: wire.CommandMessage, Message: &wire.MessageCommandPayload{Text: "already executed"}},
				}, NextOffset: 1})
			}))
			defer server.Close()
			if _, err := replayWorkerCommands(ctx, server.Client(), server.URL, sessionID, runtime, cursor); err != nil {
				t.Fatal(err)
			}
			items, _, err := store.ListDisplayItems(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			starts := 0
			for _, item := range items {
				if item.Type == zotigosession.DisplayItemTurnStarted {
					starts++
				}
			}
			if starts != 1 {
				t.Fatalf("replayed an already executed message: %d turn starts, want 1", starts)
			}

			pending, err := store.AppendDisplayItem(ctx, sessionID, zotigosession.DisplayItem{
				ID: "pending", Type: zotigosession.DisplayItemUserMessage,
				Command: &zotigosession.DisplayCommand{Type: wire.CommandMessage, Text: "execute once"},
			})
			if err != nil {
				t.Fatal(err)
			}
			pendingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, wire.CommandsPage{Commands: []wire.Command{
					gap,
					{ID: "message", Sequence: 2, Type: wire.CommandMessage, Message: &wire.MessageCommandPayload{Text: "already executed"}},
					{ID: pending.ID, Sequence: pending.Sequence, Type: wire.CommandMessage, Message: &wire.MessageCommandPayload{Text: "execute once"}},
				}, NextOffset: 2})
			}))
			defer pendingServer.Close()
			for attempt := 0; attempt < 2; attempt++ {
				if _, err := replayWorkerCommands(ctx, pendingServer.Client(), pendingServer.URL, sessionID, runtime, cursor); err != nil {
					t.Fatal(err)
				}
				runtime.mu.Lock()
				done := runtime.turnDone
				runtime.mu.Unlock()
				if done != nil {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Fatal("pending message did not finish")
					}
				}
				items, _, err := store.ListDisplayItems(ctx, sessionID)
				if err != nil {
					t.Fatal(err)
				}
				starts := 0
				for _, item := range items {
					if item.Type == zotigosession.DisplayItemTurnStarted {
						starts++
					}
				}
				if starts != 2 {
					t.Fatalf("attempt %d: pending message must execute exactly once, got %d total starts", attempt, starts)
				}
			}
		})
	}
}
