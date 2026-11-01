package tui

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/jayyao97/zotigo/cli/commands"
	cmdbuiltin "github.com/jayyao97/zotigo/cli/commands/builtin"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigodclient"
)

type fakeService struct {
	mu        sync.Mutex
	session   zotigodclient.Session
	messages  []zotigodclient.MessageRequest
	pauses    []string
	decisions map[string][]zotigodclient.ApprovalDecision
	stops     map[string]bool
	policies  []string
	created   []zotigodclient.CreateSessionRequest
	decideErr error
	// items, when non-nil, simulates the durable log for Items and Pause.
	items []session.DisplayItem
	// onDecide runs before a decision is recorded; a non-nil error is
	// returned instead (for example a lost race with another client).
	onDecide func(f *fakeService, approvalID string) error
	// submitErr, when set, is returned after the input was recorded (a
	// lost response) or instead of recording it (a rejection).
	submitErr      error
	rejectedSubmit bool
}

func (f *fakeService) appendItem(item session.DisplayItem) {
	item.Sequence = uint64(len(f.items) + 1)
	f.items = append(f.items, item)
}

func (f *fakeService) Items(context.Context, string, uint64, uint64, int) (zotigodclient.ItemsPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return zotigodclient.ItemsPage{Items: append([]session.DisplayItem(nil), f.items...)}, nil
}

func (f *fakeService) recorded(id string) bool {
	for _, item := range f.items {
		if item.ID == id {
			return true
		}
	}
	return false
}

func (f *fakeService) openTurn() string {
	open := ""
	for _, item := range f.items {
		switch item.Type {
		case session.DisplayItemTurnStarted:
			open = item.Turn.ID
		case session.DisplayItemTurnCompleted, session.DisplayItemTurnInterrupted, session.DisplayItemTurnFailed:
			open = ""
		}
	}
	return open
}

func newFakeService(sess zotigodclient.Session) *fakeService {
	return &fakeService{session: sess, decisions: map[string][]zotigodclient.ApprovalDecision{}, stops: map[string]bool{}}
}

func (f *fakeService) CreateSession(_ context.Context, req zotigodclient.CreateSessionRequest) (zotigodclient.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, req)
	return zotigodclient.Session{ID: "sess_new", WorkingDirectory: req.WorkingDirectory, ApprovalPolicy: "auto"}, nil
}

func (f *fakeService) GetSession(context.Context, string) (zotigodclient.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.session, nil
}

func (f *fakeService) SubmitMessage(_ context.Context, _ string, req zotigodclient.MessageRequest) (zotigodclient.Command, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, req)
	if f.rejectedSubmit {
		return zotigodclient.Command{}, f.submitErr
	}
	if f.submitErr != nil {
		// Lost response: the service recorded the input and started its
		// turn once; resubmissions are deduplicated.
		if f.items != nil && !f.recorded(req.ClientMessageID) {
			f.appendItem(session.DisplayItem{ID: req.ClientMessageID, Type: session.DisplayItemUserMessage})
			f.appendItem(session.DisplayItem{Type: session.DisplayItemTurnStarted, Turn: &session.DisplayTurn{ID: "turn_lost"}})
		}
		return zotigodclient.Command{}, f.submitErr
	}
	if f.items != nil {
		f.appendItem(session.DisplayItem{ID: req.ClientMessageID, Type: session.DisplayItemUserMessage})
	}
	return zotigodclient.Command{ID: req.ClientMessageID, Type: "message"}, nil
}

func (f *fakeService) Pause(_ context.Context, _ string, turnID string) (zotigodclient.Command, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.items != nil {
		open := f.openTurn()
		if open == "" {
			return zotigodclient.Command{}, &zotigodclient.APIError{Status: 409, Code: "no_active_turn"}
		}
		if turnID != "" && turnID != open {
			return zotigodclient.Command{}, &zotigodclient.APIError{Status: 409, Code: "turn_mismatch"}
		}
		f.appendItem(session.DisplayItem{Type: session.DisplayItemTurnInterrupted, Turn: &session.DisplayTurn{ID: open}})
	}
	f.pauses = append(f.pauses, turnID)
	return zotigodclient.Command{Type: "pause", TurnID: turnID}, nil
}

func (f *fakeService) DecideApproval(_ context.Context, _ string, approvalID string, decisions []zotigodclient.ApprovalDecision, stopTurn bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.decideErr != nil {
		return f.decideErr
	}
	if f.onDecide != nil {
		if err := f.onDecide(f, approvalID); err != nil {
			return err
		}
	}
	f.decisions[approvalID] = decisions
	f.stops[approvalID] = stopTurn
	return nil
}

func (f *fakeService) SetApprovalPolicy(_ context.Context, _ string, policy string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.policies = append(f.policies, policy)
	f.session.ApprovalPolicy = policy
	return nil
}

func (f *fakeService) SetProfile(context.Context, string, string) error { return nil }

func (f *fakeService) Profiles(context.Context, string) (zotigodclient.Profiles, error) {
	return zotigodclient.Profiles{}, nil
}

func (f *fakeService) Skills(context.Context, string, bool) ([]zotigodclient.Skill, error) {
	return nil, nil
}

func (f *fakeService) Subscribe(context.Context, string, uint64, zotigodclient.StreamOptions) <-chan zotigodclient.Event {
	return make(chan zotigodclient.Event)
}

func newTestModel(t *testing.T, service *fakeService, history []session.DisplayItem) *Model {
	t.Helper()
	registry := commands.NewRegistry()
	cmdbuiltin.RegisterAll(registry)
	model := NewModel(ModelOptions{
		Service:  service,
		Session:  service.session,
		Registry: registry,
		History:  history,
		Location: "Directory",
	})
	model.viewportEnabled = true
	t.Cleanup(model.Close)
	return model
}

// runCmd executes a command tree and returns committed transcript lines and
// any other messages, without blocking on the live event stream.
func runCmd(t *testing.T, cmd tea.Cmd) ([]string, []tea.Msg) {
	t.Helper()
	var lines []string
	var others []tea.Msg
	var visit func(tea.Cmd)
	visit = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		msg := cmd()
		if msg == nil {
			return
		}
		if line, ok := msg.(transcriptMsg); ok {
			lines = append(lines, string(line))
			return
		}
		value := reflect.ValueOf(msg)
		if value.Kind() == reflect.Slice && value.Type().Elem() == reflect.TypeOf(tea.Cmd(nil)) {
			for index := 0; index < value.Len(); index++ {
				visit(value.Index(index).Interface().(tea.Cmd))
			}
			return
		}
		others = append(others, msg)
	}
	visit(cmd)
	return lines, others
}

func item(seq uint64, itemType session.DisplayItemType) session.DisplayItem {
	return session.DisplayItem{ID: "item_" + string(rune('a'+seq)), Sequence: seq, Type: itemType}
}

func TestRenderSessionBanner(t *testing.T) {
	banner := renderSessionBanner(zotigodclient.Session{
		ID:               "sess_1",
		WorkingDirectory: "/repo/code/backend",
		Profile:          "fast",
		ApprovalPolicy:   "bypass_permissions",
	}, "Workspace Main", true)
	for _, want := range []string{"sess_1", "/repo/code/backend", "Workspace Main", "fast", "Full access", "--no-daemon"} {
		if !strings.Contains(banner, want) {
			t.Fatalf("banner missing %q:\n%s", want, banner)
		}
	}
}

func TestWriteInputFooterWarnsWhenFullAccess(t *testing.T) {
	ta := textarea.New()
	ta.SetWidth(80)
	model := &Model{input: ta, approvalPolicy: "bypass_permissions"}
	var output strings.Builder
	model.writeInputFooter(&output)
	if !strings.Contains(output.String(), "FULL ACCESS") {
		t.Fatalf("expected full access warning, got %q", output.String())
	}
}

func TestDurableAssistantItemDoesNotRepeatStreamedText(t *testing.T) {
	service := newFakeService(zotigodclient.Session{ID: "sess_1", Live: true})
	model := newTestModel(t, service, nil)

	var lines []string
	collect := func(cmd tea.Cmd) {
		got, _ := runCmd(t, cmd)
		lines = append(lines, got...)
	}
	collect(model.handleServiceEvent(zotigodclient.Event{Delta: &zotigodclient.Delta{ItemID: "blk", PartType: "text", Delta: "Hello wor"}}))
	assistant := session.DisplayItem{ID: "blk", Sequence: 3, Type: session.DisplayItemAssistantMessage,
		Content: []session.DisplayContentPart{{Type: "text", Text: "Hello world\nsecond line"}}}
	collect(model.handleServiceEvent(zotigodclient.Event{Item: &assistant}))

	joined := strings.Join(lines, "\n")
	if strings.Count(joined, "Hello wor") != 1 {
		t.Fatalf("streamed text repeated or missing:\n%s", joined)
	}
	if !strings.Contains(joined, "Hello world") || !strings.Contains(joined, "second line") {
		t.Fatalf("durable remainder missing:\n%s", joined)
	}
	if model.currentAsstMsg != "" || model.cursor != 3 {
		t.Fatalf("live block = %q cursor = %d", model.currentAsstMsg, model.cursor)
	}
}

func TestDurableAssistantItemWithoutDeltasIsRendered(t *testing.T) {
	model := newTestModel(t, newFakeService(zotigodclient.Session{ID: "sess_1"}), nil)
	assistant := session.DisplayItem{ID: "blk", Sequence: 1, Type: session.DisplayItemAssistantMessage,
		Content: []session.DisplayContentPart{
			{Type: "text", Text: "dropped preview"},
			{Type: "tool_call", ToolCall: &session.DisplayToolCall{ID: "c1", Name: "shell", Arguments: `{"command":"ls"}`}},
		}}
	lines, _ := runCmd(t, model.handleServiceEvent(zotigodclient.Event{Item: &assistant}))
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "dropped preview") || !strings.Contains(joined, "Shell(ls)") {
		t.Fatalf("durable content not rendered:\n%s", joined)
	}
}

func TestOwnInputIsRenderedOnceAndForeignInputIsShown(t *testing.T) {
	service := newFakeService(zotigodclient.Session{ID: "sess_1", Live: true})
	model := newTestModel(t, service, nil)

	lines, _ := runCmd(t, model.submitInput("run tests"))
	if len(service.messages) != 1 || service.messages[0].Text != "run tests" {
		t.Fatalf("messages = %+v", service.messages)
	}
	if !model.thinking || strings.Count(strings.Join(lines, "\n"), "run tests") != 1 {
		t.Fatalf("submit render = %q thinking=%v", lines, model.thinking)
	}
	own := session.DisplayItem{ID: service.messages[0].ClientMessageID, Sequence: 1, Type: session.DisplayItemUserMessage,
		Content: []session.DisplayContentPart{{Type: "text", Text: "run tests"}}}
	if lines, _ := runCmd(t, model.handleServiceEvent(zotigodclient.Event{Item: &own})); len(lines) != 0 {
		t.Fatalf("own durable input rendered again: %q", lines)
	}
	foreign := session.DisplayItem{ID: "item_desktop", Sequence: 2, Type: session.DisplayItemSteeringMessage,
		Content: []session.DisplayContentPart{{Type: "text", Text: "from desktop"}}}
	lines, _ = runCmd(t, model.handleServiceEvent(zotigodclient.Event{Item: &foreign}))
	if !strings.Contains(strings.Join(lines, "\n"), "from desktop") {
		t.Fatalf("foreign input not rendered: %q", lines)
	}
}

func approvalItems(turnID string) (session.DisplayItem, session.DisplayItem) {
	started := item(1, session.DisplayItemTurnStarted)
	started.Turn = &session.DisplayTurn{ID: turnID}
	request := item(2, session.DisplayItemApprovalRequest)
	request.Approval = &session.DisplayApproval{ID: "apr_1", TurnID: turnID, Pending: []session.DisplayPendingApproval{
		{ToolCallID: "c1", ToolName: "shell", Arguments: `{"command":"rm -rf build"}`, Description: "deletes files"},
		{ToolCallID: "c2", ToolName: "write_file", Arguments: `{"path":"a.txt"}`},
	}}
	return started, request
}

func TestApprovalBatchAcceptsThenDeniesRemaining(t *testing.T) {
	service := newFakeService(zotigodclient.Session{ID: "sess_1", Live: true})
	model := newTestModel(t, service, nil)
	started, request := approvalItems("turn_1")
	runCmd(t, model.handleServiceEvent(zotigodclient.Event{Item: &started}))
	runCmd(t, model.handleServiceEvent(zotigodclient.Event{Item: &request}))
	if !model.approving || model.approval == nil || model.approval.ID != "apr_1" {
		t.Fatalf("approval UI not shown: %+v", model.approval)
	}

	_, cmd := model.acceptApproval()
	if cmd != nil || model.approvalItemChoice != 1 {
		t.Fatalf("first accept should advance to the second call")
	}
	_, cmd = model.denyApproval("")
	runCmd(t, cmd)
	got := service.decisions["apr_1"]
	if len(got) != 2 || !got[0].Approved || got[1].Approved || got[1].Reason == "" {
		t.Fatalf("decisions = %+v", got)
	}
	if service.stops["apr_1"] {
		t.Fatal("a partially approved batch cannot stop the turn")
	}

	decision := item(3, session.DisplayItemApprovalDecision)
	decision.Approval = &session.DisplayApproval{ID: "apr_1", Decisions: []session.DisplayApprovalDecision{{ToolCallID: "c1", Approved: true}, {ToolCallID: "c2"}}}
	lines, _ := runCmd(t, model.handleServiceEvent(zotigodclient.Event{Item: &decision}))
	if model.approving || strings.Contains(strings.Join(lines, "\n"), "another client") {
		t.Fatalf("own decision handling: approving=%v lines=%q", model.approving, lines)
	}
}

func TestPlainDenyStopsTurnWithDenial(t *testing.T) {
	service := newFakeService(zotigodclient.Session{ID: "sess_1", Live: true})
	model := newTestModel(t, service, nil)
	started, request := approvalItems("turn_1")
	runCmd(t, model.handleServiceEvent(zotigodclient.Event{Item: &started}))
	runCmd(t, model.handleServiceEvent(zotigodclient.Event{Item: &request}))
	_, cmd := model.denyApproval("")
	runCmd(t, cmd)
	if got := service.decisions["apr_1"]; len(got) != 2 || got[0].Approved || got[1].Approved || !service.stops["apr_1"] {
		t.Fatalf("decisions=%+v stop=%v", got, service.stops["apr_1"])
	}
	if len(service.pauses) != 0 {
		t.Fatalf("the stop travels with the decision, not a separate pause: %+v", service.pauses)
	}
}

func TestDenyWithFeedbackLetsAgentContinue(t *testing.T) {
	service := newFakeService(zotigodclient.Session{ID: "sess_1", Live: true})
	model := newTestModel(t, service, nil)
	started, request := approvalItems("turn_1")
	runCmd(t, model.handleServiceEvent(zotigodclient.Event{Item: &started}))
	runCmd(t, model.handleServiceEvent(zotigodclient.Event{Item: &request}))
	_, cmd := model.denyApproval("use git clean instead")
	runCmd(t, cmd)
	if got := service.decisions["apr_1"]; len(got) != 2 || got[0].Reason != "use git clean instead" || service.stops["apr_1"] {
		t.Fatalf("decisions=%+v stop=%v", got, service.stops["apr_1"])
	}
}

func TestApprovalDecidedElsewhereClosesPrompt(t *testing.T) {
	service := newFakeService(zotigodclient.Session{ID: "sess_1", Live: true})
	model := newTestModel(t, service, nil)
	started, request := approvalItems("turn_1")
	runCmd(t, model.handleServiceEvent(zotigodclient.Event{Item: &started}))
	runCmd(t, model.handleServiceEvent(zotigodclient.Event{Item: &request}))

	decision := item(3, session.DisplayItemApprovalDecision)
	decision.Approval = &session.DisplayApproval{ID: "apr_1", Decisions: []session.DisplayApprovalDecision{{ToolCallID: "c1", Approved: true}, {ToolCallID: "c2", Approved: true}}}
	lines, _ := runCmd(t, model.handleServiceEvent(zotigodclient.Event{Item: &decision}))
	if model.approving {
		t.Fatal("approval prompt still open after another client decided")
	}
	if !strings.Contains(strings.Join(lines, "\n"), "another client") {
		t.Fatalf("missing attribution: %q", lines)
	}
	if len(service.decisions) != 0 {
		t.Fatalf("TUI must not decide again: %+v", service.decisions)
	}
}

func TestPendingApprovalRestoredOnlyForLiveSession(t *testing.T) {
	started, request := approvalItems("turn_1")
	live := newTestModel(t, newFakeService(zotigodclient.Session{ID: "sess_1", Live: true}), []session.DisplayItem{started, request})
	if !live.approving || !live.thinking || live.openTurnID != "turn_1" {
		t.Fatalf("live resume state approving=%v thinking=%v turn=%q", live.approving, live.thinking, live.openTurnID)
	}
	offline := newTestModel(t, newFakeService(zotigodclient.Session{ID: "sess_1", Live: false}), []session.DisplayItem{started, request})
	if offline.approving {
		t.Fatal("offline session approval is resolved by the next worker, not the TUI")
	}
}

func TestTurnCompletionAccumulatesUsageAndEndsTurn(t *testing.T) {
	model := newTestModel(t, newFakeService(zotigodclient.Session{ID: "sess_1", Live: true}), nil)
	started := item(1, session.DisplayItemTurnStarted)
	started.Turn = &session.DisplayTurn{ID: "turn_1"}
	runCmd(t, model.handleServiceEvent(zotigodclient.Event{Item: &started}))
	completed := item(2, session.DisplayItemTurnCompleted)
	completed.Turn = &session.DisplayTurn{ID: "turn_1", Usage: &protocol.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}}
	lines, msgs := runCmd(t, model.handleServiceEvent(zotigodclient.Event{Item: &completed}))
	if model.thinking || model.openTurnID != "" || model.turns != 1 || model.sessionUsage.OutputTokens != 5 {
		t.Fatalf("turn state thinking=%v turn=%q turns=%d usage=%+v", model.thinking, model.openTurnID, model.turns, model.sessionUsage)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "Completed in") {
		t.Fatalf("missing completion line: %q", lines)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected a session refresh, got %#v", msgs)
	}
}

func TestEscPausesRunningTurn(t *testing.T) {
	service := newFakeService(zotigodclient.Session{ID: "sess_1", Live: true})
	model := newTestModel(t, service, nil)
	started := item(1, session.DisplayItemTurnStarted)
	started.Turn = &session.DisplayTurn{ID: "turn_9"}
	runCmd(t, model.handleServiceEvent(zotigodclient.Event{Item: &started}))
	_, cmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	runCmd(t, cmd)
	if len(service.pauses) != 1 || service.pauses[0] != "turn_9" {
		t.Fatalf("pauses = %+v", service.pauses)
	}
}

func TestClearStartsNewSessionInSameDirectory(t *testing.T) {
	service := newFakeService(zotigodclient.Session{ID: "sess_1", Live: true, WorkingDirectory: "/repo/sub", ApprovalPolicy: "auto"})
	model := newTestModel(t, service, nil)
	model.input.SetValue("/clear")
	_, cmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if model.session.ID != "sess_new" {
		t.Fatalf("session = %q", model.session.ID)
	}
	if len(service.created) != 1 || service.created[0].WorkingDirectory != "/repo/sub" {
		t.Fatalf("created = %+v", service.created)
	}
	if cmd == nil {
		t.Fatal("expected banner and stream commands")
	}
}

func TestBuildMessageRequestInlinesReadableImages(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/shot.png"
	if err := writeFile(path, []byte("png-bytes")); err != nil {
		t.Fatal(err)
	}
	req := buildMessageRequest("look at @" + path + " and @missing.png")
	if len(req.Images) != 1 || req.Images[0].MimeType != "image/png" {
		t.Fatalf("images = %+v", req.Images)
	}
	if strings.Contains(req.Text, path) || !strings.Contains(req.Text, "@missing.png") {
		t.Fatalf("text = %q", req.Text)
	}
	if req.ClientMessageID == "" {
		t.Fatal("client message id is required for dedupe")
	}
}

func TestPasteMsgInsertsMultilineTextOnce(t *testing.T) {
	ta := textarea.New()
	ta.Focus()
	ta.Prompt = ""
	ta.SetWidth(80)
	ta.SetHeight(1)

	m := &Model{input: ta}
	pasted := "first line\nsecond line\nthird line"

	updated, _ := m.Update(tea.PasteMsg{Content: pasted})
	got := updated.(*Model).input.Value()

	if got != pasted {
		t.Fatalf("paste should insert content once, got %q", got)
	}
}

func TestShouldUseViewportRendererDisablesJetBrainsTerminal(t *testing.T) {
	t.Setenv("TERMINAL_EMULATOR", "JetBrains-JediTerm")
	t.Setenv("TERM_PROGRAM", "")

	if shouldUseViewportRenderer() {
		t.Fatal("expected JetBrains terminal to use inline renderer")
	}
}

func TestShouldUseViewportRendererAllowsOtherTerminals(t *testing.T) {
	t.Setenv("TERMINAL_EMULATOR", "")
	t.Setenv("TERM_PROGRAM", "iTerm.app")

	if !shouldUseViewportRenderer() {
		t.Fatal("expected non-JetBrains terminal to use viewport renderer")
	}
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}

// Deltas travel faster than durable items, so a text block can start
// streaming before the previous tool items are durable. Each piece must
// still be committed exactly once.
func TestStreamingAheadOfDurableToolItemsCommitsOnce(t *testing.T) {
	model := newTestModel(t, newFakeService(zotigodclient.Session{ID: "sess_1", Live: true}), nil)
	var lines []string
	collect := func(cmd tea.Cmd) {
		got, _ := runCmd(t, cmd)
		lines = append(lines, got...)
	}
	collect(model.handleServiceEvent(zotigodclient.Event{Delta: &zotigodclient.Delta{ItemID: "text", PartType: "text", Delta: "done after tool"}}))
	call := session.DisplayItem{ID: "call", Sequence: 1, Type: session.DisplayItemAssistantMessage, Content: []session.DisplayContentPart{
		{Type: "tool_call", ToolCall: &session.DisplayToolCall{ID: "c1", Name: "shell", Arguments: `{"command":"pwd"}`}}}}
	result := session.DisplayItem{ID: "result", Sequence: 2, Type: session.DisplayItemAssistantMessage, Content: []session.DisplayContentPart{
		{Type: "tool_result", ToolResult: &session.DisplayToolResult{ToolCallID: "c1", ToolName: "shell", Text: "/repo"}}}}
	text := session.DisplayItem{ID: "text", Sequence: 3, Type: session.DisplayItemAssistantMessage, Content: []session.DisplayContentPart{{Type: "text", Text: "done after tool"}}}
	for _, item := range []session.DisplayItem{call, result, text} {
		collect(model.handleServiceEvent(zotigodclient.Event{Item: &item}))
	}
	joined := strings.Join(lines, "\n")
	if strings.Count(joined, "done after tool") != 1 || strings.Count(joined, "Shell(pwd)") != 1 {
		t.Fatalf("duplicated output:\n%s", joined)
	}
}

func TestShiftTabCyclesManualAutoFullAccess(t *testing.T) {
	for current, want := range map[string]string{
		"manual":             "auto",
		"auto":               "bypass_permissions",
		"":                   "bypass_permissions",
		"bypass_permissions": "manual",
	} {
		if got := nextApprovalPolicy(current); got != want {
			t.Errorf("nextApprovalPolicy(%q) = %q, want %q", current, got, want)
		}
	}
}

func ctrlC() tea.KeyPressMsg { return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl} }

func quits(msgs []tea.Msg) bool {
	for _, msg := range msgs {
		if _, ok := msg.(tea.QuitMsg); ok {
			return true
		}
	}
	return false
}

func startRunningTurn(t *testing.T, model *Model, turnID string) {
	t.Helper()
	started := item(1, session.DisplayItemTurnStarted)
	started.Turn = &session.DisplayTurn{ID: turnID}
	runCmd(t, model.handleServiceEvent(zotigodclient.Event{Item: &started}))
}

func TestCtrlCWithRunningTurnAsksBeforeExit(t *testing.T) {
	service := newFakeService(zotigodclient.Session{ID: "sess_1", Live: true})
	model := newTestModel(t, service, nil)
	startRunningTurn(t, model, "turn_1")

	_, cmd := model.Update(ctrlC())
	if _, msgs := runCmd(t, cmd); quits(msgs) || !model.exitPrompt {
		t.Fatal("ctrl+c with a running daemon turn must ask first")
	}
	if !strings.Contains(model.liveView(), "Keep it running") {
		t.Fatalf("exit prompt not rendered:\n%s", model.liveView())
	}
	// Cancel returns to the session.
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if model.exitPrompt || len(service.pauses) != 0 {
		t.Fatal("cancel must not stop or exit")
	}

	// Stop: pause the turn, then exit.
	model.Update(ctrlC())
	_, cmd = model.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	_, msgs := runCmd(t, cmd)
	if len(service.pauses) != 1 || service.pauses[0] != "turn_1" {
		t.Fatalf("pauses = %+v", service.pauses)
	}
	if len(msgs) != 1 {
		t.Fatalf("msgs = %+v", msgs)
	}
	_, cmd = model.Update(msgs[0])
	if _, msgs := runCmd(t, cmd); !quits(msgs) {
		t.Fatal("stop did not exit")
	}
}

func TestExitPromptBackgroundKeepsTurn(t *testing.T) {
	service := newFakeService(zotigodclient.Session{ID: "sess_1", Live: true})
	model := newTestModel(t, service, nil)
	startRunningTurn(t, model, "turn_1")
	model.Update(ctrlC())
	_, cmd := model.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	lines, msgs := runCmd(t, cmd)
	if !quits(msgs) || len(service.pauses) != 0 {
		t.Fatalf("background exit quit=%v pauses=%v", quits(msgs), service.pauses)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "zotigo --resume") {
		t.Fatalf("missing reattach hint: %v", lines)
	}
}

func TestExitPromptStopDeniesPendingApproval(t *testing.T) {
	service := newFakeService(zotigodclient.Session{ID: "sess_1", Live: true})
	model := newTestModel(t, service, nil)
	started, request := approvalItems("turn_1")
	service.items = []session.DisplayItem{}
	service.appendItem(started)
	service.appendItem(request)
	runCmd(t, model.handleServiceEvent(zotigodclient.Event{Item: &started}))
	runCmd(t, model.handleServiceEvent(zotigodclient.Event{Item: &request}))
	model.Update(ctrlC())
	_, cmd := model.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	runCmd(t, cmd)
	got := service.decisions["apr_1"]
	if len(got) != 2 || got[0].Approved || got[1].Approved || !service.stops["apr_1"] || len(service.pauses) != 0 {
		t.Fatalf("decisions=%+v stop=%v pauses=%v", got, service.stops["apr_1"], service.pauses)
	}
}

func TestCtrlCIdleOrEmbeddedExitsDirectly(t *testing.T) {
	service := newFakeService(zotigodclient.Session{ID: "sess_1", Live: true})
	idle := newTestModel(t, service, nil)
	_, cmd := idle.Update(ctrlC())
	if _, msgs := runCmd(t, cmd); !quits(msgs) {
		t.Fatal("idle ctrl+c must exit")
	}
	embedded := newTestModel(t, service, nil)
	embedded.embedded = true
	startRunningTurn(t, embedded, "turn_1")
	_, cmd = embedded.Update(ctrlC())
	if _, msgs := runCmd(t, cmd); !quits(msgs) || embedded.exitPrompt {
		t.Fatal("--no-daemon ctrl+c exits and stops the turn on shutdown")
	}
}

// Review P1: stop-and-exit before turn_started reached the TUI must still
// stop the turn the submitted input starts.
func TestStopAndExitBeforeTurnStartedStopsInputTurn(t *testing.T) {
	service := newFakeService(zotigodclient.Session{ID: "sess_1", Live: true})
	service.items = []session.DisplayItem{}
	model := newTestModel(t, service, nil)
	model.input.SetValue("long task")
	_, cmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	_, msgs := runCmd(t, cmd)
	for _, msg := range msgs {
		model.Update(msg)
	}
	if !model.thinking || model.openTurnID != "" {
		t.Fatalf("thinking=%v open=%q", model.thinking, model.openTurnID)
	}
	model.Update(ctrlC())
	_, cmd = model.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	// The worker starts the turn while the stop is waiting for it.
	go func() {
		time.Sleep(300 * time.Millisecond)
		service.mu.Lock()
		service.appendItem(session.DisplayItem{Type: session.DisplayItemTurnStarted, Turn: &session.DisplayTurn{ID: "turn_late"}})
		service.mu.Unlock()
	}()
	_, msgs = runCmd(t, cmd)
	var stopped *exitStoppedMsg
	for _, msg := range msgs {
		if value, ok := msg.(exitStoppedMsg); ok {
			stopped = &value
		}
	}
	if stopped == nil || stopped.err != nil || stopped.result != zotigodclient.StopRequested {
		t.Fatalf("stop = %+v", stopped)
	}
	if len(service.pauses) != 1 || service.pauses[0] != "turn_late" {
		t.Fatalf("pauses = %v", service.pauses)
	}
}

// Review P1: losing the approval race to another client (409) is not a
// stop; the possibly approved turn must still be paused.
func TestStopAndExitAfterApprovalRaceStillPauses(t *testing.T) {
	service := newFakeService(zotigodclient.Session{ID: "sess_1", Live: true})
	started, request := approvalItems("turn_1")
	service.items = []session.DisplayItem{}
	service.appendItem(started)
	service.appendItem(request)
	service.onDecide = func(f *fakeService, approvalID string) error {
		// Desktop approved first.
		f.appendItem(session.DisplayItem{Type: session.DisplayItemApprovalDecision, Approval: &session.DisplayApproval{ID: approvalID, TurnID: "turn_1"}})
		f.onDecide = nil
		return &zotigodclient.APIError{Status: 409, Code: "conflict", Message: "approval request already resolved"}
	}
	model := newTestModel(t, service, nil)
	runCmd(t, model.handleServiceEvent(zotigodclient.Event{Item: &started}))
	runCmd(t, model.handleServiceEvent(zotigodclient.Event{Item: &request}))
	model.Update(ctrlC())
	_, cmd := model.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	_, msgs := runCmd(t, cmd)
	var stopped *exitStoppedMsg
	for _, msg := range msgs {
		if value, ok := msg.(exitStoppedMsg); ok {
			stopped = &value
		}
	}
	if stopped == nil || stopped.err != nil || stopped.result != zotigodclient.StopRequested {
		t.Fatalf("stop = %+v", stopped)
	}
	if len(service.pauses) != 1 || service.pauses[0] != "turn_1" {
		t.Fatalf("approved turn was not paused: %v", service.pauses)
	}
}

// A stop that cannot be confirmed keeps the TUI attached.
func TestStopAndExitFailureDoesNotQuit(t *testing.T) {
	service := newFakeService(zotigodclient.Session{ID: "sess_1", Live: true})
	model := newTestModel(t, service, nil)
	_, cmd := model.Update(exitStoppedMsg{err: errors.New("service unavailable")})
	lines, msgs := runCmd(t, cmd)
	if quits(msgs) || !strings.Contains(strings.Join(lines, "\n"), "still attached") {
		t.Fatalf("lines=%v quit=%v", lines, quits(msgs))
	}
}

// Review P2: a delta that arrives after its durable item is not shown again.
func TestLateDeltaAfterDurableItemIsDropped(t *testing.T) {
	model := newTestModel(t, newFakeService(zotigodclient.Session{ID: "sess_1", Live: true}), nil)
	durable := session.DisplayItem{ID: "a1", Sequence: 1, Type: session.DisplayItemAssistantMessage, Content: []session.DisplayContentPart{{Type: "text", Text: "answer"}}}
	lines, _ := runCmd(t, model.handleServiceEvent(zotigodclient.Event{Item: &durable}))
	late, _ := runCmd(t, model.handleServiceEvent(zotigodclient.Event{Delta: &zotigodclient.Delta{ItemID: "a1", PartType: "text", Delta: "answer"}}))
	all := strings.Join(append(lines, late...), "\n") + model.currentAsstMsg
	if strings.Count(all, "answer") != 1 {
		t.Fatalf("answer rendered %d times:\n%s", strings.Count(all, "answer"), all)
	}
}

func TestLateDeltaForRestoredHistoryIsDropped(t *testing.T) {
	history := []session.DisplayItem{{ID: "a1", Sequence: 1, Type: session.DisplayItemAssistantMessage, Content: []session.DisplayContentPart{{Type: "text", Text: "answer"}}}}
	model := newTestModel(t, newFakeService(zotigodclient.Session{ID: "sess_1", Live: true}), history)
	late, _ := runCmd(t, model.handleServiceEvent(zotigodclient.Event{Delta: &zotigodclient.Delta{ItemID: "a1", PartType: "text", Delta: "answer"}}))
	if len(late) != 0 || model.currentAsstMsg != "" {
		t.Fatalf("late delta rendered: %v %q", late, model.currentAsstMsg)
	}
}

// Deltas may also trail into the next turn; they stay dropped.
func TestLateDeltaAcrossTurnsIsDropped(t *testing.T) {
	model := newTestModel(t, newFakeService(zotigodclient.Session{ID: "sess_1", Live: true}), nil)
	durable := session.DisplayItem{ID: "a1", Sequence: 1, Type: session.DisplayItemAssistantMessage, Content: []session.DisplayContentPart{{Type: "text", Text: "answer"}}}
	completed := session.DisplayItem{Sequence: 2, Type: session.DisplayItemTurnCompleted, Turn: &session.DisplayTurn{ID: "t1", Status: "completed"}}
	started := session.DisplayItem{Sequence: 3, Type: session.DisplayItemTurnStarted, Turn: &session.DisplayTurn{ID: "t2"}}
	for _, item := range []session.DisplayItem{durable, completed, started} {
		runCmd(t, model.handleServiceEvent(zotigodclient.Event{Item: &item}))
	}
	late, _ := runCmd(t, model.handleServiceEvent(zotigodclient.Event{Delta: &zotigodclient.Delta{ItemID: "a1", PartType: "text", Delta: "answer"}}))
	if len(late) != 0 || model.currentAsstMsg != "" {
		t.Fatalf("late delta rendered: %v %q", late, model.currentAsstMsg)
	}
}

func submitAndSettle(t *testing.T, model *Model, text string) []string {
	t.Helper()
	model.input.SetValue(text)
	_, cmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	lines, msgs := runCmd(t, cmd)
	for _, msg := range msgs {
		_, next := model.Update(msg)
		more, _ := runCmd(t, next)
		lines = append(lines, more...)
	}
	return lines
}

// Review round 3: a lost response is not a rejection. The input stays a
// stop target, Ctrl+C asks, and "stop" pauses the turn it started.
func TestLostSubmitResponseKeepsInputStoppable(t *testing.T) {
	service := newFakeService(zotigodclient.Session{ID: "sess_1", Live: true})
	service.items = []session.DisplayItem{}
	service.submitErr = io.ErrUnexpectedEOF
	model := newTestModel(t, service, nil)
	lines := submitAndSettle(t, model, "long task")
	if joined := strings.Join(lines, "\n"); strings.Contains(joined, "not accepted") || !strings.Contains(joined, "Could not confirm") {
		t.Fatalf("lines = %v", lines)
	}
	if len(service.messages) < 2 {
		t.Fatalf("unconfirmed submission was not retried: %d attempts", len(service.messages))
	}
	_, cmd := model.Update(ctrlC())
	if !model.exitPrompt || quits(msgsOf(t, cmd)) {
		t.Fatal("Ctrl+C must ask while an unconfirmed input may be running")
	}
	_, cmd = model.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	_, msgs := runCmd(t, cmd)
	var stopped *exitStoppedMsg
	for _, msg := range msgs {
		if value, ok := msg.(exitStoppedMsg); ok {
			stopped = &value
		}
	}
	if stopped == nil || stopped.err != nil || stopped.result != zotigodclient.StopRequested {
		t.Fatalf("stop = %+v", stopped)
	}
	if len(service.pauses) != 1 {
		t.Fatalf("pauses = %v", service.pauses)
	}
}

// A 4xx is a rejection: the input is dropped and Ctrl+C exits directly.
func TestRejectedSubmitIsDropped(t *testing.T) {
	service := newFakeService(zotigodclient.Session{ID: "sess_1", Live: true})
	service.rejectedSubmit = true
	service.submitErr = &zotigodclient.APIError{Status: 409, Code: "turn_stopping", Message: "stopping"}
	model := newTestModel(t, service, nil)
	lines := submitAndSettle(t, model, "hello")
	if !strings.Contains(strings.Join(lines, "\n"), "not accepted") || len(service.messages) != 1 {
		t.Fatalf("lines=%v attempts=%d", lines, len(service.messages))
	}
	_, cmd := model.Update(ctrlC())
	if model.exitPrompt || !quits(msgsOf(t, cmd)) {
		t.Fatal("Ctrl+C should exit when nothing may be running")
	}
}

func msgsOf(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	_, msgs := runCmd(t, cmd)
	return msgs
}

// Review round 4: Esc after an unconfirmed submission must stop the turn
// the input may have started, not exit as if idle.
func TestEscAfterLostSubmitResponseStopsTurn(t *testing.T) {
	service := newFakeService(zotigodclient.Session{ID: "sess_1", Live: true})
	service.items = []session.DisplayItem{}
	service.submitErr = io.ErrUnexpectedEOF
	model := newTestModel(t, service, nil)
	submitAndSettle(t, model, "long task")
	if model.thinking || len(model.pendingInputs) == 0 {
		t.Fatalf("thinking=%v pending=%v", model.thinking, model.pendingInputs)
	}
	_, cmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	_, msgs := runCmd(t, cmd)
	if quits(msgs) {
		t.Fatal("Esc exited while an unconfirmed input may be running")
	}
	var finished *stopFinishedMsg
	for _, msg := range msgs {
		if value, ok := msg.(stopFinishedMsg); ok {
			finished = &value
		}
	}
	if finished == nil || finished.err != nil || finished.result != zotigodclient.StopRequested || len(service.pauses) != 1 {
		t.Fatalf("stop=%+v pauses=%v", finished, service.pauses)
	}
	model.Update(*finished)
	if len(model.pendingInputs) != 0 {
		t.Fatalf("placed inputs still pending: %v", model.pendingInputs)
	}
	// With the turn stopped and nothing pending, Esc exits again.
	_, cmd = model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !quits(msgsOf(t, cmd)) {
		t.Fatalf("Esc did not exit once idle: thinking=%v stopping=%v pending=%v", model.thinking, model.stopping, model.pendingInputs)
	}
}
