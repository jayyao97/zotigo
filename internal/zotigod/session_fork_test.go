package zotigod

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	session "github.com/jayyao97/zotigo/core/session"
)

func completeForkFixture(t *testing.T) (*handler, sessionToolCaller) {
	t.Helper()
	h, caller := newSessionToolFixture(t)
	source := caller.Stored
	source.AgentSnapshot = agent.Snapshot{State: agent.StateIdle, History: []protocol.Message{protocol.NewUserMessage("first"), protocol.NewAssistantMessage("first answer")}}
	source.ForkPoints = map[string]session.ForkPoint{"turn": {HistoryLength: 2, HistoryDigest: session.SnapshotDigest(source.AgentSnapshot)}}
	if err := h.store.Put(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.AppendDisplayItem(context.Background(), source.ID, session.DisplayItem{Type: session.DisplayItemTurnCompleted, Turn: &session.DisplayTurn{ID: "turn", Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	return h, caller
}

func TestNativeForkExactPrefixAndIsolation(t *testing.T) {
	h, caller := completeForkFixture(t)
	ctx := context.Background()
	source, _ := h.store.Get(ctx, caller.SessionID)
	source.AgentSnapshot.History = append(source.AgentSnapshot.History, protocol.NewUserMessage("future secret"), protocol.NewAssistantMessage("future answer"))
	if err := h.store.Put(ctx, source); err != nil {
		t.Fatal(err)
	}
	child, err := h.forkSession(ctx, "branch", source.ID, "turn", "Branch")
	if err != nil {
		t.Fatal(err)
	}
	if len(child.AgentSnapshot.History) != 2 || child.ForkedFrom.ThroughTurnID != "turn" || child.AgentSnapshot.State != agent.StateIdle {
		t.Fatalf("unexpected branch: %#v", child)
	}
	if child.ConversationID != "" || child.Capabilities.ChannelToolsVersion != 0 {
		t.Fatal("branch inherited runtime binding")
	}
	items, _, err := h.items.LoadItems(ctx, child.ID)
	if err != nil || len(items) != 3 {
		t.Fatalf("history: %d %v", len(items), err)
	}
	for _, item := range items {
		if item.Command != nil {
			t.Fatal("inherited replayable command")
		}
	}
	org, err := h.catalog.GetSessionOrganization(ctx, child.ID)
	if err != nil || org.WorkspaceID == nil || *org.WorkspaceID != caller.WorkspaceID {
		t.Fatalf("workspace: %#v %v", org, err)
	}
	if _, err := h.catalog.SetSessionTitle(ctx, child.ID, "Renamed"); err != nil {
		t.Fatal(err)
	}
	retried, err := h.forkSession(ctx, "branch", source.ID, "turn", "Branch")
	if err != nil || retried.ID != child.ID {
		t.Fatalf("retry: %#v %v", retried, err)
	}
	org, _ = h.catalog.GetSessionOrganization(ctx, child.ID)
	if org.Title == nil || *org.Title != "Renamed" {
		t.Fatal("retry overwrote title")
	}
	if _, err := h.forkSession(ctx, "branch", source.ID, "other", "Branch"); !errors.Is(err, errCommandIDConflict) {
		t.Fatalf("conflict: %v", err)
	}
	after, _ := h.store.Get(ctx, source.ID)
	if len(after.AgentSnapshot.History) != 4 {
		t.Fatal("source changed")
	}
}

func TestNativeForkRejectsMissingAndCompactedBoundary(t *testing.T) {
	h, caller := completeForkFixture(t)
	ctx := context.Background()
	if _, err := h.forkSession(ctx, "missing", caller.SessionID, "uncompleted", ""); err == nil {
		t.Fatal("accepted missing turn")
	}
	source, _ := h.store.Get(ctx, caller.SessionID)
	source.AgentSnapshot.History[0] = protocol.NewUserMessage("future summary")
	if err := h.store.Put(ctx, source); err != nil {
		t.Fatal(err)
	}
	if _, err := h.forkSession(ctx, "compacted", caller.SessionID, "turn", ""); err == nil || !strings.Contains(err.Error(), "compacted") {
		t.Fatalf("compaction: %v", err)
	}
	if child, _ := h.store.Get(ctx, "compacted"); child != nil {
		t.Fatal("failed validation created child")
	}
}

func TestSessionForkHTTPAndLatestCompleted(t *testing.T) {
	h, caller := completeForkFixture(t)
	for attempt := 0; attempt < 2; attempt++ {
		req := httptest.NewRequest("POST", "/sessions/"+caller.SessionID+"/fork", strings.NewReader(`{"request_id":"stable"}`))
		res := httptest.NewRecorder()
		h.handleSessionFork(res, req, caller.SessionID)
		if res.Code != 201 {
			t.Fatalf("response: %d %s", res.Code, res.Body.String())
		}
		var envelope struct {
			Data Session `json:"data"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Data.ForkedFrom == nil || envelope.Data.ForkedFrom.ThroughTurnID != "turn" {
			t.Fatal("missing lineage")
		}
	}
}

func TestSessionToolForkCannotReadOtherChannelSession(t *testing.T) {
	h, caller := completeForkFixture(t)
	caller.Origin = &protocol.RequestContext{Source: "channel"}
	req := workerRuntimeToolRequest{Name: "create_session", CallID: "fork", Arguments: json.RawMessage(`{"workspace_id":"` + caller.WorkspaceID + `","initial_message":"hello","fork_from":{"session_id":"other","through_turn_id":"turn"}}`)}
	if _, err := h.writeToolSession(context.Background(), caller, req); err == nil || !strings.Contains(err.Error(), "bound session") {
		t.Fatalf("channel fork: %v", err)
	}
}

func TestSessionForkCodexLatestIgnoresBackfillAndProjectsChild(t *testing.T) {
	h, caller := completeForkFixture(t)
	ctx := context.Background()
	source, _ := h.store.Get(ctx, caller.SessionID)
	source.Agent, source.ConversationID = "codex", "source-thread"
	source.LastPrompt = "future input must not be inherited"
	if err := h.store.Put(ctx, source); err != nil {
		t.Fatal(err)
	}
	// Older history is imported after the latest completion. Its append position
	// must not make it the default fork point.
	if _, err := h.store.AppendDisplayItem(ctx, source.ID, session.DisplayItem{Type: session.DisplayItemTurnCompleted, Turn: &session.DisplayTurn{ID: "older", Status: "completed"}, CreatedAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Unix()
	turn := codexTurn{ID: "turn", Status: "completed", StartedAt: &stamp, CompletedAt: &stamp, Items: []codexThreadItem{{ID: "answer-id", Type: "agentMessage", Text: "late inherited answer"}}}
	forkCalls := 0
	host := &codexSyncHost{rpc: codexSyncFuncRPC{call: func(_ context.Context, method string, raw any, result any) error {
		params := raw.(map[string]any)
		var payload any
		switch method {
		case "thread/fork":
			forkCalls++
			if params["lastTurnId"] != "turn" {
				t.Fatalf("selected older backfill: %#v", params)
			}
			payload = codexThreadSnapshot{Thread: codexThread{ID: "child-thread"}}
		case "thread/turns/list":
			payload = codexTurnList{Data: []codexTurn{turn}}
		case "thread/items/list":
			payload = codexThreadItemList{Data: []codexThreadItemEntry{{TurnID: "turn", Item: turn.Items[0]}}}
		default:
			t.Fatalf("unexpected call %s", method)
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, result)
	}}}
	h.codexSync = &codexSessionSyncer{host: host}
	child, err := h.forkSession(ctx, "codex-branch", source.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if child.LastPrompt != "" {
		t.Fatal("inherited future preview")
	}
	items, _, err := h.items.LoadItems(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	answers := 0
	for _, item := range items {
		if item.ID == "answer-id" {
			answers++
		}
	}
	if answers != 1 {
		t.Fatalf("missing late answer: %#v", items)
	}
	if err := syncCompletedCodexTurns(ctx, h.items, child.ID, []codexTurn{turn}); err != nil {
		t.Fatal(err)
	}
	after, _, _ := h.items.LoadItems(ctx, child.ID)
	if len(after) != len(items) {
		t.Fatal("resync duplicated inherited history")
	}
	if _, err := h.forkSession(ctx, "codex-branch", source.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	if forkCalls != 1 {
		t.Fatal("retry forked twice")
	}
}
