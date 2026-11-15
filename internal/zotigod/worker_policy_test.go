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

	"github.com/jayyao97/zotigo/internal/testutil/sessionlogtest"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/providers"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

type failingApprovalPolicyStore struct {
	zotigosession.Store
	onUpdate func()
}

func (s failingApprovalPolicyStore) UpdateApprovalPolicy(context.Context, string, agent.ApprovalPolicy, time.Time) error {
	if s.onUpdate != nil {
		s.onUpdate()
	}
	return errors.New("approval policy persistence unavailable")
}

func TestWorkerRuntimeRestoresAndPersistsApprovalPolicy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const providerName = "approval-policy-runtime-test"
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	workDir := t.TempDir()
	projectConfig := fmt.Sprintf("default_profile: test\nprofiles:\n  test:\n    provider: %s\n    model: test\n", providerName)
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:               "sess-runtime-policy",
			WorkingDirectory: workDir,
			ProfileName:      "test",
			ApprovalPolicy:   agent.ApprovalPolicyBypass,
			CreatedAt:        now,
			UpdatedAt:        now,
		},
		AgentSnapshot: agent.Snapshot{State: agent.StateIdle, CreatedAt: now},
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}

	runtime, err := newWorkerRuntime(context.Background(), workerRuntimeConfig{SessionID: "sess-runtime-policy", Store: store})
	if err != nil {
		t.Fatalf("create worker runtime: %v", err)
	}
	defer runtime.Close()
	if got := runtime.agent.Describe().ApprovalPolicy; got != agent.ApprovalPolicyBypass {
		t.Fatalf("restored policy = %q, want bypass", got)
	}
	commandItem, err := store.AppendDisplayItem(context.Background(), "sess-runtime-policy", zotigosession.DisplayItem{
		Type:    zotigosession.DisplayItemSessionCommand,
		Command: &zotigosession.DisplayCommand{Type: wire.CommandApprovalPolicy, ApprovalPolicy: string(agent.ApprovalPolicyAuto)},
	})
	if err != nil {
		t.Fatalf("append policy command: %v", err)
	}
	command := wire.Command{
		ID:             commandItem.ID,
		Sequence:       commandItem.Sequence,
		Type:           wire.CommandApprovalPolicy,
		ApprovalPolicy: &wire.ApprovalPolicyCommandPayload{Policy: agent.ApprovalPolicyAuto},
	}
	oppositeItem, err := store.AppendDisplayItem(context.Background(), "sess-runtime-policy", zotigosession.DisplayItem{
		Type:    zotigosession.DisplayItemSessionCommand,
		Command: &zotigosession.DisplayCommand{Type: wire.CommandApprovalPolicy, ApprovalPolicy: string(agent.ApprovalPolicyBypass)},
	})
	if err != nil {
		t.Fatalf("append opposite policy command: %v", err)
	}
	oppositeCommand := wire.Command{
		ID:             oppositeItem.ID,
		Sequence:       oppositeItem.Sequence,
		Type:           wire.CommandApprovalPolicy,
		ApprovalPolicy: &wire.ApprovalPolicyCommandPayload{Policy: agent.ApprovalPolicyBypass},
	}
	commandServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeWorkerTestJSON(w, http.StatusOK, wire.CommandsPage{
			Commands:   []wire.Command{command, oppositeCommand},
			NextCursor: fmt.Sprintf("%d", oppositeCommand.Sequence),
			NextOffset: 1,
		})
	}))
	defer commandServer.Close()

	storedItems := runtime.display.Items
	runtime.display.Items = &sessionlogtest.ItemSource{
		Items: map[string][]zotigosession.DisplayItem{"sess-runtime-policy": {commandItem, oppositeItem}},
		AppendErr: func(_ string, item zotigosession.DisplayItem) error {
			if item.Type == zotigosession.DisplayItemApprovalPolicyChanged {
				return fmt.Errorf("completion marker unavailable")
			}
			return nil
		},
	}
	cursor, replayErr := replayWorkerCommands(context.Background(), commandServer.Client(), commandServer.URL, runtime.sessionID, runtime, workerbase.CommandCursor{})
	if replayErr == nil || !strings.Contains(replayErr.Error(), "completion marker unavailable") {
		t.Fatalf("expected completion marker failure, got %v", replayErr)
	}
	if cursor != (workerbase.CommandCursor{}) {
		t.Fatalf("cursor advanced before completion marker: %#v", cursor)
	}
	if got := runtime.agent.Describe().ApprovalPolicy; got != agent.ApprovalPolicyAuto {
		t.Fatalf("runtime policy = %q, want auto", got)
	}
	stored, err := store.Get(context.Background(), "sess-runtime-policy")
	if err != nil || stored.ApprovalPolicy != agent.ApprovalPolicyAuto {
		t.Fatalf("stored policy = %#v, err = %v", stored, err)
	}

	runtime.display.Items = storedItems
	cursor, err = replayWorkerCommands(context.Background(), commandServer.Client(), commandServer.URL, runtime.sessionID, runtime, workerbase.CommandCursor{})
	if err != nil {
		t.Fatalf("replay pending policy command: %v", err)
	}
	if cursor.Sequence != oppositeCommand.Sequence || cursor.Offset != 1 {
		t.Fatalf("unexpected replay cursor: %#v", cursor)
	}
	savedCursor, err := workerbase.LoadCommandCursor(context.Background(), nil, runtime.sessionID)
	if err != nil {
		t.Fatalf("load saved cursor: %v", err)
	}
	if savedCursor != cursor {
		t.Fatalf("saved cursor = %#v, want %#v", savedCursor, cursor)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "sess-runtime-policy")
	if err != nil {
		t.Fatalf("load display items: %v", err)
	}
	if runtime.agent.Describe().ApprovalPolicy != agent.ApprovalPolicyBypass {
		t.Fatalf("final runtime policy = %q, want bypass", runtime.agent.Describe().ApprovalPolicy)
	}
	stored, err = store.Get(context.Background(), "sess-runtime-policy")
	if err != nil || stored.ApprovalPolicy != agent.ApprovalPolicyBypass {
		t.Fatalf("final stored policy = %#v, err = %v", stored, err)
	}
	if got := workerbase.RecoverAppliedCommandSequence(items); got != oppositeCommand.Sequence {
		t.Fatalf("recovered cursor = %d, want %d", got, oppositeCommand.Sequence)
	}
	if len(items) != 4 || items[2].Type != zotigosession.DisplayItemApprovalPolicyChanged || items[3].Type != zotigosession.DisplayItemApprovalPolicyChanged {
		t.Fatalf("expected one completion marker per command, got %#v", items)
	}

	staleCursor, err := replayWorkerCommands(context.Background(), commandServer.Client(), commandServer.URL, runtime.sessionID, runtime, workerbase.CommandCursor{})
	if err != nil {
		t.Fatalf("replay stale cursor: %v", err)
	}
	if staleCursor.Sequence != oppositeCommand.Sequence {
		t.Fatalf("stale replay cursor = %#v", staleCursor)
	}
	items, _, err = store.ListDisplayItems(context.Background(), "sess-runtime-policy")
	if err != nil {
		t.Fatalf("load items after stale replay: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("stale replay duplicated completion marker: %#v", items)
	}

}

func TestWorkerRuntimeApprovalPolicyPersistenceFailureKeepsSaferPolicy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const providerName = "approval-policy-persistence-failure-test"
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	workDir := t.TempDir()
	projectConfig := fmt.Sprintf("default_profile: test\nprofiles:\n  test:\n    provider: %s\n    model: test\n", providerName)
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	tests := []struct {
		name    string
		initial agent.ApprovalPolicy
		target  agent.ApprovalPolicy
	}{
		{name: "upgrade", initial: agent.ApprovalPolicyAuto, target: agent.ApprovalPolicyBypass},
		{name: "downgrade", initial: agent.ApprovalPolicyBypass, target: agent.ApprovalPolicyAuto},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
			if err != nil {
				t.Fatalf("create store: %v", err)
			}
			defer store.Close()
			now := time.Now().UTC()
			sessionID := "policy-persistence-failure-" + tt.name
			if err := store.Put(context.Background(), &zotigosession.Session{
				Metadata: zotigosession.Metadata{
					ID:               sessionID,
					WorkingDirectory: workDir,
					ProfileName:      "test",
					ApprovalPolicy:   tt.initial,
					CreatedAt:        now,
					UpdatedAt:        now,
				},
				AgentSnapshot: agent.Snapshot{State: agent.StateIdle, CreatedAt: now},
			}); err != nil {
				t.Fatalf("put session: %v", err)
			}
			failingStore := failingApprovalPolicyStore{Store: store}
			runtime, err := newWorkerRuntime(context.Background(), workerRuntimeConfig{
				SessionID: sessionID,
				Store:     &failingStore,
			})
			if err != nil {
				t.Fatalf("create worker runtime: %v", err)
			}
			defer runtime.Close()
			failingStore.onUpdate = func() {
				if got := runtime.agent.Describe().ApprovalPolicy; got != agent.ApprovalPolicyAuto {
					t.Errorf("runtime policy at persistence = %q, want safer auto", got)
				}
			}

			err = runtime.setApprovalPolicy(context.Background(), "command-1", &wire.ApprovalPolicyCommandPayload{Policy: tt.target})
			if err == nil || !strings.Contains(err.Error(), "approval policy persistence unavailable") {
				t.Fatalf("set policy error = %v", err)
			}
			if got := runtime.agent.Describe().ApprovalPolicy; got != agent.ApprovalPolicyAuto {
				t.Fatalf("runtime policy = %q, want safer auto", got)
			}
			stored, err := store.Get(context.Background(), sessionID)
			if err != nil {
				t.Fatalf("load stored session: %v", err)
			}
			if stored.ApprovalPolicy != tt.initial {
				t.Fatalf("stored policy = %q, want unchanged %q", stored.ApprovalPolicy, tt.initial)
			}
		})
	}
}
