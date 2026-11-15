package zotigod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	channelruntime "github.com/jayyao97/zotigo/internal/channels"
	"github.com/jayyao97/zotigo/internal/codexapp"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

func respondCodexRequestError(app any, message codexapp.Message, code int, reason string) {
	if responder, ok := app.(interface {
		RespondError(json.RawMessage, int, string) error
	}); ok {
		_ = responder.RespondError(message.ID, code, reason)
	}
}

type codexRequestFatalError struct{ err error }

func (e *codexRequestFatalError) Error() string { return e.err.Error() }
func (e *codexRequestFatalError) Unwrap() error { return e.err }

func (r *codexWorkerRuntime) handleServerRequest(ctx context.Context, message codexapp.Message) error {
	switch message.Method {
	case "item/tool/requestUserInput", "tool/requestUserInput":
		return r.registerCodexUserInput(ctx, message)
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval":
		return r.registerCodexApproval(ctx, message)
	case "item/tool/call":
		return r.executeCodexRuntimeTool(ctx, message)
	default:
		return fmt.Errorf("codex server request %q is not supported", message.Method)
	}
}

func (r *codexWorkerRuntime) executeCodexRuntimeTool(ctx context.Context, message codexapp.Message) error {
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	var params struct {
		ThreadID  string          `json:"threadId"`
		TurnID    string          `json:"turnId"`
		CallID    string          `json:"callId"`
		Namespace *string         `json:"namespace"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := sonic.Unmarshal(message.Params, &params); err != nil {
		return fmt.Errorf("decode Codex runtime tool call: %w", err)
	}
	sessionTool := false
	if params.Namespace != nil && *params.Namespace == "zotigo" && r.cfg.SessionToolsVersion >= 1 {
		for _, spec := range workerbase.SessionToolSpecs() {
			if spec.Name == params.Tool {
				sessionTool = true
			}
		}
	}
	if !sessionTool && (r.cfg.ChannelToolsVersion < channelruntime.RuntimeToolsVersion || params.Namespace == nil || *params.Namespace != "channel" || params.Tool != channelruntime.RuntimeToolReadMessages) {
		return fmt.Errorf("codex runtime tool %q is not available", params.Tool)
	}
	if params.ThreadID != r.threadID || params.TurnID == "" || params.TurnID != r.activeTurnID || (!sessionTool && r.activeRequestContext == nil) {
		return errors.New("channel tool call is outside the active Channel turn")
	}
	if strings.TrimSpace(params.CallID) == "" {
		return errors.New("codex runtime tool call is missing callId")
	}
	if r.writer == nil || r.runtimeToolResults == nil {
		return &codexRequestFatalError{err: errors.New("channel runtime tool transport is unavailable")}
	}
	requestContext := r.activeRequestContext.Clone()
	if err := r.writer.SendRuntimeToolRequest(ctx, wire.RuntimeToolRequest{
		RequestID: params.CallID, Namespace: *params.Namespace, TurnID: params.TurnID, CallID: params.CallID, Name: params.Tool, Arguments: params.Arguments, RequestContext: requestContext,
	}); err != nil {
		return &codexRequestFatalError{err: fmt.Errorf("send Channel runtime tool request: %w", err)}
	}
	var result wire.RuntimeToolResult
	for {
		select {
		case <-ctx.Done():
			return &codexRequestFatalError{err: ctx.Err()}
		case candidate, ok := <-r.runtimeToolResults:
			if !ok {
				return &codexRequestFatalError{err: errors.New("channel runtime tool transport closed")}
			}
			if candidate.RequestID != params.CallID {
				continue
			}
			result = candidate
		}
		break
	}
	responder, ok := r.app.(interface {
		RespondResult(json.RawMessage, any) error
	})
	if !ok {
		return &codexRequestFatalError{err: errors.New("codex client cannot answer runtime tool calls")}
	}
	text := result.Text
	success := result.Error == ""
	if !success {
		text = result.Error
	}
	if err := responder.RespondResult(message.ID, map[string]any{
		"success":      success,
		"contentItems": []map[string]any{{"type": "inputText", "text": text}},
	}); err != nil {
		return &codexRequestFatalError{err: fmt.Errorf("answer Codex runtime tool call: %w", err)}
	}
	return nil
}

func (r *codexWorkerRuntime) registerCodexUserInput(ctx context.Context, message codexapp.Message) error {
	type question struct {
		ID       string                   `json:"id"`
		Header   string                   `json:"header"`
		Question string                   `json:"question"`
		IsOther  bool                     `json:"isOther"`
		IsSecret bool                     `json:"isSecret"`
		Options  []wire.InteractionOption `json:"options"`
	}
	var params struct {
		ThreadID         string     `json:"threadId"`
		TurnID           string     `json:"turnId"`
		ItemID           string     `json:"itemId"`
		Questions        []question `json:"questions"`
		IsBlocking       *bool      `json:"isBlocking"`
		AutoResolutionMS *uint64    `json:"autoResolutionMs"`
	}
	if err := sonic.Unmarshal(message.Params, &params); err != nil {
		return fmt.Errorf("decode Codex user input request: %w", err)
	}
	blocking := true
	if params.IsBlocking != nil {
		blocking = *params.IsBlocking
	}
	requester := wire.InteractionRequester{Agent: "codex", ThreadID: params.ThreadID}
	if params.ThreadID != "" && params.ThreadID != r.threadID {
		requester.Name = "Subagent"
	}
	questions := make([]wire.InteractionQuestion, 0, len(params.Questions))
	for _, item := range params.Questions {
		questions = append(questions, wire.InteractionQuestion(item))
	}
	req, err := sessionlog.NewUserInputInteraction(r.cfg.SessionID, params.TurnID, params.ItemID, requester, questions, blocking, params.AutoResolutionMS)
	if err != nil {
		return err
	}
	if err := r.append(sessionlog.InteractionItem(req, false)); err != nil {
		return fmt.Errorf("record Codex user input request: %w", err)
	}
	r.interactions[req.ID] = codexPendingInteraction{request: req, rpcID: append(json.RawMessage(nil), message.ID...)}
	if err := r.writer.SendInteractionRequest(ctx, req); err != nil {
		delete(r.interactions, req.ID)
		return &codexRequestFatalError{err: errors.Join(err, r.expireInteraction(req))}
	}
	return nil
}

func (r *codexWorkerRuntime) resolveInteraction(response wire.InteractionResponse) (wire.InteractionResult, error) {
	pending, ok := r.interactions[response.InteractionID]
	if !ok {
		return wire.InteractionResult{RequestID: response.RequestID, Error: "interaction is not pending"}, nil
	}
	if err := sessionlog.ValidateInteractionAnswers(pending.request, response.Answers); err != nil {
		return wire.InteractionResult{RequestID: response.RequestID, Error: err.Error()}, nil
	}
	responder, ok := r.app.(interface {
		RespondResult(json.RawMessage, any) error
	})
	if !ok {
		return wire.InteractionResult{RequestID: response.RequestID, Error: "Codex client cannot answer server requests"}, nil
	}
	result := map[string]any{"answers": map[string]any{}}
	answers := result["answers"].(map[string]any)
	for questionID, values := range response.Answers {
		answers[questionID] = map[string]any{"answers": values}
	}
	if err := responder.RespondResult(pending.rpcID, result); err != nil {
		return wire.InteractionResult{RequestID: response.RequestID, Error: err.Error()}, nil
	}
	resolvedAt := time.Now().UTC()
	delete(r.interactions, response.InteractionID)
	req := pending.request
	req.Status = sessionlog.InteractionStatusResolved
	req.Answers = response.Answers
	req.ResolvedAt = &resolvedAt
	if err := r.append(sessionlog.InteractionItem(req, true)); err != nil {
		result := wire.InteractionResult{RequestID: response.RequestID, Error: fmt.Sprintf("record interaction response: %v", err)}
		return result, fmt.Errorf("codex interaction resolved but could not be recorded: %w", err)
	}
	req.Answers = sessionlog.DisplayInteractionAnswers(req)
	return wire.InteractionResult{RequestID: response.RequestID, Interaction: &req}, nil
}

func (r *codexWorkerRuntime) registerCodexApproval(ctx context.Context, message codexapp.Message) error {
	var params codexApprovalRequestParams
	if err := sonic.Unmarshal(message.Params, &params); err != nil {
		return fmt.Errorf("decode Codex approval request: %w", err)
	}
	kind := codexApprovalDecision
	var permissionGrant json.RawMessage
	if message.Method == "item/permissions/requestApproval" {
		var permissions map[string]json.RawMessage
		if len(params.Permissions) == 0 || sonic.Unmarshal(params.Permissions, &permissions) != nil || permissions == nil {
			return fmt.Errorf("decode Codex permission approval: permissions must be an object")
		}
		kind = codexApprovalPermissions
		permissionGrant = append(json.RawMessage(nil), params.Permissions...)
	}
	callID := params.ItemID
	if params.ApprovalID != "" {
		callID += ":" + params.ApprovalID
	}
	name, arguments := "codex_command", ""
	switch message.Method {
	case "item/fileChange/requestApproval":
		name, arguments = "codex_file_change", params.GrantRoot
	case "item/permissions/requestApproval":
		name, arguments = "codex_permissions", string(params.Permissions)
	default:
		var encodeErr error
		arguments, encodeErr = codexCommandApprovalArguments(params)
		if encodeErr != nil {
			return encodeErr
		}
	}
	source := "codex"
	if params.ThreadID != "" && params.ThreadID != r.threadID {
		source = "codex_subagent:" + params.ThreadID
	}
	req, err := sessionlog.NewApprovalRequest(r.cfg.SessionID, params.TurnID, []zotigosession.DisplayPendingApproval{{
		ToolCallID: callID, ToolName: name, Arguments: arguments, Description: params.CWD, Reason: params.Reason, Source: source,
	}})
	if err != nil {
		return err
	}
	item := zotigosession.DisplayItem{Type: zotigosession.DisplayItemApprovalRequest, Turn: &zotigosession.DisplayTurn{ID: req.TurnID}, Approval: &zotigosession.DisplayApproval{ID: req.ID, TurnID: req.TurnID, Pending: sessionlog.CopyPendingApprovals(req.Pending)}}
	if err := r.append(item); err != nil {
		return fmt.Errorf("record Codex approval request: %w", err)
	}
	approve, deny := "", ""
	if kind == codexApprovalDecision {
		approve, deny = codexSimpleApprovalDecisions(message.Method, params.AvailableDecisions)
	}
	pending := codexPendingApproval{request: req, rpcID: append(json.RawMessage(nil), message.ID...), kind: kind, permissions: permissionGrant, approve: approve, deny: deny}
	r.approvals[req.ID] = pending
	if err := r.writer.SendApprovalRequest(ctx, sessionlog.PublicApprovalRequest(req)); err != nil {
		delete(r.approvals, req.ID)
		return &codexRequestFatalError{err: errors.Join(err, r.expireApproval(req, "Codex approval could not be delivered"))}
	}
	return nil
}

func (r *codexWorkerRuntime) resolveCodexApproval(decision wire.ApprovalDecision) (wire.ApprovalResult, error) {
	pending, ok := r.approvals[decision.ApprovalID]
	if !ok {
		return wire.ApprovalResult{RequestID: decision.RequestID, Error: "approval is not pending"}, nil
	}
	if err := sessionlog.ValidateApprovalDecisions(pending.request.Pending, decision.Decisions); err != nil {
		return wire.ApprovalResult{RequestID: decision.RequestID, Error: err.Error()}, nil
	}
	approved := decision.Decisions[0].Approved
	responder, ok := r.app.(interface {
		RespondResult(json.RawMessage, any) error
	})
	if !ok {
		return wire.ApprovalResult{RequestID: decision.RequestID, Error: "Codex client cannot answer server requests"}, nil
	}
	var answer any
	if pending.kind == codexApprovalPermissions {
		permissions := json.RawMessage(`{}`)
		if approved {
			permissions = pending.permissions
		}
		answer = map[string]any{"permissions": permissions, "scope": "turn"}
	} else {
		answerDecision := pending.deny
		if approved {
			answerDecision = pending.approve
		}
		if answerDecision == "" {
			return wire.ApprovalResult{RequestID: decision.RequestID, Error: "Codex did not offer a compatible approval decision"}, nil
		}
		answer = map[string]any{"decision": answerDecision}
	}
	if err := responder.RespondResult(pending.rpcID, answer); err != nil {
		return wire.ApprovalResult{RequestID: decision.RequestID, Error: err.Error()}, nil
	}
	resolvedAt := time.Now().UTC()
	delete(r.approvals, decision.ApprovalID)
	req := sessionlog.ResolvedApprovalFromDecision(pending.request, decision.Decisions, resolvedAt)
	if err := r.append(zotigosession.DisplayItem{Type: zotigosession.DisplayItemApprovalDecision, Turn: &zotigosession.DisplayTurn{ID: req.TurnID}, Approval: &zotigosession.DisplayApproval{ID: req.ID, TurnID: req.TurnID, Decisions: sessionlog.CopyApprovalDecisions(req.Decisions)}}); err != nil {
		result := wire.ApprovalResult{RequestID: decision.RequestID, Error: fmt.Sprintf("record approval decision: %v", err)}
		return result, fmt.Errorf("codex approval resolved but could not be recorded: %w", err)
	}
	response := sessionlog.PublicApprovalRequest(req)
	return wire.ApprovalResult{RequestID: decision.RequestID, Approval: &response}, nil
}

func codexCommandApprovalArguments(params codexApprovalRequestParams) (string, error) {
	if params.Kind == "" && params.EnvironmentID == "" && len(params.NetworkApprovalContext) == 0 && len(params.AdditionalPermissions) == 0 {
		return params.Command, nil
	}
	details := struct {
		Kind                   string          `json:"kind,omitempty"`
		EnvironmentID          string          `json:"environment_id,omitempty"`
		Command                string          `json:"command,omitempty"`
		NetworkApprovalContext json.RawMessage `json:"network_approval_context,omitempty"`
		AdditionalPermissions  json.RawMessage `json:"additional_permissions,omitempty"`
	}{
		Kind: params.Kind, EnvironmentID: params.EnvironmentID, Command: params.Command,
		NetworkApprovalContext: params.NetworkApprovalContext, AdditionalPermissions: params.AdditionalPermissions,
	}
	encoded, err := sonic.Marshal(details)
	if err != nil {
		return "", fmt.Errorf("encode Codex approval details: %w", err)
	}
	return string(encoded), nil
}

func codexSimpleApprovalDecisions(method string, available []json.RawMessage) (string, string) {
	if method != "item/commandExecution/requestApproval" || available == nil {
		return "accept", "cancel"
	}
	approve, deny := "", ""
	for _, raw := range available {
		var candidate string
		if sonic.Unmarshal(raw, &candidate) != nil {
			continue
		}
		switch candidate {
		case "accept":
			approve = candidate
		case "decline", "cancel":
			if deny == "" {
				deny = candidate
			}
		}
	}
	return approve, deny
}

func (r *codexWorkerRuntime) expireInteraction(req wire.InteractionRequest) error {
	now := time.Now().UTC()
	req.Status = sessionlog.InteractionStatusExpired
	req.ResolvedAt = &now
	return r.append(sessionlog.InteractionItem(req, true))
}

func (r *codexWorkerRuntime) expireApproval(req sessionlog.ApprovalRequest, reason string) error {
	decisions := make([]zotigosession.DisplayApprovalDecision, 0, len(req.Pending))
	for _, pending := range req.Pending {
		decisions = append(decisions, zotigosession.DisplayApprovalDecision{
			ToolCallID: pending.ToolCallID, Approved: false, Reason: reason,
		})
	}
	return r.append(zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemApprovalDecision, Turn: &zotigosession.DisplayTurn{ID: req.TurnID},
		Approval: &zotigosession.DisplayApproval{ID: req.ID, TurnID: req.TurnID, Decisions: decisions},
	})
}
