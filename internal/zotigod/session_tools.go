package zotigod

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	session "github.com/jayyao97/zotigo/core/session"
	workspace "github.com/jayyao97/zotigo/core/workspace"
)

type sessionToolCaller struct {
	SessionID, TurnID, WorkspaceID string
	Generation                     string
	Origin                         *protocol.RequestContext
	Stored                         *session.Session
}

// Resolve authority from durable host input, never from model arguments or the
// worker's echoed RequestContext. A generation identifies a worker incarnation.
func (h *handler) resolveSessionToolCaller(ctx context.Context, id, generation, turnID string) (sessionToolCaller, error) {
	return h.resolveRuntimeToolCaller(ctx, id, generation, turnID, true)
}

func (h *handler) resolveRuntimeToolCaller(ctx context.Context, id, generation, turnID string, sessionScope bool) (sessionToolCaller, error) {
	caller := sessionToolCaller{SessionID: id, TurnID: turnID, Generation: generation}
	if h.workers == nil || !h.workers.Matches(id, generation) || turnID == "" {
		return caller, errors.New("runtime tool caller is not the active worker")
	}
	stored, err := h.store.Get(ctx, id)
	if err != nil {
		return caller, err
	}
	if stored == nil {
		return caller, errSessionNotFound
	}
	items, exists, err := h.items.LoadItems(ctx, id)
	if err != nil {
		return caller, err
	}
	if !exists || lastOpenTurnID(items) != turnID {
		return caller, errors.New("runtime tool caller turn is no longer active")
	}
	var pending, input *session.DisplayItem
	started := false
	for index := range items {
		item := &items[index]
		if item.Command != nil && item.Command.Type == sessionCommandMessage {
			pending = item
		}
		if item.Type == session.DisplayItemTurnStarted && item.Turn != nil && item.Turn.ID == turnID {
			input = pending
			started = true
		}
		// Mixed-principal steering is not an authorization context. Fail closed until
		// per-input principal intersection is supported, even for local steering.
		if sessionScope && started && item.Command != nil && item.Command.Type == sessionCommandSteering {
			return caller, errors.New("session tools are disabled in turns with steering input")
		}
	}
	if input == nil || input.Command == nil {
		return caller, errors.New("runtime tool caller has no durable host input")
	}
	if sessionScope && strings.HasPrefix(input.ID, "session_tool:") {
		return caller, errors.New("delegated tasks cannot invoke session tools again")
	}
	caller.Origin = input.Command.RequestContext.Clone()
	if caller.Origin == nil && strings.HasPrefix(input.ID, "channel:") {
		return caller, errors.New("channel input has no trusted origin")
	}
	if h.catalog == nil {
		return caller, errors.New("session tools require a workspace catalog")
	}
	organization, err := h.catalog.GetSessionOrganization(ctx, id)
	if err != nil {
		return caller, err
	}
	if organization.WorkspaceID == nil || organization.EffectiveArchived() {
		return caller, errors.New("source session has no active workspace")
	}
	caller.WorkspaceID = *organization.WorkspaceID
	caller.Stored = stored
	return caller, nil
}

func (h *handler) executeRuntimeTool(ctx context.Context, id, generation string, request workerRuntimeToolRequest) (string, error) {
	if !h.workers.Matches(id, generation) {
		return "", errors.New("stale runtime tool worker")
	}
	if request.Namespace == "host" && request.Name == "authorize_input" {
		return h.authorizeDelegatedReplay(ctx, id, request.Arguments)
	}
	if request.Namespace == "" || request.Namespace == "channel" {
		// Only the pre-existing channel.read_messages protocol accepts legacy calls.
		if request.Name != "read_messages" || h.channels == nil {
			return "", errors.New("unknown runtime tool")
		}
		turnID := request.TurnID
		if turnID == "" {
			items, _, err := h.items.LoadItems(ctx, id)
			if err != nil {
				return "", err
			}
			turnID = lastOpenTurnID(items)
		}
		caller, err := h.resolveRuntimeToolCaller(ctx, id, generation, turnID, false)
		if err != nil {
			return "", err
		}
		value, err := h.channels.ExecuteRuntimeTool(ctx, id, caller.Origin, request.Name, request.Arguments)
		return value.Text, err
	}
	if request.Namespace != "zotigo" || request.CallID == "" || len(request.CallID) > 512 {
		return "", errors.New("invalid runtime tool identity")
	}
	var spec *runtimeToolSpec
	for _, candidate := range sessionToolSpecs() {
		if candidate.Name == request.Name {
			copy := candidate
			spec = &copy
			break
		}
	}
	if spec == nil {
		return "", errors.New("unknown session tool")
	}
	caller, err := h.resolveSessionToolCaller(ctx, id, generation, request.TurnID)
	if err != nil {
		return "", err
	}
	if caller.Stored.Capabilities.SessionToolsVersion < 1 {
		return "", errors.New("session tools were not installed for this session")
	}
	var result any
	run := func() error {
		// The same mandatory guard covers both runtimes. A configuration fence in
		// channels additionally prevents owner revocation racing durable admission.
		if !h.workers.Matches(id, generation) {
			return errors.New("stale runtime tool worker")
		}
		boundWorkspace, release, err := h.lockWorkspaceForUse(ctx, caller.WorkspaceID)
		if err != nil {
			return err
		}
		defer release()
		if boundWorkspace.Status != workspace.WorkspaceStatusReady {
			return errors.New("source workspace is not ready")
		}
		fresh, err := h.resolveSessionToolCaller(ctx, id, generation, request.TurnID)
		if err != nil {
			return err
		}
		if fresh.WorkspaceID != caller.WorkspaceID {
			return errors.New("source workspace changed during authorization")
		}
		switch request.Name {
		case "list_sessions":
			result, err = h.listToolSessions(ctx, caller, request.Arguments)
		case "read_session":
			result, err = h.readToolSession(ctx, caller, request.Arguments)
		default:
			result, err = h.writeToolSession(ctx, caller, request)
		}
		return err
	}
	if caller.Origin != nil {
		if h.channels == nil {
			return "", errors.New("channel authorization is unavailable")
		}
		err = h.channels.WithSessionToolAccess(ctx, id, caller.Origin, caller.WorkspaceID, spec.Write, run)
	} else {
		err = run()
	}
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(result)
	return string(encoded), err
}

func decodeSessionToolArguments(raw json.RawMessage, target any) error {
	if len(raw) > 16*1024 {
		return errors.New("tool arguments exceed 16 KiB")
	}
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("tool arguments must be an object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid tool arguments: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("tool arguments contain trailing data")
	}
	return nil
}

func toolLimit(value int) (int, error) {
	if value == 0 {
		return 20, nil
	}
	if value < 1 || value > 100 {
		return 0, errors.New("limit must be between 1 and 100")
	}
	return value, nil
}

func (h *handler) authorizeToolTarget(ctx context.Context, caller sessionToolCaller, id string, read bool) error {
	if id == "" {
		return errors.New("session_id is required")
	}
	if read && caller.Origin != nil && id != caller.SessionID {
		return errors.New("channel history is restricted to its bound session")
	}
	organization, err := h.catalog.GetSessionOrganization(ctx, id)
	if err != nil {
		return errors.New("target session is unavailable")
	}
	if organization.WorkspaceID == nil || *organization.WorkspaceID != caller.WorkspaceID || organization.EffectiveArchived() {
		return errors.New("target session is outside the authorized workspace")
	}
	return nil
}

type toolSessionSummary struct {
	ID                   string       `json:"session_id"`
	Title                string       `json:"title,omitempty"`
	WorkspaceID          string       `json:"workspace_id"`
	Agent                string       `json:"agent"`
	State                SessionState `json:"state"`
	LastMatchedMessageAt *time.Time   `json:"last_matched_message_at,omitempty"`
}

func toolTimeWindow(since, until string) (session.DisplayTimeWindow, error) {
	var window session.DisplayTimeWindow
	for _, field := range []struct {
		value  string
		target **time.Time
	}{{since, &window.Since}, {until, &window.Until}} {
		if field.value == "" {
			continue
		}
		stamp, err := time.Parse(time.RFC3339Nano, field.value)
		if err != nil {
			return window, errors.New("time bounds must be RFC3339 timestamps with timezone")
		}
		if !time.Unix(0, stamp.UnixNano()).Equal(stamp) {
			return window, errors.New("time bounds are outside the supported nanosecond timestamp range")
		}
		*field.target = &stamp
	}
	if window.Since != nil && window.Until != nil && !window.Since.Before(*window.Until) {
		return window, errors.New("since must precede until")
	}
	return window, nil
}

func (h *handler) listToolSessions(ctx context.Context, caller sessionToolCaller, raw json.RawMessage) (any, error) {
	var req struct {
		Limit         int    `json:"limit"`
		AfterID       string `json:"after_id"`
		ActivitySince string `json:"activity_since"`
		ActivityUntil string `json:"activity_until"`
	}
	if err := decodeSessionToolArguments(raw, &req); err != nil {
		return nil, err
	}
	limit, err := toolLimit(req.Limit)
	if err != nil {
		return nil, err
	}
	window, err := toolTimeWindow(req.ActivitySince, req.ActivityUntil)
	if err != nil {
		return nil, err
	}
	organizations, err := h.catalog.ListSessionOrganizations(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(organizations, func(i, j int) bool { return organizations[i].SessionID < organizations[j].SessionID })
	results := make([]toolSessionSummary, 0, limit)
	hasMore := false
	for _, org := range organizations {
		if org.SessionID <= req.AfterID || org.WorkspaceID == nil || *org.WorkspaceID != caller.WorkspaceID || org.EffectiveArchived() || (caller.Origin != nil && org.SessionID != caller.SessionID) {
			continue
		}
		var matched *time.Time
		if window.Since != nil || window.Until != nil {
			indexed, ok := h.store.(interface {
				SessionDialogueActivity(context.Context, string, session.DisplayTimeWindow) (*time.Time, error)
			})
			if !ok {
				return nil, errors.New("time filtering requires indexed history storage")
			}
			matched, err = indexed.SessionDialogueActivity(ctx, org.SessionID, window)
			if err != nil {
				return nil, err
			}
			if matched == nil {
				continue
			}
		}
		var stored *session.Metadata
		if indexed, ok := h.store.(interface {
			GetMetadata(context.Context, string) (*session.Metadata, error)
		}); ok {
			stored, err = indexed.GetMetadata(ctx, org.SessionID)
		} else {
			var full *session.Session
			full, err = h.store.Get(ctx, org.SessionID)
			if full != nil {
				stored = &full.Metadata
			}
		}
		if err != nil {
			return nil, err
		}
		if stored == nil {
			continue
		}
		if len(results) == limit {
			hasMore = true
			break
		}
		row := toolSessionSummary{ID: org.SessionID, WorkspaceID: caller.WorkspaceID, Agent: stored.Agent, State: SessionStateOffline}
		row.LastMatchedMessageAt = matched
		if org.Title != nil {
			row.Title = boundedToolText(*org.Title, 200)
		}
		if live, ok := h.registry.Get(org.SessionID); ok {
			row.State = live.State
		}
		results = append(results, row)
	}
	next := ""
	if hasMore {
		next = results[len(results)-1].ID
	}
	return map[string]any{"sessions": results, "next_after_id": next}, nil
}

func boundedToolText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	text = text[:limit]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}

type toolHistoryItem struct {
	Sequence  uint64                  `json:"sequence"`
	Type      session.DisplayItemType `json:"type"`
	Text      string                  `json:"text,omitempty"`
	Truncated bool                    `json:"truncated,omitempty"`
	Timestamp time.Time               `json:"timestamp"`
	TurnID    string                  `json:"turn_id,omitempty"`
}

func (h *handler) readToolSession(ctx context.Context, caller sessionToolCaller, raw json.RawMessage) (any, error) {
	var req struct {
		SessionID string  `json:"session_id"`
		Limit     int     `json:"limit"`
		After     *uint64 `json:"after"`
		Before    *uint64 `json:"before"`
		Since     string  `json:"since"`
		Until     string  `json:"until"`
	}
	if err := decodeSessionToolArguments(raw, &req); err != nil {
		return nil, err
	}
	if req.After != nil && req.Before != nil {
		return nil, errors.New("after and before are mutually exclusive")
	}
	limit, err := toolLimit(req.Limit)
	if err != nil {
		return nil, err
	}
	if err := h.authorizeToolTarget(ctx, caller, req.SessionID, true); err != nil {
		return nil, err
	}
	window, err := toolTimeWindow(req.Since, req.Until)
	if err != nil {
		return nil, err
	}
	query := session.DisplayPageQuery{Limit: limit}
	if req.After != nil {
		query.After = *req.After
		query.HasAfter = true
	}
	if req.Before != nil {
		query.Before = *req.Before
		query.HasBefore = true
	}
	var page session.DisplayPage
	if indexed, ok := h.store.(interface {
		ReadDisplayPage(context.Context, string, session.DisplayPageQuery, session.DisplayTimeWindow) (session.DisplayPage, bool, error)
	}); ok {
		var exists bool
		page, exists, err = indexed.ReadDisplayPage(ctx, req.SessionID, query, window)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, errSessionNotFound
		}
	} else {
		if window.Since != nil || window.Until != nil {
			return nil, errors.New("time filtering requires indexed history storage")
		}
		items, exists, loadErr := h.items.LoadItems(ctx, req.SessionID)
		if loadErr != nil {
			return nil, loadErr
		}
		if !exists {
			return nil, errSessionNotFound
		}
		page = session.PageDisplayItems(items, query)
	}
	result := make([]toolHistoryItem, 0, len(page.Items))
	budget := 8 * 1024
	for _, item := range page.Items {
		row := toolHistoryItem{Sequence: item.Sequence, Type: item.Type, Timestamp: item.CreatedAt}
		if item.Turn != nil {
			row.TurnID = item.Turn.ID
		} else if item.ToolExecution != nil {
			row.TurnID = item.ToolExecution.TurnID
		} else if item.Command != nil {
			row.TurnID = item.Command.TurnID
		}
		if item.Type == session.DisplayItemUserMessage || item.Type == session.DisplayItemAssistantMessage || item.Type == session.DisplayItemSteeringMessage {
			for _, part := range item.Content {
				if part.Type == "text" {
					row.Text += part.Text
				}
			}
		}
		cap := min(8192, budget)
		text := boundedToolText(row.Text, cap)
		row.Truncated = len(text) < len(row.Text)
		row.Text = text
		budget -= len(text)
		result = append(result, row)
	}
	// 8 KiB of text leaves room for envelopes after worst-case JSON escaping.
	return map[string]any{"session_id": req.SessionID, "items": result, "next_cursor": page.NextCursor, "prev_cursor": page.PrevCursor, "has_more": page.HasMore}, nil
}

type sessionToolWriteInput struct {
	ForkFrom       *session.ForkOrigin `json:"fork_from,omitempty"`
	SessionID      string              `json:"session_id,omitempty"`
	WorkspaceID    string              `json:"workspace_id,omitempty"`
	Text           string              `json:"text,omitempty"`
	InitialMessage string              `json:"initial_message,omitempty"`
	Title          string              `json:"title,omitempty"`
}

type sessionToolOperation struct {
	ApprovalPolicy  agent.ApprovalPolicy     `json:"approval_policy"`
	PromptConfig    session.PromptConfig     `json:"prompt_config"`
	WorkspaceID     string                   `json:"workspace_id"`
	DispatchStarted bool                     `json:"dispatch_started,omitempty"`
	SourceSessionID string                   `json:"source_session_id"`
	SourceTurnID    string                   `json:"source_turn_id"`
	CallID          string                   `json:"call_id"`
	Name            string                   `json:"name"`
	Input           sessionToolWriteInput    `json:"input"`
	TargetID        string                   `json:"target_id"`
	Origin          *protocol.RequestContext `json:"origin,omitempty"`
}

func toolDigest(values ...string) string {
	encoded, _ := json.Marshal(values)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func (h *handler) writeToolSession(ctx context.Context, caller sessionToolCaller, request workerRuntimeToolRequest) (any, error) {
	var input sessionToolWriteInput
	if err := decodeSessionToolArguments(request.Arguments, &input); err != nil {
		return nil, err
	}
	text := input.Text
	if request.Name == "create_session" {
		if input.WorkspaceID != caller.WorkspaceID || input.SessionID != "" || input.Text != "" {
			return nil, errors.New("create requires the source workspace and no session_id or text")
		}
		text = input.InitialMessage
	} else if input.SessionID == "" || input.WorkspaceID != "" || input.InitialMessage != "" || input.Title != "" || input.ForkFrom != nil {
		return nil, errors.New("send requires session_id and text only")
	}
	if strings.TrimSpace(text) == "" || len(text) > 8192 || len(input.Title) > 200 {
		return nil, errors.New("message must be 1..8192 bytes; title at most 200 bytes")
	}
	if input.SessionID == caller.SessionID {
		return nil, errors.New("self-send is not allowed")
	}
	if input.ForkFrom != nil {
		if input.ForkFrom.ThroughTurnID == "" {
			return nil, errors.New("fork_from requires through_turn_id")
		}
		if err := h.authorizeToolTarget(ctx, caller, input.ForkFrom.SessionID, true); err != nil {
			return nil, err
		}
		source, err := h.store.Get(ctx, input.ForkFrom.SessionID)
		if err != nil {
			return nil, err
		}
		if source == nil || source.Agent != caller.Stored.Agent || source.ApprovalPolicy != caller.Stored.ApprovalPolicy || source.PromptConfig != caller.Stored.PromptConfig {
			return nil, errors.New("fork source runtime, approval and prompt constraints must match caller")
		}
	}
	root := h.sessionStoreRoot()
	if root == "" {
		return nil, errors.New("session tool writes require durable storage")
	}
	turnKey := toolDigest(caller.SessionID, caller.TurnID)
	callKey := toolDigest(request.CallID)
	unlock := h.sessionOps.lock("runtime-tool:" + turnKey)
	defer unlock()
	if _, err := h.resolveSessionToolCaller(ctx, caller.SessionID, caller.Generation, caller.TurnID); err != nil {
		return nil, err
	}
	dir := filepath.Join(root, "runtime-tool-operations", turnKey)
	path := filepath.Join(dir, callKey+".json")
	operation := sessionToolOperation{ApprovalPolicy: caller.Stored.ApprovalPolicy, PromptConfig: caller.Stored.PromptConfig, WorkspaceID: caller.WorkspaceID, SourceSessionID: caller.SessionID, SourceTurnID: caller.TurnID, CallID: request.CallID, Name: request.Name, Input: input, TargetID: input.SessionID, Origin: caller.Origin}
	if request.Name == "create_session" {
		operation.TargetID = "sess_tool_" + toolDigest(turnKey, callKey)
	}
	data, err := os.ReadFile(path)
	if err == nil {
		var existing sessionToolOperation
		if err = json.Unmarshal(data, &existing); err != nil {
			return nil, err
		}
		if existing.SourceSessionID != operation.SourceSessionID || existing.SourceTurnID != operation.SourceTurnID || existing.CallID != operation.CallID || existing.Name != operation.Name || !reflect.DeepEqual(existing.Input, input) || existing.TargetID != operation.TargetID {
			return nil, errCommandIDConflict
		}
		operation = existing
	} else if errors.Is(err, os.ErrNotExist) {
		entries, readErr := os.ReadDir(dir)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return nil, readErr
		}
		if len(entries) >= 20 {
			return nil, errors.New("session tool mutation limit reached for this turn")
		}
		if err = saveSessionToolOperation(dir, path, operation); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	if operation.WorkspaceID != caller.WorkspaceID || operation.ApprovalPolicy != caller.Stored.ApprovalPolicy || operation.PromptConfig != caller.Stored.PromptConfig {
		return nil, errors.New("source policy changed since the operation was recorded")
	}
	if request.Name == "create_session" {
		if err = h.createToolSession(ctx, caller, operation); err != nil {
			if errors.Is(err, errForkOutcomeUnknown) {
				return uncertainSessionToolResult(operation.TargetID, "session_tool:"+turnKey+":"+callKey), nil
			}
			return nil, err
		}
	}
	if err = h.authorizeToolTarget(ctx, caller, operation.TargetID, false); err != nil {
		return nil, err
	}
	target, err := h.store.Get(ctx, operation.TargetID)
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, errSessionNotFound
	}
	commandID := "session_tool:" + turnKey + ":" + callKey
	previous, found, err := findExistingSessionInput(ctx, h.items, operation.TargetID, commandResponse{ID: commandID, Message: &messageCommandPayload{Text: text, RequestContext: caller.Origin}}, root)
	if err != nil {
		return nil, err
	}
	if found {
		return acceptedSessionToolResult(operation, previous.ID), nil
	}
	if operation.DispatchStarted {
		return uncertainSessionToolResult(operation.TargetID, commandID), nil
	}
	if target.ApprovalPolicy != caller.Stored.ApprovalPolicy || target.PromptConfig != caller.Stored.PromptConfig {
		return nil, errors.New("target approval and prompt constraints must match the source")
	}
	// Channel-bound targets are not task inboxes: running here would bypass their
	// delivery lifecycle and could expose output to another group.
	if target.Capabilities.ChannelToolsVersion > 0 {
		return nil, errors.New("cannot send delegated work to a Channel-bound session")
	}
	if _, err = h.ensureSessionRunningInWorkspace(ctx, operation.TargetID); err != nil {
		return nil, err
	}
	if !h.ensureWorkerOnline(ctx, operation.TargetID) {
		return nil, errWorkerOffline
	}
	if _, err := h.resolveSessionToolCaller(ctx, caller.SessionID, caller.Generation, caller.TurnID); err != nil {
		return nil, err
	}
	operation.DispatchStarted = true
	if err := saveSessionToolOperation(dir, path, operation); err != nil {
		return nil, err
	}
	accepted, err := h.acceptSessionInputCommand(ctx, operation.TargetID, commandID, text, nil, nil, "", false, true, &sessionInputConstraints{approvalPolicy: target.ApprovalPolicy, promptRevision: target.PromptConfig.Revision, requestContext: caller.Origin})
	if err != nil {
		if errors.Is(err, errActiveTurn) || errors.Is(err, errInputPolicyMismatch) || errors.Is(err, errWorkerInputStopping) {
			return nil, err
		}
		return uncertainSessionToolResult(operation.TargetID, commandID), nil
	}
	return acceptedSessionToolResult(operation, accepted.ID), nil
}

func acceptedSessionToolResult(operation sessionToolOperation, commandID string) any {
	result := map[string]any{"session_id": operation.TargetID, "command_id": commandID, "status": "accepted"}
	if operation.Input.ForkFrom != nil {
		result["forked_from"] = operation.Input.ForkFrom
	}
	return result
}

func uncertainSessionToolResult(targetID, commandID string) any {
	return map[string]any{"session_id": targetID, "command_id": commandID, "status": "outcome_unknown", "message": "Dispatch may have started work. Inspect the target; do not resubmit with a new call ID."}
}

func saveSessionToolOperation(dir, path string, operation any) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(operation)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".operation-")
	if err != nil {
		return err
	}
	// After rename the temporary path is absent; cleanup must not mask write errors.
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

func (h *handler) createToolSession(ctx context.Context, caller sessionToolCaller, operation sessionToolOperation) error {
	if origin := operation.Input.ForkFrom; origin != nil {
		_, err := h.forkSessionInWorkspace(ctx, operation.TargetID, origin.SessionID, origin.ThroughTurnID, operation.Input.Title, caller.WorkspaceID)
		return err
	}
	unlock := h.sessionOps.lock(operation.TargetID)
	defer unlock()
	stored, err := h.store.Get(ctx, operation.TargetID)
	if err != nil {
		return err
	}
	if stored == nil {
		created := newSession(caller.Stored.WorkingDirectory, caller.Stored.ProfileName)
		created.ID = operation.TargetID
		created.Agent = caller.Stored.Agent
		created.Model = caller.Stored.Model
		created.ReasoningEffort = caller.Stored.ReasoningEffort
		created.ApprovalPolicy = caller.Stored.ApprovalPolicy
		if err = h.persistSession(ctx, created); err != nil {
			return err
		}
		stored, err = h.store.Get(ctx, created.ID)
		if err != nil {
			return err
		}
	}
	// Retry can recover a crash between session persistence and assignment, but
	// must not overwrite settings of an already-visible or running session.
	org, err := h.catalog.GetSessionOrganization(ctx, operation.TargetID)
	if err != nil && !errors.Is(err, workspace.ErrNotFound) {
		return err
	}
	if errors.Is(err, workspace.ErrNotFound) || org.WorkspaceID == nil {
		stored.PromptConfig = caller.Stored.PromptConfig
		if err = h.store.Put(ctx, stored); err != nil {
			return err
		}
		if _, err = h.catalog.AssignSession(ctx, operation.TargetID, caller.WorkspaceID); err != nil {
			return err
		}
	}
	if operation.Input.Title != "" && org.Title == nil {
		if _, err = h.catalog.SetSessionTitle(ctx, operation.TargetID, operation.Input.Title); err != nil {
			return err
		}
	}
	return nil
}
