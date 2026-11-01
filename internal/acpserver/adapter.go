package acpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jayyao97/zotigo/core/acp"
	"github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigodclient"
)

type adapter struct {
	client    *zotigodclient.Client
	embedded  bool
	workspace *zotigodclient.Workspace
	logger    *log.Logger
	server    *acp.Server

	mu       sync.Mutex
	sessions map[string]*acpSession
	// permissions tracks in-flight permission requests so shutdown can let
	// them settle before deciding what to stop.
	permissions sync.WaitGroup
}

// acpSession tracks one attached zotigod session for this ACP connection.
// The service is the source of truth; this state only maps its durable item
// stream onto ACP notifications and pending prompt responses.
type acpSession struct {
	id     string
	cwd    string
	cancel context.CancelFunc

	mu              sync.Mutex
	openTurnID      string
	ourInputs       map[string]bool
	waiters         []*promptWaiter
	ownedTurns      map[string]bool // turns an outstanding prompt of this connection waits on
	streamed        map[string]map[string]string
	finalized       map[string]bool // durable assistant items already sent; late deltas are dropped
	announcedCalls  map[string]bool
	pendingApproval string
	cancelRequested bool
	submissions     *zotigodclient.Submissions
	// unsettled maps this connection's inputs that may still run to the
	// turn they started or joined ("" until known). Unlike waiters it
	// survives a prompt that returned early (submission error, editor
	// gone), so cancel and disconnect still find the turn.
	unsettled map[string]string
	notice    string
	policy    string
}

// promptWaiter waits for the turn that consumes one submitted input.
type promptWaiter struct {
	inputID string
	turnID  string
	done    chan promptOutcome
}

type promptOutcome struct {
	stopReason string
	err        error
}

func newACPSession(id string, cwd string) *acpSession {
	return &acpSession{
		id:             id,
		cwd:            cwd,
		ourInputs:      make(map[string]bool),
		ownedTurns:     make(map[string]bool),
		submissions:    zotigodclient.NewSubmissions(),
		unsettled:      make(map[string]string),
		streamed:       make(map[string]map[string]string),
		finalized:      make(map[string]bool),
		announcedCalls: make(map[string]bool),
	}
}

func (a *adapter) onSessionNew(ctx context.Context, params acp.SessionNewParams) (string, error) {
	cwd, err := validateCwd(params.Cwd)
	if err != nil {
		return "", err
	}
	// Editors confirm each tool call in their own UI, so ACP sessions start
	// in Manual; clients switch with session/set_mode.
	req := zotigodclient.CreateSessionRequest{WorkingDirectory: cwd, ApprovalPolicy: policyManual}
	if a.workspace != nil {
		// The service rejects a cwd outside the Workspace; it never moves
		// the session to the Workspace root.
		req.WorkspaceID = a.workspace.ID
	}
	created, err := a.client.CreateSession(ctx, req)
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	sess := newACPSession(created.ID, cwd)
	sess.notice = mcpNotice(params.MCPServers)
	sess.policy = created.ApprovalPolicy
	a.attach(sess, 0)
	a.logger.Printf("Session created: %s (cwd=%s)", created.ID, cwd)
	return created.ID, nil
}

// mcpNotice explains client-provided MCP servers that this agent cannot
// load, so the configuration is not silently ignored.
func mcpNotice(servers []acp.MCPServer) string {
	if len(servers) == 0 {
		return ""
	}
	names := make([]string, 0, len(servers))
	for _, server := range servers {
		names = append(names, server.Name)
	}
	return fmt.Sprintf("Note: zotigo does not support MCP servers yet; the configured servers were not loaded: %s.\n\n", strings.Join(names, ", "))
}

func (a *adapter) onSessionLoad(ctx context.Context, params acp.SessionLoadParams) (func(), error) {
	cwd, err := validateCwd(params.Cwd)
	if err != nil {
		return nil, err
	}
	stored, err := a.client.GetSession(ctx, params.SessionID)
	if err != nil {
		return nil, fmt.Errorf("load session %s: %w", params.SessionID, err)
	}
	if stored.Agent != "" && stored.Agent != "zotigo" {
		return nil, fmt.Errorf("session %s uses the %s runtime, which ACP cannot drive", stored.ID, stored.Agent)
	}
	if filepath.Clean(stored.WorkingDirectory) != cwd {
		return nil, fmt.Errorf("session %s runs in %s, not the requested cwd %s", stored.ID, stored.WorkingDirectory, cwd)
	}
	if a.workspace != nil {
		projection, err := a.client.CatalogSession(ctx, stored.ID)
		if err != nil {
			return nil, fmt.Errorf("load session ownership: %w", err)
		}
		if projection.WorkspaceID() != a.workspace.ID {
			return nil, fmt.Errorf("session %s does not belong to workspace %s", stored.ID, a.workspace.ID)
		}
	}
	items, err := a.client.AllItems(ctx, stored.ID)
	if err != nil {
		return nil, fmt.Errorf("load session history: %w", err)
	}
	a.detach(stored.ID)
	sess := newACPSession(stored.ID, cwd)
	sess.notice = mcpNotice(params.MCPServers)
	sess.policy = stored.ApprovalPolicy
	var cursor uint64
	for _, item := range items {
		a.replayItem(ctx, sess, item)
		cursor = item.Sequence
	}
	sess.openTurnID = openTurnID(items)
	pending := pendingApproval(items, sess.openTurnID)
	a.attach(sess, cursor)
	a.logger.Printf("Session loaded: %s (cwd=%s, items=%d)", stored.ID, cwd, len(items))
	if pending == nil || !stored.Live {
		// An offline session's pending approval is denied by the next
		// worker on restart; there is nothing live to answer.
		return nil, nil
	}
	return func() { a.goRequestPermission(sess, *pending) }, nil
}

const (
	policyManual = "manual"
	policyAuto   = "auto"
	policyBypass = "bypass_permissions"
)

// approvalModes exposes the session approval policy as ACP session modes.
var approvalModes = []acp.SessionMode{
	{ID: policyManual, Name: "Manual", Description: "Ask before every action that changes something"},
	{ID: policyAuto, Name: "Auto", Description: "Run safe actions; ask when the safety checks require it"},
	{ID: policyBypass, Name: "Full access", Description: "Run every tool without approval"},
}

func (a *adapter) sessionModes(_ context.Context, sessionID string) *acp.SessionModeState {
	sess, ok := a.session(sessionID)
	if !ok {
		return nil
	}
	sess.mu.Lock()
	current := sess.policy
	sess.mu.Unlock()
	if current == "" {
		current = policyAuto
	}
	return &acp.SessionModeState{CurrentModeID: current, AvailableModes: approvalModes}
}

// onSetMode changes the session approval policy while the session is idle.
// When a worker is running the service records approval_policy_changed,
// which every attached client (including this one) reports as the current
// mode.
func (a *adapter) onSetMode(ctx context.Context, sessionID string, modeID string) error {
	sess, ok := a.session(sessionID)
	if !ok {
		return fmt.Errorf("session %q not found", sessionID)
	}
	known := false
	for _, mode := range approvalModes {
		known = known || mode.ID == modeID
	}
	if !known {
		return fmt.Errorf("unknown mode %q", modeID)
	}
	if err := a.client.SetApprovalPolicy(ctx, sessionID, modeID); err != nil {
		return fmt.Errorf("set mode: %w", err)
	}
	// A session without a running worker applies the policy directly and
	// records no item, so track the accepted mode here as well.
	sess.mu.Lock()
	sess.policy = modeID
	sess.mu.Unlock()
	return nil
}

func (a *adapter) onSessionList(ctx context.Context, params acp.SessionListParams) (acp.SessionListResult, error) {
	projections, err := a.client.CatalogSessions(ctx)
	if err != nil {
		return acp.SessionListResult{}, err
	}
	var result acp.SessionListResult
	for _, projection := range projections {
		runtime := projection.Runtime
		if runtime == nil || projection.Availability != "ready" || (runtime.Agent != "" && runtime.Agent != "zotigo") {
			continue
		}
		if params.Cwd != "" && filepath.Clean(params.Cwd) != filepath.Clean(runtime.WorkingDirectory) {
			continue
		}
		if a.workspace != nil && projection.WorkspaceID() != a.workspace.ID {
			continue
		}
		title := runtime.LastPrompt
		if projection.Organization != nil && projection.Organization.Title != nil {
			title = *projection.Organization.Title
		}
		result.Sessions = append(result.Sessions, acp.SessionInfo{SessionID: runtime.ID, Cwd: runtime.WorkingDirectory, Title: title})
	}
	return result, nil
}

func (a *adapter) session(id string) (*acpSession, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sess, ok := a.sessions[id]
	return sess, ok
}

// attach starts streaming service items after cursor into ACP updates.
func (a *adapter) attach(sess *acpSession, cursor uint64) {
	streamCtx, cancel := context.WithCancel(context.Background())
	sess.cancel = cancel
	a.mu.Lock()
	a.sessions[sess.id] = sess
	a.mu.Unlock()
	a.server.RegisterSession(sess.id, acp.NewSession(sess.id, sess.cwd))
	events := a.client.Subscribe(streamCtx, sess.id, cursor, zotigodclient.StreamOptions{})
	go func() {
		for event := range events {
			a.handleEvent(streamCtx, sess, event)
		}
	}()
}

func (a *adapter) detach(id string) {
	a.mu.Lock()
	sess := a.sessions[id]
	delete(a.sessions, id)
	a.mu.Unlock()
	if sess != nil {
		sess.cancel()
		sess.finishAll(promptOutcome{err: errors.New("session was reloaded")})
	}
}

// shutdown runs after the editor disconnects. A turn that one of this
// connection's prompts was waiting on is stopped: ACP has no background
// mode, and nobody would answer its approvals. Turns started by other
// clients of the shared daemon keep running. With an embedded service
// every open turn is stopped so the worker records the interruption before
// the process exits. Pending approvals were already settled by their
// permission requests, which fail once the connection is gone.
func (a *adapter) shutdown() {
	settled := make(chan struct{})
	go func() {
		a.permissions.Wait()
		close(settled)
	}()
	select {
	case <-settled:
	case <-time.After(10 * time.Second):
		a.logger.Printf("Permission requests still in flight at shutdown")
	}
	a.mu.Lock()
	sessions := make([]*acpSession, 0, len(a.sessions))
	for _, sess := range a.sessions {
		sessions = append(sessions, sess)
	}
	a.mu.Unlock()
	for _, sess := range sessions {
		target := sess.promptTarget(a.embedded)
		target.AnyOpenTurn = a.embedded
		if target.TurnID != "" || len(target.InputIDs) > 0 || target.AnyOpenTurn {
			a.stop(sess, target, "client disconnected")
		} else if sess.openTurn() != "" {
			a.logger.Printf("Turn %s of session %s was started by another client and continues in zotigod", sess.openTurn(), sess.id)
		}
		sess.cancel()
		sess.finishAll(promptOutcome{err: errors.New("ACP server shutting down")})
	}
}

// stop stops the turn target describes and waits until the service has
// accepted it or the turn has ended. It covers turns the event stream has
// not reported yet and approvals resolved by another client meanwhile.
func (a *adapter) stop(sess *acpSession, target zotigodclient.StopTarget, why string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := zotigodclient.StopTurn(ctx, a.client, sess.id, target)
	if err != nil {
		a.logger.Printf("Stopping session %s (%s): %v", sess.id, why, err)
		return
	}
	if result == zotigodclient.StopRequested {
		a.logger.Printf("Stopped session %s turn (%s)", sess.id, why)
	}
}

// promptTarget is the open turn plus the inputs of this connection's
// outstanding prompts, including submissions still in flight.
func (s *acpSession) promptTarget(includeOpen bool) zotigodclient.StopTarget {
	s.mu.Lock()
	defer s.mu.Unlock()
	target := zotigodclient.StopTarget{Submissions: s.submissions}
	if includeOpen || s.ownedTurns[s.openTurnID] {
		target.TurnID = s.openTurnID
	}
	inputs := make(map[string]bool, len(s.waiters)+len(s.unsettled))
	for _, waiter := range s.waiters {
		inputs[waiter.inputID] = true
	}
	for id := range s.unsettled {
		inputs[id] = true
	}
	for id := range inputs {
		target.InputIDs = append(target.InputIDs, id)
	}
	return target
}

func (a *adapter) onSessionPrompt(ctx context.Context, sessionID string, text string, images []acp.ContentBlock) acp.PromptResult {
	sess, ok := a.session(sessionID)
	if !ok {
		return acp.PromptResult{Err: fmt.Errorf("session %q not found", sessionID)}
	}
	req := zotigodclient.MessageRequest{Text: text, ClientMessageID: "acp_" + uuid.NewString()}
	for _, image := range images {
		if image.Data != "" {
			req.Images = append(req.Images, zotigodclient.Image{MimeType: image.MimeType, DataBase64: image.Data})
		}
	}
	waiter := &promptWaiter{inputID: req.ClientMessageID, done: make(chan promptOutcome, 1)}
	sess.mu.Lock()
	sess.unsettled[req.ClientMessageID] = ""
	sess.cancelRequested = false
	sess.ourInputs[req.ClientMessageID] = true
	sess.waiters = append(sess.waiters, waiter)
	notice := sess.notice
	sess.notice = ""
	sess.mu.Unlock()
	if notice != "" {
		_ = a.server.SendTextChunk(ctx, sessionID, notice)
	}

	command, err := zotigodclient.SubmitTracked(ctx, a.client, sessionID, req, sess.submissions)
	if err != nil {
		sess.removeWaiter(waiter)
		if zotigodclient.SubmissionRejectedErr(err) {
			sess.settle(req.ClientMessageID)
			return acp.PromptResult{Err: fmt.Errorf("submit message: %w", err)}
		}
		// The input may be running; it stays unsettled so cancel and
		// disconnect still stop it, and its output keeps streaming.
		return acp.PromptResult{Err: fmt.Errorf("submit message: %w (it may still be running; session/cancel stops it)", err)}
	}
	if command.Type == "steering" && command.TurnID != "" {
		sess.bindWaiter(req.ClientMessageID, command.TurnID)
	}

	select {
	case outcome := <-waiter.done:
		return acp.PromptResult{StopReason: outcome.stopReason, Err: outcome.err}
	case <-ctx.Done():
		sess.removeWaiter(waiter)
		// The client went away or the server is stopping. The service keeps
		// the turn; a reconnecting client can session/load it.
		return acp.PromptResult{StopReason: acp.StopReasonCancelled}
	}
}

// onSessionCancel stops the session's running turn, including one this
// connection's prompt has submitted but the stream has not reported yet.
// A pending approval is denied with stop_turn; the client also answers its
// open permission request with "cancelled".
func (a *adapter) onSessionCancel(_ context.Context, sessionID string) {
	sess, ok := a.session(sessionID)
	if !ok {
		return
	}
	sess.mu.Lock()
	sess.cancelRequested = true
	sess.mu.Unlock()
	go a.stop(sess, sess.promptTarget(true), "session/cancel")
}

func (s *acpSession) openTurn() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openTurnID
}

func (s *acpSession) settle(inputID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.unsettled, inputID)
}

func (s *acpSession) bindWaiter(inputID string, turnID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.unsettled[inputID]; ok {
		s.unsettled[inputID] = turnID
	}
	for _, waiter := range s.waiters {
		if waiter.inputID == inputID && waiter.turnID == "" {
			waiter.turnID = turnID
			s.ownedTurns[turnID] = true
		}
	}
}

func (s *acpSession) removeWaiter(target *promptWaiter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, waiter := range s.waiters {
		if waiter == target {
			s.waiters = append(s.waiters[:index], s.waiters[index+1:]...)
			return
		}
	}
}

// finishTurn resolves every prompt waiting on turnID.
func (s *acpSession) finishTurn(turnID string, outcome promptOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ownedTurns, turnID)
	for id, joined := range s.unsettled {
		if joined == turnID {
			delete(s.unsettled, id)
		}
	}
	kept := s.waiters[:0]
	for _, waiter := range s.waiters {
		if waiter.turnID == turnID {
			waiter.done <- outcome
			continue
		}
		kept = append(kept, waiter)
	}
	s.waiters = kept
}

func (s *acpSession) finishAll(outcome promptOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, waiter := range s.waiters {
		waiter.done <- outcome
	}
	s.waiters = nil
}

func (a *adapter) handleEvent(ctx context.Context, sess *acpSession, event zotigodclient.Event) {
	switch {
	case event.Delta != nil:
		a.handleDelta(ctx, sess, *event.Delta)
	case event.Item != nil:
		a.handleItem(ctx, sess, *event.Item)
	case event.Reconnecting:
		a.logger.Printf("Session %s stream reconnecting: %v", sess.id, event.Err)
	case event.Err != nil:
		a.logger.Printf("Session %s stream ended: %v", sess.id, event.Err)
		sess.finishAll(promptOutcome{err: fmt.Errorf("session stream ended: %w", event.Err)})
	}
}

func (a *adapter) handleDelta(ctx context.Context, sess *acpSession, delta zotigodclient.Delta) {
	if delta.Subagent != nil || delta.Delta == "" {
		return
	}
	if delta.PartType != "text" && delta.PartType != "reasoning" {
		return
	}
	sess.mu.Lock()
	if sess.finalized[delta.ItemID] {
		// Deltas and durable items travel separately; the durable item
		// already delivered this text.
		sess.mu.Unlock()
		return
	}
	if sess.streamed[delta.ItemID] == nil {
		sess.streamed[delta.ItemID] = make(map[string]string)
	}
	sess.streamed[delta.ItemID][delta.PartType] += delta.Delta
	sess.mu.Unlock()
	a.sendChunk(ctx, sess.id, delta.PartType, delta.Delta)
}

func (a *adapter) sendChunk(ctx context.Context, sessionID string, partType string, text string) {
	if text == "" {
		return
	}
	if partType == "reasoning" {
		_ = a.server.SendThoughtChunk(ctx, sessionID, text)
		return
	}
	_ = a.server.SendTextChunk(ctx, sessionID, text)
}

func (a *adapter) handleItem(ctx context.Context, sess *acpSession, item session.DisplayItem) {
	if item.Subagent != nil {
		return
	}
	switch item.Type {
	case session.DisplayItemUserMessage, session.DisplayItemSteeringMessage, session.DisplayItemSessionCommand:
		a.handleInputItem(ctx, sess, item)
	case session.DisplayItemAssistantMessage:
		a.sendAssistantItem(ctx, sess, item)
	case session.DisplayItemTurnStarted:
		if item.Turn == nil {
			return
		}
		sess.mu.Lock()
		sess.openTurnID = item.Turn.ID
		for id, joined := range sess.unsettled {
			if joined == "" && !sess.ourInputs[id] {
				// Recorded as a user message: this is the turn it started.
				sess.unsettled[id] = item.Turn.ID
				sess.ownedTurns[item.Turn.ID] = true
			}
		}
		for _, waiter := range sess.waiters {
			if waiter.turnID == "" && waiter.inputID != "" && !sess.ourInputs[waiter.inputID] {
				// Our message was recorded and this is the turn it started.
				waiter.turnID = item.Turn.ID
				sess.ownedTurns[item.Turn.ID] = true
			}
		}
		sess.mu.Unlock()
	case session.DisplayItemApprovalRequest:
		if item.Approval != nil && len(item.Approval.Pending) > 0 {
			a.goRequestPermission(sess, *item.Approval)
		}
	case session.DisplayItemApprovalDecision:
		if item.Approval != nil {
			sess.mu.Lock()
			if sess.pendingApproval == item.Approval.ID {
				sess.pendingApproval = ""
			}
			sess.mu.Unlock()
		}
	case session.DisplayItemTurnCompleted, session.DisplayItemTurnFailed, session.DisplayItemTurnInterrupted:
		if item.Turn == nil {
			return
		}
		sess.mu.Lock()
		if sess.openTurnID == item.Turn.ID {
			sess.openTurnID = ""
		}
		sess.mu.Unlock()
		sess.finishTurn(item.Turn.ID, turnOutcome(item))
	case session.DisplayItemError:
		if item.Error != "" {
			_ = a.server.SendTextChunk(ctx, sess.id, "\nError: "+item.Error+"\n")
		}
	case session.DisplayItemContextCompacted:
		_ = a.server.SendTextChunk(ctx, sess.id, "\n[context compacted]\n")
	case session.DisplayItemApprovalPolicyChanged:
		if item.ApprovalPolicy == nil || item.ApprovalPolicy.To == "" {
			return
		}
		sess.mu.Lock()
		sess.policy = item.ApprovalPolicy.To
		sess.mu.Unlock()
		_ = a.server.SendCurrentMode(ctx, sess.id, item.ApprovalPolicy.To)
	}
}

func (s *acpSession) ownsTurn(turnID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ownedTurns[turnID]
}

// handleInputItem recognizes our own inputs by their client message ID and
// shows inputs from other clients as user messages.
func (a *adapter) handleInputItem(ctx context.Context, sess *acpSession, item session.DisplayItem) {
	sess.mu.Lock()
	ours := sess.ourInputs[item.ID]
	if ours {
		delete(sess.ourInputs, item.ID)
		if item.Type != session.DisplayItemUserMessage {
			// Steering names the turn it joined.
			joined := sess.openTurnID
			if item.Command != nil && item.Command.TurnID != "" {
				joined = item.Command.TurnID
			}
			if _, ok := sess.unsettled[item.ID]; ok && joined != "" {
				sess.unsettled[item.ID] = joined
				sess.ownedTurns[joined] = true
			}
		}
		for _, waiter := range sess.waiters {
			if waiter.inputID == item.ID && waiter.turnID == "" && sess.openTurnID != "" && item.Type != session.DisplayItemUserMessage {
				// Applied or pending steering joins the open turn.
				waiter.turnID = sess.openTurnID
			}
		}
	}
	sess.mu.Unlock()
	if ours || item.Type == session.DisplayItemSessionCommand {
		return
	}
	if text := displayText(item.Content); text != "" {
		_ = a.server.SendUserChunk(ctx, sess.id, text)
	}
}

// sendAssistantItem sends what the delta stream has not already delivered
// for a durable assistant item, then its tool calls and results.
func (a *adapter) sendAssistantItem(ctx context.Context, sess *acpSession, item session.DisplayItem) {
	sess.mu.Lock()
	streamed := sess.streamed[item.ID]
	delete(sess.streamed, item.ID)
	sess.finalized[item.ID] = true
	sess.mu.Unlock()
	for _, part := range item.Content {
		switch part.Type {
		case "text", "reasoning":
			shown := streamed[part.Type]
			remainder := part.Text
			if shown != "" && strings.HasPrefix(part.Text, shown) {
				remainder = part.Text[len(shown):]
			}
			a.sendChunk(ctx, sess.id, part.Type, remainder)
		case "tool_call":
			if part.ToolCall != nil {
				a.announceToolCall(ctx, sess, part.ToolCall.ID, part.ToolCall.Name, part.ToolCall.Arguments, acp.ToolCallStatusPending)
			}
		case "tool_result":
			if part.ToolResult != nil {
				a.sendToolResult(ctx, sess, part.ToolResult)
			}
		}
	}
}

func (a *adapter) announceToolCall(ctx context.Context, sess *acpSession, id string, name string, arguments string, status string) {
	sess.mu.Lock()
	announced := sess.announcedCalls[id]
	sess.announcedCalls[id] = true
	sess.mu.Unlock()
	if announced {
		return
	}
	update := acp.SessionUpdate{
		"sessionUpdate": "tool_call",
		"toolCallId":    id,
		"title":         toolTitle(name, arguments),
		"kind":          toolKind(name),
		"status":        status,
	}
	if raw := rawJSON(arguments); raw != nil {
		update["rawInput"] = raw
	}
	_ = a.server.SendUpdate(ctx, sess.id, update)
}

func (a *adapter) sendToolResult(ctx context.Context, sess *acpSession, result *session.DisplayToolResult) {
	a.announceToolCall(ctx, sess, result.ToolCallID, result.ToolName, "", acp.ToolCallStatusInProgress)
	status := acp.ToolCallStatusCompleted
	if result.IsError {
		status = acp.ToolCallStatusFailed
	}
	text := result.Text
	if text == "" && result.Reason != "" {
		text = result.Reason
	}
	fields := map[string]any{"status": status}
	if text != "" {
		fields["content"] = []acp.ToolCallContent{{Type: "content", ContentBlock: &acp.ContentBlock{Type: "text", Text: text}}}
	}
	_ = a.server.SendToolCallUpdate(ctx, sess.id, result.ToolCallID, fields)
}

// requestPermission asks the client about each pending call and submits
// one decision set. Another client may decide first; the service keeps the
// first durable decision and rejects ours with a conflict.
func (a *adapter) goRequestPermission(sess *acpSession, approval session.DisplayApproval) {
	a.permissions.Add(1)
	go func() {
		defer a.permissions.Done()
		a.requestPermission(sess, approval)
	}()
}

func (a *adapter) clientGone() bool {
	select {
	case <-a.server.Done():
		return true
	default:
		return false
	}
}

func (a *adapter) requestPermission(sess *acpSession, approval session.DisplayApproval) {
	sess.mu.Lock()
	if sess.pendingApproval == approval.ID {
		sess.mu.Unlock()
		return
	}
	sess.pendingApproval = approval.ID
	sess.mu.Unlock()
	defer func() {
		sess.mu.Lock()
		if sess.pendingApproval == approval.ID {
			sess.pendingApproval = ""
		}
		sess.mu.Unlock()
	}()

	ctx := context.Background()
	decisions := make([]zotigodclient.ApprovalDecision, 0, len(approval.Pending))
	cancelled := false
	for _, pending := range approval.Pending {
		if cancelled {
			decisions = append(decisions, zotigodclient.ApprovalDecision{ToolCallID: pending.ToolCallID, Reason: "Cancelled by the ACP client"})
			continue
		}
		a.announceToolCall(ctx, sess, pending.ToolCallID, pending.ToolName, pending.Arguments, acp.ToolCallStatusPending)
		outcome, err := a.server.RequestPermission(ctx, sess.id, acp.ToolCallData{
			ToolCallID: pending.ToolCallID,
			Title:      permissionTitle(pending),
			Kind:       toolKind(pending.ToolName),
			Status:     acp.ToolCallStatusPending,
			RawInput:   rawJSON(pending.Arguments),
		}, []acp.PermissionOption{
			{OptionID: "allow_once", Name: "Allow", Kind: "allow_once"},
			{OptionID: "reject_once", Name: "Reject", Kind: "reject_once"},
		})
		switch {
		case err != nil && a.clientGone():
			// The editor disconnected. Stop a turn this connection was
			// waiting on (or any turn of an embedded service, which is
			// about to exit); leave other clients' approvals pending for
			// them to answer.
			if !a.embedded && !sess.ownsTurn(approval.TurnID) {
				a.logger.Printf("Client disconnected; approval %s stays pending for other clients", approval.ID)
				return
			}
			cancelled = true
			decisions = append(decisions, zotigodclient.ApprovalDecision{ToolCallID: pending.ToolCallID, Reason: "ACP client disconnected"})
		case err != nil:
			// Fail closed: a client that cannot answer must not leave the
			// turn blocked, and must never approve implicitly.
			a.logger.Printf("Permission request for %s failed: %v", pending.ToolCallID, err)
			decisions = append(decisions, zotigodclient.ApprovalDecision{ToolCallID: pending.ToolCallID, Reason: "ACP client could not answer the permission request"})
		case outcome.Outcome == "cancelled":
			cancelled = true
			decisions = append(decisions, zotigodclient.ApprovalDecision{ToolCallID: pending.ToolCallID, Reason: "Cancelled by the ACP client"})
		default:
			approved := outcome.Outcome == "selected" && outcome.OptionID == "allow_once"
			decision := zotigodclient.ApprovalDecision{ToolCallID: pending.ToolCallID, Approved: approved}
			if !approved {
				decision.Reason = "Rejected in the editor"
			}
			decisions = append(decisions, decision)
		}
	}
	sess.mu.Lock()
	if sess.cancelRequested {
		cancelled = true
	}
	resolvedElsewhere := sess.pendingApproval != approval.ID
	sess.mu.Unlock()
	stopTarget := zotigodclient.StopTarget{TurnID: approval.TurnID}
	if resolvedElsewhere {
		// Another client decided first, possibly approving. A cancel
		// still has to stop the turn.
		if cancelled {
			a.stop(sess, stopTarget, "cancelled after approval resolved elsewhere")
		}
		return
	}
	decideCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	// A cancelled prompt stops the turn together with the denial so the
	// agent cannot continue past it.
	err := a.client.DecideApproval(decideCtx, sess.id, approval.ID, decisions, cancelled && allDenied(decisions))
	switch {
	case err == nil && cancelled && !allDenied(decisions):
		a.stop(sess, stopTarget, "cancelled after partial approval")
	case err == nil:
	case zotigodclient.IsStatus(err, 409):
		// 409 only means the approval was resolved, not that the turn
		// stopped; it may have been approved by another client.
		a.logger.Printf("Approval %s was already resolved by another client", approval.ID)
		if cancelled {
			a.stop(sess, stopTarget, "cancelled after approval resolved elsewhere")
		}
	default:
		a.logger.Printf("Submitting approval %s: %v", approval.ID, err)
	}
}

func allDenied(decisions []zotigodclient.ApprovalDecision) bool {
	for _, decision := range decisions {
		if decision.Approved {
			return false
		}
	}
	return true
}

func turnOutcome(item session.DisplayItem) promptOutcome {
	switch item.Type {
	case session.DisplayItemTurnInterrupted:
		return promptOutcome{stopReason: acp.StopReasonCancelled}
	case session.DisplayItemTurnFailed:
		reason := item.Error
		if reason == "" && item.Turn != nil {
			reason = item.Turn.Reason
		}
		if reason == "" {
			reason = "turn failed"
		}
		return promptOutcome{err: errors.New(reason)}
	}
	if item.Turn != nil {
		switch item.Turn.ProviderFinishReason {
		case "length", "max_tokens":
			return promptOutcome{stopReason: acp.StopReasonMaxTokens}
		case "refusal", "content_filter":
			return promptOutcome{stopReason: acp.StopReasonRefusal}
		}
	}
	return promptOutcome{stopReason: acp.StopReasonEndTurn}
}

// replayItem sends one historical item during session/load.
func (a *adapter) replayItem(ctx context.Context, sess *acpSession, item session.DisplayItem) {
	if item.Subagent != nil {
		return
	}
	switch item.Type {
	case session.DisplayItemUserMessage, session.DisplayItemSteeringMessage:
		if text := displayText(item.Content); text != "" {
			_ = a.server.SendUserChunk(ctx, sess.id, text)
		}
	case session.DisplayItemAssistantMessage:
		a.sendAssistantItem(ctx, sess, item)
	}
}

func displayText(parts []session.DisplayContentPart) string {
	var texts []string
	for _, part := range parts {
		if part.Type == "text" && part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func openTurnID(items []session.DisplayItem) string {
	open := ""
	for _, item := range items {
		if item.Turn == nil || item.Turn.ID == "" {
			continue
		}
		switch item.Type {
		case session.DisplayItemTurnStarted, session.DisplayItemTurnPaused:
			open = item.Turn.ID
		case session.DisplayItemTurnCompleted, session.DisplayItemTurnFailed, session.DisplayItemTurnInterrupted:
			open = ""
		}
	}
	return open
}

func pendingApproval(items []session.DisplayItem, turnID string) *session.DisplayApproval {
	if turnID == "" {
		return nil
	}
	var pending *session.DisplayApproval
	for _, item := range items {
		switch {
		case item.Type == session.DisplayItemApprovalRequest && item.Approval != nil && item.Approval.TurnID == turnID:
			approval := *item.Approval
			pending = &approval
		case item.Type == session.DisplayItemApprovalDecision && item.Approval != nil && pending != nil && item.Approval.ID == pending.ID:
			pending = nil
		}
	}
	return pending
}

func rawJSON(arguments string) any {
	if strings.TrimSpace(arguments) == "" {
		return nil
	}
	var value any
	if err := json.Unmarshal([]byte(arguments), &value); err != nil {
		return arguments
	}
	return value
}

// toolKind maps zotigo tools onto ACP tool kinds so editors pick icons
// and affordances.
func toolKind(name string) string {
	switch name {
	case "read_file", "view_image":
		return "read"
	case "write_file", "edit":
		return "edit"
	case "shell":
		return "execute"
	case "grep", "glob", "lsp":
		return "search"
	case "web_fetch", "web_search":
		return "fetch"
	case "spawn":
		return "think"
	default:
		return "other"
	}
}

func toolTitle(name string, arguments string) string {
	args, ok := rawJSON(arguments).(map[string]any)
	if !ok {
		return name
	}
	for _, key := range []string{"command", "path", "pattern", "query", "url"} {
		if value, ok := args[key].(string); ok && value != "" {
			if len(value) > 120 {
				value = value[:117] + "..."
			}
			return name + ": " + value
		}
	}
	return name
}

func permissionTitle(pending session.DisplayPendingApproval) string {
	title := toolTitle(pending.ToolName, pending.Arguments)
	if pending.Description != "" {
		title += " — " + pending.Description
	}
	if strings.HasPrefix(pending.Source, "subagent:") {
		title = "[" + strings.TrimPrefix(pending.Source, "subagent:") + "] " + title
	}
	return title
}
