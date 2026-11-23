package zotigod

import (
	"net/http"
	"testing"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/testutil/sessionlogtest"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

// The catalog page reads live activity from the registry while the session
// detail derives it from the display log. A tool result must clear both, or
// clients alternating between them show a finished tool as running.
func TestToolResultClearsActiveToolInCatalogPage(t *testing.T) {
	items := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
	workers := newWorkerRegistry()
	handler, registry, _, workspace := newCatalogSessionFixtureWithRuntime(t, newSessionOperationLocks(), items, workers)
	sessionlogtest.WriteProfileConfig(t, workspace.RootPath)
	created := requestCatalog(t, handler, http.MethodPost, "/sessions", `{"workspace_id":`+quotedJSON(t, workspace.ID)+`}`)
	if created.Code != http.StatusCreated {
		t.Fatal(created.Body.String())
	}
	var session Session
	decodeCatalogData(t, created, &session)
	if _, err := registry.Start(session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.MarkRunning(session.ID); err != nil {
		t.Fatal(err)
	}
	deliver := func(msg wire.Message) {
		workers.mu.Lock()
		onMessage := workers.onMessage
		workers.mu.Unlock()
		onMessage(session.ID, "generation", msg)
	}
	activeTools := func() (string, string) {
		var detail Session
		decodeCatalogData(t, requestCatalog(t, handler, http.MethodGet, "/sessions/"+session.ID, ""), &detail)
		var page struct {
			Sessions []sessionProjection `json:"sessions"`
		}
		decodeCatalogData(t, requestCatalog(t, handler, http.MethodGet, "/catalog/sessions?page=true&ids="+session.ID, ""), &page)
		return detail.ActiveTool, page.Sessions[0].Runtime.ActiveTool
	}
	const callID = "call-shell"
	log := []zotigosession.DisplayItem{
		{ID: "turn", Sequence: 1, Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
		{ID: "call", Sequence: 2, Type: zotigosession.DisplayItemAssistantMessage, Content: []zotigosession.DisplayContentPart{{Type: "tool_call", ToolCall: &zotigosession.DisplayToolCall{ID: callID, Name: "shell"}}}},
		{ID: "started", Sequence: 3, Type: zotigosession.DisplayItemToolExecutionStarted, ToolExecution: &zotigosession.DisplayToolExecution{ToolCallID: callID, ToolName: "shell"}},
	}
	items.Mu.Lock()
	items.Items[session.ID] = log
	items.Mu.Unlock()
	deliver(wire.Message{Type: wire.MessageDelta, Delta: &wire.DisplayDelta{ItemID: "tool-progress:" + callID, PartType: "tool_progress", Delta: "output\n", ToolCallID: callID, ToolName: "shell"}})
	if detail, page := activeTools(); detail != "shell" || page != "shell" {
		t.Fatalf("running active tool: detail=%q page=%q", detail, page)
	}

	result := zotigosession.DisplayItem{ID: "result", Sequence: 4, Type: zotigosession.DisplayItemAssistantMessage, Content: []zotigosession.DisplayContentPart{{Type: "tool_result", ToolResult: &zotigosession.DisplayToolResult{ToolCallID: callID, Text: "done"}}}}
	items.Mu.Lock()
	items.Items[session.ID] = append(log, result)
	items.Mu.Unlock()
	retirement := sessionlog.PreviewRetirement(result)
	deliver(wire.Message{Type: wire.MessageDisplayWake, PreviewRetirement: &retirement})
	if detail, page := activeTools(); detail != "" || page != "" {
		t.Fatalf("finished active tool: detail=%q page=%q", detail, page)
	}
	if current, _ := registry.Get(session.ID); !current.Working {
		t.Fatalf("tool result ended turn activity: %#v", current)
	}
}
