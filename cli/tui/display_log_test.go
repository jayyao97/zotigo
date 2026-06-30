package tui

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/tools"
)

func TestAppendTurnPausedWritesPausedNotCompleted(t *testing.T) {
	model, _ := newDisplayLogTestModel(t)

	model.appendTurnPaused()

	got := loadDisplayLog(t, model)
	if len(got) != 1 {
		t.Fatalf("expected 1 display item, got %d", len(got))
	}
	if got[0].Type != session.DisplayItemTurnPaused {
		t.Fatalf("expected turn_paused, got %s", got[0].Type)
	}
	if got[0].Turn == nil || got[0].Turn.Reason != "need_approval" {
		t.Fatalf("unexpected turn payload: %#v", got[0].Turn)
	}
}

func TestAppendTurnCompletedWritesAssistantMessageAndCompleted(t *testing.T) {
	model, _ := newDisplayLogTestModel(t)
	model.displayAsstContent = []session.DisplayContentPart{
		{Type: string(protocol.ContentTypeText), Text: "done"},
	}

	model.appendTurnCompleted(protocol.FinishReasonStop)

	got := loadDisplayLog(t, model)
	if len(got) != 2 {
		t.Fatalf("expected 2 display items, got %d", len(got))
	}
	if got[0].Type != session.DisplayItemAssistantMessage {
		t.Fatalf("expected assistant_message, got %s", got[0].Type)
	}
	if got[1].Type != session.DisplayItemTurnCompleted {
		t.Fatalf("expected turn_completed, got %s", got[1].Type)
	}
	if got[1].Turn == nil || got[1].Turn.ProviderFinishReason != string(protocol.FinishReasonStop) {
		t.Fatalf("unexpected turn payload: %#v", got[1].Turn)
	}
	if got[1].Turn.LastAgentMessage != "done" {
		t.Fatalf("expected last agent message %q, got %q", "done", got[1].Turn.LastAgentMessage)
	}
}

func TestAppendTurnCompletedUsesVisibleAssistantContent(t *testing.T) {
	model, _ := newDisplayLogTestModel(t)
	model.displayAsstContent = []session.DisplayContentPart{
		{Type: string(protocol.ContentTypeText), Text: "visible"},
	}

	model.appendTurnCompleted(protocol.FinishReasonStop)

	got := loadDisplayLog(t, model)
	if got[0].Content[0].Text != "visible" {
		t.Fatalf("expected visible assistant content, got %#v", got[0].Content)
	}
	if got[1].Turn.LastAgentMessage != "visible" {
		t.Fatalf("expected visible last agent message, got %q", got[1].Turn.LastAgentMessage)
	}
}

func TestAppendTurnPausedFlushesVisibleAssistantContent(t *testing.T) {
	model, _ := newDisplayLogTestModel(t)
	model.appendToolCallDisplayPart(&protocol.ToolCall{
		ID:        "call-1",
		Name:      "shell",
		Arguments: `{"command":"git status"}`,
	})

	model.appendTurnPaused()

	got := loadDisplayLog(t, model)
	if len(got) != 2 {
		t.Fatalf("expected assistant message plus turn_paused, got %d", len(got))
	}
	if got[0].Type != session.DisplayItemAssistantMessage {
		t.Fatalf("expected assistant_message, got %s", got[0].Type)
	}
	if got[0].Content[0].ToolCall == nil {
		t.Fatalf("expected structured tool call, got %#v", got[0].Content[0])
	}
	if got[0].Content[0].ToolCall.ID != "call-1" || got[0].Content[0].ToolCall.Name != "shell" {
		t.Fatalf("unexpected structured tool call: %#v", got[0].Content[0].ToolCall)
	}
	if got[1].Type != session.DisplayItemTurnPaused {
		t.Fatalf("expected turn_paused, got %s", got[1].Type)
	}
}

func TestAppendTurnInterruptedClosesPausedTurn(t *testing.T) {
	model, _ := newDisplayLogTestModel(t)
	model.appendTurnPaused()

	model.appendDeniedToolResults([]protocol.ToolResult{{
		ToolCallID: "call-1",
		ToolName:   "shell",
		Type:       protocol.ToolResultTypeExecutionDenied,
		Reason:     "User denied",
		IsError:    true,
	}})
	model.appendTurnInterrupted("User denied")

	got := loadDisplayLog(t, model)
	if got[len(got)-1].Type != session.DisplayItemTurnInterrupted {
		t.Fatalf("expected turn_interrupted, got %s", got[len(got)-1].Type)
	}
	if lastOpenDisplayTurnID(got) != "" {
		t.Fatalf("expected no open turn after interrupt, got %q", lastOpenDisplayTurnID(got))
	}
	if got[len(got)-2].Type != session.DisplayItemAssistantMessage {
		t.Fatalf("expected denied tool result assistant message, got %s", got[len(got)-2].Type)
	}
	if got[len(got)-2].Content[0].Type != string(protocol.ContentTypeToolResult) {
		t.Fatalf("expected tool_result content, got %#v", got[len(got)-2].Content)
	}
	if got[len(got)-2].Content[0].ToolResult == nil {
		t.Fatalf("expected structured tool result, got %#v", got[len(got)-2].Content[0])
	}
	if got[len(got)-2].Content[0].ToolResult.ToolCallID != "call-1" {
		t.Fatalf("expected tool call ID %q, got %#v", "call-1", got[len(got)-2].Content[0].ToolResult)
	}
	if got[len(got)-2].Content[0].ToolResult.ResultType != string(protocol.ToolResultTypeExecutionDenied) {
		t.Fatalf("expected execution-denied result type, got %#v", got[len(got)-2].Content[0].ToolResult)
	}
	if text := toolResultTextFromDisplay(got[len(got)-2].Content[0].ToolResult, defaultToolResultMaxLines); text != "Denied: User denied" {
		t.Fatalf("expected denied text from structured result, got %q", text)
	}
}

func TestAppendToolResultDisplayPartPreservesStructuredContent(t *testing.T) {
	model, _ := newDisplayLogTestModel(t)

	model.appendToolResultDisplayPart(&protocol.ToolResult{
		ToolCallID: "call-1",
		ToolName:   "screenshot",
		Type:       protocol.ToolResultTypeContent,
		Content: []protocol.ToolResultContentPart{{
			Type: protocol.ContentTypeText,
			Text: "captured",
		}, {
			Type: protocol.ContentTypeImage,
			Image: &protocol.MediaPart{
				URL:       "file:///tmp/screenshot.png",
				MediaType: "image/png",
			},
		}},
	})
	model.appendTurnCompleted(protocol.FinishReasonStop)

	got := loadDisplayLog(t, model)
	if len(got) == 0 || len(got[0].Content) != 1 {
		t.Fatalf("expected assistant message with tool result, got %#v", got)
	}
	result := got[0].Content[0].ToolResult
	if result == nil {
		t.Fatalf("expected structured tool result, got %#v", got[0].Content[0])
	}
	if result.ResultType != string(protocol.ToolResultTypeContent) {
		t.Fatalf("expected content result type, got %#v", result)
	}
	if len(result.Content) != 2 {
		t.Fatalf("expected structured content parts, got %#v", result.Content)
	}
	if result.Content[0].Type != string(protocol.ContentTypeText) || result.Content[0].Text != "captured" {
		t.Fatalf("unexpected text content part: %#v", result.Content[0])
	}
	if result.Content[1].Image == nil || result.Content[1].Image.URL != "file:///tmp/screenshot.png" {
		t.Fatalf("unexpected image content part: %#v", result.Content[1])
	}
}

func TestDenyAndReturnWritesInterruptedTurn(t *testing.T) {
	model, _ := newDisplayLogTestModel(t)
	model.agent = newDisplayLogTestAgent(t)
	model.agent.Restore(agent.Snapshot{
		State: agent.StatePaused,
		PendingActions: []*agent.PendingAction{{
			ToolCallID: "call-1",
			Name:       "shell",
		}},
	})
	model.pendingToolName = "Shell(git status)"
	model.appendTurnPaused()

	updated, _ := model.denyAndReturn("")
	model = updated

	got := loadDisplayLog(t, model)
	if got[len(got)-1].Type != session.DisplayItemTurnInterrupted {
		t.Fatalf("expected turn_interrupted, got %s", got[len(got)-1].Type)
	}
	if lastOpenDisplayTurnID(got) != "" {
		t.Fatalf("expected deny to close open turn, got %q", lastOpenDisplayTurnID(got))
	}
}

func TestAppendTurnFailedFlushesVisibleAssistantContent(t *testing.T) {
	model, _ := newDisplayLogTestModel(t)
	model.displayAsstContent = []session.DisplayContentPart{
		{Type: string(protocol.ContentTypeText), Text: "visible before error"},
	}

	model.appendTurnFailed(errors.New("boom"))

	got := loadDisplayLog(t, model)
	if len(got) != 3 {
		t.Fatalf("expected assistant, error, and turn_failed, got %d", len(got))
	}
	if got[0].Type != session.DisplayItemAssistantMessage {
		t.Fatalf("expected assistant_message, got %s", got[0].Type)
	}
	if got[0].Content[0].Text != "visible before error" {
		t.Fatalf("expected visible assistant content, got %#v", got[0].Content)
	}
	if got[1].Type != session.DisplayItemError {
		t.Fatalf("expected error item, got %s", got[1].Type)
	}
	if got[2].Type != session.DisplayItemTurnFailed {
		t.Fatalf("expected turn_failed, got %s", got[2].Type)
	}
	if lastOpenDisplayTurnID(got) != "" {
		t.Fatalf("expected failed turn to be closed, got %q", lastOpenDisplayTurnID(got))
	}
}

func TestUpdateErrorEventWritesFailedTurn(t *testing.T) {
	model, _ := newDisplayLogTestModel(t)
	model.agent = newDisplayLogTestAgent(t)
	model.thinking = true
	model.displayAsstContent = []session.DisplayContentPart{
		{Type: string(protocol.ContentTypeText), Text: "visible before event error"},
	}

	updated, _ := model.Update(protocol.NewErrorEvent(errors.New("event boom")))
	model = updated.(*Model)

	got := loadDisplayLog(t, model)
	if got[len(got)-1].Type != session.DisplayItemTurnFailed {
		t.Fatalf("expected turn_failed, got %s", got[len(got)-1].Type)
	}
	if got[0].Content[0].Text != "visible before event error" {
		t.Fatalf("expected visible assistant content, got %#v", got[0].Content)
	}
}

func TestAppendContextCompactedDoesNotRewriteDisplayLog(t *testing.T) {
	model, sess := newDisplayLogTestModel(t)
	sess.AgentSnapshot = agent.Snapshot{
		History: []protocol.Message{
			protocol.NewUserMessage("old prompt"),
			protocol.NewAssistantMessage("old answer"),
		},
	}
	if err := model.sessionMgr.Save(sess); err != nil {
		t.Fatalf("save session: %v", err)
	}
	if _, err := model.sessionMgr.AppendDisplayItem(sess.ID, session.DisplayItem{
		Type:    session.DisplayItemUserMessage,
		Role:    string(protocol.RoleUser),
		Content: []session.DisplayContentPart{{Type: string(protocol.ContentTypeText), Text: "old prompt"}},
	}); err != nil {
		t.Fatalf("append display item: %v", err)
	}
	snap := agent.Snapshot{
		History: []protocol.Message{
			protocol.NewUserMessage("[Previous conversation summary]\nold prompt and answer"),
		},
	}

	if contextCompacted(sess, snap) {
		if _, err := model.sessionMgr.AppendDisplayItem(sess.ID, session.DisplayItem{Type: session.DisplayItemContextCompacted}); err != nil {
			t.Fatalf("append compacted item: %v", err)
		}
	}

	items := loadDisplayLog(t, model)
	if len(items) != 2 {
		t.Fatalf("expected old display item plus compacted event, got %d", len(items))
	}
	if items[0].Content[0].Text != "old prompt" {
		t.Fatalf("expected original display item to remain, got %#v", items[0])
	}
	if items[1].Type != session.DisplayItemContextCompacted {
		t.Fatalf("expected context_compacted, got %s", items[1].Type)
	}
}

func TestInitialDisplayItemsReadsDisplayLogNotRuntimeHistory(t *testing.T) {
	model, sess := newDisplayLogTestModel(t)
	sess.AgentSnapshot = agent.Snapshot{
		History: []protocol.Message{
			protocol.NewUserMessage("runtime prompt"),
			protocol.NewAssistantMessage("runtime answer"),
		},
	}
	if err := model.sessionMgr.Save(sess); err != nil {
		t.Fatalf("save session: %v", err)
	}
	if _, err := model.sessionMgr.AppendDisplayItem(sess.ID, session.DisplayItem{
		Type:    session.DisplayItemUserMessage,
		Role:    string(protocol.RoleUser),
		Content: []session.DisplayContentPart{{Type: string(protocol.ContentTypeText), Text: "display prompt"}},
	}); err != nil {
		t.Fatalf("append display item: %v", err)
	}

	items, truncated := model.initialDisplayItems()
	if truncated {
		t.Fatal("expected display history not to be truncated")
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 display item, got %d", len(items))
	}
	if items[0].Content[0].Text != "display prompt" {
		t.Fatalf("expected display log content, got %#v", items[0].Content)
	}
}

func TestInitialDisplayItemsEmptyWithoutDisplayLog(t *testing.T) {
	model, sess := newDisplayLogTestModel(t)
	sess.AgentSnapshot = agent.Snapshot{
		History: []protocol.Message{
			protocol.NewUserMessage("runtime prompt"),
			protocol.NewAssistantMessage("runtime answer"),
		},
	}
	if err := model.sessionMgr.Save(sess); err != nil {
		t.Fatalf("save session: %v", err)
	}

	items, truncated := model.initialDisplayItems()
	if truncated {
		t.Fatal("expected empty display history not to be truncated")
	}
	if len(items) != 0 {
		t.Fatalf("expected no display items, got %d", len(items))
	}
}

func TestRestoreOpenDisplayTurnIDUsesPausedTurn(t *testing.T) {
	model, sess := newDisplayLogTestModel(t)
	model.displayTurnID = ""
	if _, err := model.sessionMgr.AppendDisplayItem(sess.ID, session.DisplayItem{
		Type: session.DisplayItemTurnStarted,
		Turn: &session.DisplayTurn{ID: "turn-open"},
	}); err != nil {
		t.Fatalf("append turn_started: %v", err)
	}
	if _, err := model.sessionMgr.AppendDisplayItem(sess.ID, session.DisplayItem{
		Type: session.DisplayItemTurnPaused,
		Turn: &session.DisplayTurn{ID: "turn-open", Reason: "need_approval"},
	}); err != nil {
		t.Fatalf("append turn_paused: %v", err)
	}

	model.restoreOpenDisplayTurnID()

	if model.displayTurnID != "turn-open" {
		t.Fatalf("expected open turn ID %q, got %q", "turn-open", model.displayTurnID)
	}
}

func TestRestoreOpenDisplayTurnIDIgnoresClosedTurn(t *testing.T) {
	model, sess := newDisplayLogTestModel(t)
	model.displayTurnID = ""
	if _, err := model.sessionMgr.AppendDisplayItem(sess.ID, session.DisplayItem{
		Type: session.DisplayItemTurnStarted,
		Turn: &session.DisplayTurn{ID: "turn-closed"},
	}); err != nil {
		t.Fatalf("append turn_started: %v", err)
	}
	if _, err := model.sessionMgr.AppendDisplayItem(sess.ID, session.DisplayItem{
		Type: session.DisplayItemTurnCompleted,
		Turn: &session.DisplayTurn{ID: "turn-closed", Status: "completed"},
	}); err != nil {
		t.Fatalf("append turn_completed: %v", err)
	}

	model.restoreOpenDisplayTurnID()

	if model.displayTurnID != "" {
		t.Fatalf("expected no open turn ID, got %q", model.displayTurnID)
	}
}

func newDisplayLogTestModel(t *testing.T) (*Model, *session.Session) {
	t.Helper()

	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	manager := session.NewManagerWithStore(store)
	sess, err := manager.CreateNew(t.TempDir())
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	model := Model{
		sessionMgr:    manager,
		sessionID:     sess.ID,
		turnStartTime: time.Now().Add(-2 * time.Second),
		displayTurnID: "turn-test",
	}
	return &model, sess
}

func newDisplayLogTestAgent(t *testing.T) *agent.Agent {
	t.Helper()

	const providerName = "tui-display-log-noop"
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) {
		return noopDisplayLogProvider{}, nil
	})
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	ag, err := agent.New(config.ProfileConfig{Provider: providerName}, exec)
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return ag
}

type noopDisplayLogProvider struct{}

func (noopDisplayLogProvider) Name() string { return "tui-display-log-noop" }

func (noopDisplayLogProvider) StreamChat(context.Context, []protocol.Message, []tools.Tool, ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event)
	close(ch)
	return ch, nil
}

func loadDisplayLog(t *testing.T, model *Model) []session.DisplayItem {
	t.Helper()

	items, ok, err := model.sessionMgr.ListDisplayItems(model.sessionID)
	if err != nil {
		t.Fatalf("list display items: %v", err)
	}
	if !ok {
		t.Fatal("expected session to exist")
	}
	return items
}
