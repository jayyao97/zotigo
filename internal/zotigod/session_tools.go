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
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	session "github.com/jayyao97/zotigo/core/session"
	workspace "github.com/jayyao97/zotigo/core/workspace"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

type sessionToolCaller struct {
	SessionID, TurnID, WorkspaceID string
	Generation                     string
	Origin                         *protocol.RequestContext
	CanReadWorkspaceSessions       bool
	Stored                         *session.Session
}

// Resolve authority from durable host input, never from model arguments or the
// worker's echoed RequestContext. A generation identifies a worker incarnation.
func (h *handler) resolveSessionToolCaller(ctx context.Context, id, generation, turnID string) (sessionToolCaller, error) {
	return h.resolveRuntimeToolCaller(ctx, id, generation, turnID, true, "")
}

func (h *handler) resolveRuntimeToolCaller(ctx context.Context, id, generation, turnID string, sessionScope bool, watchReadID string) (sessionToolCaller, error) {
	caller := sessionToolCaller{SessionID: id, TurnID: turnID, Generation: generation}
	if h.workers == nil || !h.workers.Matches(id, generation) || turnID == "" {
		return caller, errors.New("runtime tool caller is not the active worker")
	}
	stored, err := h.store.Get(ctx, id)
	if err != nil {
		return caller, err
	}
	if stored == nil {
		return caller, workerbase.ErrSessionNotFound
	}
	items, exists, err := h.items.LoadItems(ctx, id)
	if err != nil {
		return caller, err
	}
	if !exists || sessionlog.LastOpenTurnID(items) != turnID {
		return caller, errors.New("runtime tool caller turn is no longer active")
	}
	var pending, input *session.DisplayItem
	started := false
	for index := range items {
		item := &items[index]
		if item.Command != nil && item.Command.Type == wire.CommandMessage {
			pending = item
		}
		if item.Type == session.DisplayItemTurnStarted && item.Turn != nil && item.Turn.ID == turnID {
			input = pending
			started = true
		}
		// Steering from the same principal preserves the turn's authority. Do
		// not let a different sender borrow the initiating input's grants.
		if sessionScope && started && item.Command != nil && item.Command.Type == wire.CommandSteering {
			// Host-created callback feedback contributes data, not a new principal.
			// Preserve the initiating user's authority only for a verified receipt;
			// callback-initiated turns still hit the delegation fence below.
			if strings.HasPrefix(item.ID, "completion:") && h.catalog != nil {
				trigger, err := h.catalog.CompletionTrigger(ctx, strings.TrimPrefix(item.ID, "completion:"))
				if err != nil {
					return caller, err
				}
				if trigger.SessionID != id || trigger.State != "fired" || trigger.DeliveryMode != "steer" ||
					item.Command.Text != completionPrompt(trigger.OnFire, trigger.CallbackText) ||
					!sessionlog.SameRequestContext(item.Command.RequestContext, completionOrigin(trigger)) {
					return caller, errors.New("completion steering has no matching host receipt")
				}
				continue
			}
			if input == nil || input.Command == nil ||
				strings.HasPrefix(item.ID, "session_tool:") ||
				(strings.HasPrefix(item.ID, "channel:") && item.Command.RequestContext == nil) ||
				!sameSessionToolPrincipal(input.Command.RequestContext, item.Command.RequestContext) {
				return caller, errors.New("session tools require steering input from the same trusted principal")
			}
		}
	}
	if input == nil || input.Command == nil {
		return caller, errors.New("runtime tool caller has no durable host input")
	}
	if sessionScope && strings.HasPrefix(input.ID, "session_tool:") {
		return caller, errors.New("delegated tasks cannot invoke session tools again")
	}
	if sessionScope && strings.HasPrefix(input.ID, "completion:") {
		if watchReadID == "" || h.catalog == nil {
			return caller, errors.New("delegated tasks cannot invoke session tools again")
		}
		trigger, err := h.catalog.CompletionTrigger(ctx, strings.TrimPrefix(input.ID, "completion:"))
		if err != nil {
			return caller, err
		}
		if trigger.WatchSessionID != watchReadID || trigger.SessionID != id || trigger.State != "fired" ||
			input.Command.Text != completionPrompt(trigger.OnFire, trigger.CallbackText) ||
			!sessionlog.SameRequestContext(input.Command.RequestContext, completionOrigin(trigger)) {
			return caller, errors.New("completion input has no matching watch receipt for the requested session")
		}
		// A verified session watch originated from a local user. Its receipt
		// grants one target for read_session while that user's policy still holds.
		if _, err := h.validateCompletionTarget(ctx, trigger); err != nil {
			return caller, err
		}
	} else {
		caller.Origin = input.Command.RequestContext.Clone()
	}
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

func sameSessionToolPrincipal(left, right *protocol.RequestContext) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Source == right.Source && left.ConnectionID == right.ConnectionID &&
		left.ConversationID == right.ConversationID && left.ExternalConversation == right.ExternalConversation &&
		left.Actor.ID != "" && left.Actor.ID == right.Actor.ID
}

func (h *handler) executeRuntimeTool(ctx context.Context, id, generation string, request wire.RuntimeToolRequest) (string, error) {
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
			turnID = sessionlog.LastOpenTurnID(items)
		}
		caller, err := h.resolveRuntimeToolCaller(ctx, id, generation, turnID, false, "")
		if err != nil {
			return "", err
		}
		value, err := h.channels.ExecuteRuntimeTool(ctx, id, caller.Origin, request.Name, request.Arguments)
		return value.Text, err
	}
	if request.Namespace != "zotigo" || request.CallID == "" || len(request.CallID) > 512 {
		return "", errors.New("invalid runtime tool identity")
	}
	var spec *workerbase.RuntimeToolSpec
	for _, candidate := range workerbase.SessionToolSpecs() {
		if candidate.Name == request.Name {
			copy := candidate
			spec = &copy
			break
		}
	}
	if spec == nil {
		return "", errors.New("unknown session tool")
	}
	var readTarget struct {
		SessionID string `json:"session_id"`
	}
	if request.Name == "read_session" {
		if err := json.Unmarshal(request.Arguments, &readTarget); err != nil {
			return "", err
		}
	}
	caller, err := h.resolveRuntimeToolCaller(ctx, id, generation, request.TurnID, true, readTarget.SessionID)
	if err != nil {
		return "", err
	}
	if caller.Stored.Capabilities.SessionToolsVersion < 1 {
		return "", errors.New("session tools were not installed for this session")
	}
	// Queue operations acquire this lock before Channel and workspace authorization.
	var queueTarget string
	switch request.Name {
	case "send_message":
		var input sessionToolWriteInput
		if err := decodeSessionToolArguments(request.Arguments, &input); err != nil {
			return "", err
		}
		queueTarget = input.SessionID
	case "create_completion_trigger":
		var input struct {
			WatchSessionID string `json:"watch_session_id"`
		}
		if err := json.Unmarshal(request.Arguments, &input); err != nil {
			return "", err
		}
		if caller.Origin == nil && caller.Stored.Capabilities.ChannelToolsVersion == 0 {
			queueTarget = input.WatchSessionID
		}
	}
	if queueTarget != "" {
		unlock := h.queueOps.lock(queueTarget)
		defer unlock()
	}
	var result any
	run := func() error {
		// The same mandatory guard covers both runtimes. A configuration fence in
		// channels additionally prevents owner revocation racing durable admission.
		if !h.workers.Matches(id, generation) {
			return errors.New("stale runtime tool worker")
		}
		destination := caller.WorkspaceID
		if request.Name == "create_session" || request.Name == "list_runtimes" {
			var routing struct {
				WorkspaceID string `json:"workspace_id"`
			}
			if err := json.Unmarshal(request.Arguments, &routing); err != nil {
				return err
			}
			if routing.WorkspaceID != "" {
				destination = routing.WorkspaceID
			}
			if caller.Origin != nil && destination != caller.WorkspaceID {
				return errors.New("channel tasks must stay in the authorized workspace")
			}
		}
		spaces, release, err := h.lockSessionWorkspaces(ctx, caller.WorkspaceID, destination)
		if err != nil {
			return err
		}
		defer release()
		if spaces[caller.WorkspaceID].Status != workspace.WorkspaceStatusReady || spaces[destination].Status != workspace.WorkspaceStatusReady {
			return errors.New("source workspace is not ready")
		}
		fresh, err := h.resolveRuntimeToolCaller(ctx, id, generation, request.TurnID, true, readTarget.SessionID)
		if err != nil {
			return err
		}
		if fresh.WorkspaceID != caller.WorkspaceID {
			return errors.New("source workspace changed during authorization")
		}
		switch request.Name {
		case "create_completion_trigger":
			result, err = h.createCompletionTrigger(ctx, fresh, request)
		case "list_runtimes":
			dir := fresh.Stored.WorkingDirectory
			if destination != fresh.WorkspaceID {
				dir = spaces[destination].RootPath
			}
			result, err = h.listToolRuntimes(ctx, dir, request.Arguments)
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
		err = h.channels.WithSessionToolAccess(ctx, id, caller.Origin, caller.WorkspaceID, spec.Write, func(isOwner bool) error {
			caller.CanReadWorkspaceSessions = isOwner
			return run()
		})
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
	if read && caller.Origin != nil && !caller.CanReadWorkspaceSessions && id != caller.SessionID {
		return errors.New("channel history is restricted to its bound session")
	}
	organization, err := h.catalog.GetSessionOrganization(ctx, id)
	if err != nil {
		return errors.New("target session is unavailable")
	}
	// Local UI input may read across the daemon's catalog. Channel authority and
	// all mutations remain tied to the source workspace.
	workspaceScoped := !read || caller.Origin != nil
	if organization.WorkspaceID == nil {
		// Workspace deletion archives its sessions and clears their workspace FK.
		if !workspaceScoped && organization.SelfArchivedAt != nil && organization.DeletedWorkspaceID != "" {
			return nil
		}
		return errors.New("target session is outside the authorized workspace")
	}
	if (!read && organization.EffectiveArchived()) || (workspaceScoped && *organization.WorkspaceID != caller.WorkspaceID) {
		return errors.New("target session is outside the authorized workspace")
	}
	return nil
}

type toolSessionSummary struct {
	Archived             bool         `json:"archived"`
	ID                   string       `json:"session_id"`
	Title                string       `json:"title,omitempty"`
	WorkspaceID          string       `json:"workspace_id"`
	ProjectID            string       `json:"project_id"`
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
	var req sessionDiscoveryQuery
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
	req.Limit = limit
	results, next, err := h.discoverToolSessions(ctx, caller, req, window)
	if err != nil {
		return nil, err
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
	Sequence       uint64                  `json:"sequence"`
	Type           session.DisplayItemType `json:"type"`
	Text           string                  `json:"text,omitempty"`
	Truncated      bool                    `json:"truncated,omitempty"`
	Timestamp      time.Time               `json:"timestamp"`
	TurnID         string                  `json:"turn_id,omitempty"`
	Attachments    []toolHistoryAttachment `json:"attachments,omitempty"`
	TextOffset     int                     `json:"text_offset,omitempty"`
	NextTextOffset *int                    `json:"next_text_offset,omitempty"`
}

func (h *handler) readToolSession(ctx context.Context, caller sessionToolCaller, raw json.RawMessage) (any, error) {
	var req struct {
		SessionID       string  `json:"session_id"`
		Limit           int     `json:"limit"`
		After           *uint64 `json:"after"`
		Before          *uint64 `json:"before"`
		Since           string  `json:"since"`
		Until           string  `json:"until"`
		IncludeTools    bool    `json:"include_tools"`
		MessageSequence *uint64 `json:"message_sequence"`
		TextOffset      int     `json:"text_offset"`
	}
	if err := decodeSessionToolArguments(raw, &req); err != nil {
		return nil, err
	}
	if req.After != nil && req.Before != nil {
		return nil, errors.New("after and before are mutually exclusive")
	}
	if req.TextOffset < 0 || req.TextOffset != 0 && req.MessageSequence == nil {
		return nil, errors.New("text_offset must be non-negative and requires message_sequence")
	}
	if req.MessageSequence != nil && (*req.MessageSequence == 0 || req.After != nil || req.Before != nil || req.Limit != 0) {
		return nil, errors.New("message_sequence must be positive and cannot be combined with after, before or limit")
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
	query := session.DisplayPageQuery{Limit: limit, ContentKind: session.DisplayContentConversation, IncludeTools: req.IncludeTools}
	if req.After != nil {
		query.After = *req.After
		query.HasAfter = true
	}
	if req.Before != nil {
		query.Before = *req.Before
		query.HasBefore = true
	}
	if req.MessageSequence != nil {
		query.After = *req.MessageSequence - 1
		query.HasAfter = true
		query.Limit = 1
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
			return nil, workerbase.ErrSessionNotFound
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
			return nil, workerbase.ErrSessionNotFound
		}
		page = session.PageDisplayItems(items, query)
	}
	if req.MessageSequence != nil {
		if len(page.Items) != 1 || page.Items[0].Sequence != *req.MessageSequence {
			return nil, errors.New("conversation message not found in the requested time range")
		}
		page.NextCursor, page.PrevCursor, page.HasMore = "", "", false
	}
	result, err := projectToolHistory(&page, query.HasAfter, req.TextOffset, req.IncludeTools)
	if err != nil {
		return nil, err
	}
	return map[string]any{"session_id": req.SessionID, "items": result, "next_cursor": page.NextCursor, "prev_cursor": page.PrevCursor, "has_more": page.HasMore}, nil
}

type sessionToolWriteInput struct {
	Agent           string              `json:"agent,omitempty"`
	Model           string              `json:"model,omitempty"`
	Profile         string              `json:"profile,omitempty"`
	ReasoningEffort string              `json:"reasoning_effort,omitempty"`
	ForkFrom        *session.ForkOrigin `json:"fork_from,omitempty"`
	SessionID       string              `json:"session_id,omitempty"`
	WorkspaceID     string              `json:"workspace_id,omitempty"`
	Text            string              `json:"text,omitempty"`
	InitialMessage  string              `json:"initial_message,omitempty"`
	Title           string              `json:"title,omitempty"`
}

type sessionToolOperation struct {
	ApprovalPolicy  agent.ApprovalPolicy     `json:"approval_policy"`
	PromptConfig    session.PromptConfig     `json:"prompt_config"`
	WorkspaceID     string                   `json:"workspace_id"`
	DispatchStarted bool                     `json:"dispatch_started,omitempty"`
	QueueOnRetry    bool                     `json:"queue_on_retry,omitempty"`
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

func (h *handler) writeToolSession(ctx context.Context, caller sessionToolCaller, request wire.RuntimeToolRequest) (any, error) {
	var input sessionToolWriteInput
	if err := decodeSessionToolArguments(request.Arguments, &input); err != nil {
		return nil, err
	}
	text := input.Text
	if request.Name == "create_session" {
		if input.WorkspaceID == "" {
			input.WorkspaceID = caller.WorkspaceID
		}
		if (caller.Origin != nil && input.WorkspaceID != caller.WorkspaceID) || input.SessionID != "" || input.Text != "" {
			return nil, errors.New("create requires initial_message and an authorized destination workspace")
		}
		text = input.InitialMessage
	} else if input.Agent != "" || input.Model != "" || input.ReasoningEffort != "" || input.Profile != "" || input.SessionID == "" || input.WorkspaceID != "" || input.InitialMessage != "" || input.Title != "" || input.ForkFrom != nil {
		return nil, errors.New("send requires session_id and text only")
	}
	if strings.TrimSpace(text) == "" || len(text) > 8192 || len(input.Title) > 200 {
		return nil, errors.New("message must be 1..8192 bytes; title at most 200 bytes")
	}
	if input.SessionID == caller.SessionID {
		return nil, errors.New("self-send is not allowed")
	}
	if input.ForkFrom != nil {
		if input.WorkspaceID != caller.WorkspaceID || input.Agent != "" || input.Model != "" || input.Profile != "" || input.ReasoningEffort != "" {
			return nil, errors.New("fork uses the source workspace and runtime")
		}
		if input.ForkFrom.ThroughTurnID == "" {
			return nil, errors.New("fork_from requires through_turn_id")
		}
		if err := h.authorizeToolTarget(ctx, caller, input.ForkFrom.SessionID, true); err != nil {
			return nil, err
		}
		// Forking remains a same-workspace operation even when history is readable
		// across workspaces. Preserve the separate Channel read restriction too.
		if err := h.authorizeToolTarget(ctx, caller, input.ForkFrom.SessionID, false); err != nil {
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
			return nil, sessionlog.ErrCommandIDConflict
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
	targetScope := caller
	if operation.Name == "create_session" {
		targetScope.WorkspaceID = input.WorkspaceID
	}
	if err = h.authorizeToolTarget(ctx, targetScope, operation.TargetID, false); err != nil {
		return nil, err
	}
	target, err := h.store.Get(ctx, operation.TargetID)
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, workerbase.ErrSessionNotFound
	}
	commandID := "session_tool:" + turnKey + ":" + callKey
	previous, found, err := sessionlog.FindExistingInput(ctx, h.items, operation.TargetID, wire.Command{ID: commandID, Message: &wire.MessageCommandPayload{Text: text, RequestContext: caller.Origin}}, root)
	if err != nil {
		return nil, err
	}
	if found {
		return acceptedSessionToolResult(operation, previous.ID), nil
	}
	{
		row, err := h.catalog.QueuedInputByID(ctx, operation.TargetID, commandID)
		if err != nil {
			return nil, err
		}
		if row != nil && !row.Delegated {
			return nil, sessionlog.ErrCommandIDConflict
		}
		if row != nil {
			return map[string]any{"session_id": operation.TargetID, "command_id": commandID, "status": row.State}, nil
		}
		if operation.DispatchStarted && !operation.QueueOnRetry {
			return uncertainSessionToolResult(operation.TargetID, commandID), nil
		}
	}
	if target.ApprovalPolicy != caller.Stored.ApprovalPolicy || target.PromptConfig != caller.Stored.PromptConfig {
		return nil, errors.New("target approval and prompt constraints must match the source")
	}
	// Channel-bound targets are not task inboxes: running here would bypass their
	// delivery lifecycle and could expose output to another group.
	if target.Capabilities.ChannelToolsVersion > 0 {
		return nil, errors.New("cannot send delegated work to a Channel-bound session")
	}
	if operation.QueueOnRetry {
		return h.queueDelegatedMessage(ctx, operation, commandID)
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
		if request.Name == "send_message" && errors.Is(err, sessionlog.ErrActiveTurn) {
			// The worker definitively rejected direct admission. A replay may
			// now retry the idempotent queue insert even if storage was full.
			operation.QueueOnRetry = true
			if err := saveSessionToolOperation(dir, path, operation); err != nil {
				return nil, err
			}
			return h.queueDelegatedMessage(ctx, operation, commandID)

		}
		if errors.Is(err, sessionlog.ErrActiveTurn) || errors.Is(err, sessionlog.ErrInputPolicyMismatch) || errors.Is(err, errWorkerInputStopping) {
			return nil, err
		}
		return uncertainSessionToolResult(operation.TargetID, commandID), nil
	}
	return acceptedSessionToolResult(operation, accepted.ID), nil
}

func (h *handler) queueDelegatedMessage(ctx context.Context, operation sessionToolOperation, commandID string) (any, error) {
	text := operation.Input.Text
	rows, err := h.catalog.QueuedInputs(ctx, operation.TargetID)
	if err != nil {
		return nil, err
	}
	if err := validateQueueCapacity(rows, text, nil); err != nil {
		return nil, err
	}
	command := wire.Command{ID: commandID, Type: wire.CommandMessage, CreatedAt: time.Now().UTC(), Message: &wire.MessageCommandPayload{Text: text, RequestContext: operation.Origin}}
	data, err := json.Marshal(command)
	if err != nil {
		return nil, err
	}
	if err = h.catalog.QueueInput(ctx, operation.TargetID, commandID, data); err != nil {
		return nil, err
	}
	go func() { _ = h.dispatchQueuedInputs(operation.TargetID, false, true) }()
	return map[string]any{"session_id": operation.TargetID, "command_id": commandID, "status": "queued"}, nil
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
		wsID := operation.Input.WorkspaceID
		if wsID == "" {
			wsID = caller.WorkspaceID
		}
		ws, err := h.catalog.GetWorkspace(ctx, wsID)
		if err != nil {
			return err
		}
		dir := caller.Stored.WorkingDirectory
		if wsID != caller.WorkspaceID {
			dir = ws.RootPath
		}
		created := newSession(dir, caller.Stored.ProfileName)
		created.ID = operation.TargetID
		created.Agent = caller.Stored.Agent
		created.Model = caller.Stored.Model
		created.ReasoningEffort = caller.Stored.ReasoningEffort
		if err = h.selectToolRuntime(ctx, &created, operation.Input, caller.Stored.WorkingDirectory); err != nil {
			return err
		}
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
		targetWorkspace := operation.Input.WorkspaceID
		if targetWorkspace == "" {
			targetWorkspace = caller.WorkspaceID
		}
		if _, err = h.catalog.AssignSession(ctx, operation.TargetID, targetWorkspace); err != nil {
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

func (h *handler) selectToolRuntime(ctx context.Context, child *Session, in sessionToolWriteInput, sourceDir string) error {
	if in.Agent == "" && in.Profile == "" && in.Model == "" && in.ReasoningEffort == "" && child.WorkingDirectory == sourceDir {
		return nil
	}
	inheritProfile := child.Agent == "zotigo" && (in.Agent == "" || in.Agent == child.Agent) && in.Profile == ""
	if in.Agent != "" && in.Agent != child.Agent {
		child.Agent, child.ProfileName, child.Model, child.ReasoningEffort = in.Agent, "", "", ""
	}
	switch child.Agent {
	case "zotigo":
		if in.Model != "" || in.ReasoningEffort != "" {
			return errors.New("use profile to select a Zotigo API model")
		}
		cfg, err := config.NewManager().LoadForDir(child.WorkingDirectory)
		if err != nil {
			return err
		}
		if in.Profile != "" {
			child.ProfileName = in.Profile
		}
		name, profile, err := cfg.ResolveProfile(child.ProfileName)
		if err != nil {
			return err
		}
		if inheritProfile && sourceDir != child.WorkingDirectory {
			source, err := config.NewManager().LoadForDir(sourceDir)
			if err != nil {
				return err
			}
			_, original, err := source.ResolveProfile(child.ProfileName)
			if err != nil {
				return err
			}
			if profile.Provider != original.Provider || profile.Model != original.Model {
				return errors.New("the inherited profile selects a different model in the destination workspace; choose a destination profile explicitly")
			}
		}
		child.ProfileName = name
		return nil
	case "codex":
		if in.Profile != "" {
			return errors.New("profile is only valid for Zotigo")
		}
		if in.Model != "" && in.Model != child.Model {
			child.ReasoningEffort = ""
			child.Model = in.Model
		}
		if in.ReasoningEffort != "" {
			child.ReasoningEffort = in.ReasoningEffort
		}
		var err error
		child.Model, child.ReasoningEffort, err = h.resolveCodexSettings(ctx, child.Model, child.ReasoningEffort, true)
		return err
	default:
		return errors.New("unknown session runtime")
	}
}

func (h *handler) listToolRuntimes(ctx context.Context, dir string, raw json.RawMessage) (any, error) {
	var input struct {
		WorkspaceID string `json:"workspace_id"`
	}
	if err := decodeSessionToolArguments(raw, &input); err != nil {
		return nil, err
	}
	cfg, err := config.NewManager().LoadForDir(dir)
	if err != nil {
		return nil, err
	}
	profiles := make(map[string]string, len(cfg.Profiles))
	for name, profile := range cfg.Profiles {
		profiles[name] = profile.Model
	}
	result := map[string]any{"zotigo": map[string]any{"profiles": profiles, "default_profile": cfg.DefaultProfile}}
	if caps, err := h.codexCapabilities(ctx); err == nil {
		result["codex"] = codexCatalog(caps)
	}
	return result, nil
}
