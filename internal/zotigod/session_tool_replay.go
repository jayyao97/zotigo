package zotigod

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/jayyao97/zotigo/core/workspace"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

func (h *handler) authorizeDelegatedReplay(ctx context.Context, targetID string, raw json.RawMessage) (string, error) {
	var input struct {
		CommandID string `json:"command_id"`
	}
	if err := decodeSessionToolArguments(raw, &input); err != nil {
		return "", err
	}
	if strings.HasPrefix(input.CommandID, "completion:") {
		_, err := h.validateCompletionInput(ctx, targetID, input.CommandID)
		if err != nil {
			return "", err
		}
		value, err := h.catalog.CompletionTrigger(ctx, strings.TrimPrefix(input.CommandID, "completion:"))
		if err != nil {
			return "", err
		}
		_, found, err := sessionlog.FindExistingInput(ctx, h.items, targetID, wire.Command{ID: input.CommandID, Message: &wire.MessageCommandPayload{Text: completionPrompt(value.OnFire, value.CallbackText), RequestContext: completionOrigin(value)}}, h.sessionStoreRoot())
		if err != nil {
			return "", err
		}
		if !found {
			return "", errors.New("completion input is not durably admitted")
		}
		return `{"authorized":true}`, nil
	}
	err := h.withDelegatedInput(ctx, targetID, input.CommandID, false, func(op sessionToolOperation) error {
		text := op.Input.Text
		if op.Name == "create_session" {
			text = op.Input.InitialMessage
		}
		_, found, err := sessionlog.FindExistingInput(ctx, h.items, targetID, wire.Command{ID: input.CommandID, Message: &wire.MessageCommandPayload{Text: text, RequestContext: op.Origin}}, h.sessionStoreRoot())
		if err != nil {
			return err
		}
		if !found {
			return errors.New("delegated command is not durably admitted")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return `{"authorized":true}`, nil
}

func (h *handler) withDelegatedInput(ctx context.Context, targetID, commandID string, forAdmission bool, run func(sessionToolOperation) error) error {
	parts := strings.Split(commandID, ":")
	if len(parts) != 3 || parts[0] != "session_tool" {
		return errors.New("invalid delegated command ID")
	}
	for _, key := range parts[1:] {
		if decoded, err := hex.DecodeString(key); err != nil || len(decoded) != 32 {
			return errors.New("invalid delegated operation key")
		}
	}
	root := h.sessionStoreRoot()
	if root == "" {
		return errors.New("delegated operation storage is unavailable")
	}
	data, err := os.ReadFile(filepath.Join(root, "runtime-tool-operations", parts[1], parts[2]+".json"))
	if err != nil {
		return err
	}
	var operation sessionToolOperation
	if err := json.Unmarshal(data, &operation); err != nil {
		return err
	}
	if !operation.DispatchStarted || operation.TargetID != targetID || toolDigest(operation.SourceSessionID, operation.SourceTurnID) != parts[1] || toolDigest(operation.CallID) != parts[2] {
		return errors.New("delegated operation does not match the target")
	}
	validate := func() error {
		if forAdmission {
			ws, release, err := h.lockWorkspaceForUse(ctx, operation.WorkspaceID)
			if err != nil {
				return err
			}
			defer release()
			if ws.Status != workspace.WorkspaceStatusReady {
				return errors.New("delegated workspace is not ready")
			}
		}
		source, err := h.store.Get(ctx, operation.SourceSessionID)
		if err != nil {
			return err
		}
		if source == nil {
			return workerbase.ErrSessionNotFound
		}
		target, err := h.store.Get(ctx, targetID)
		if err != nil {
			return err
		}
		if target == nil {
			return workerbase.ErrSessionNotFound
		}
		caller := sessionToolCaller{SessionID: source.ID, WorkspaceID: operation.WorkspaceID, Stored: source, Origin: operation.Origin}

		if err := h.authorizeToolTarget(ctx, caller, source.ID, false); err != nil {
			return err
		}
		if err := h.authorizeToolTarget(ctx, caller, targetID, false); err != nil {
			return err
		}
		if source.ApprovalPolicy != operation.ApprovalPolicy || source.PromptConfig != operation.PromptConfig || target.ApprovalPolicy != operation.ApprovalPolicy || target.PromptConfig != operation.PromptConfig || target.Capabilities.ChannelToolsVersion != 0 {
			return errors.New("delegated target policy changed")
		}
		unavailable, err := h.ensureSessionActivatable(ctx, targetID)
		if err != nil {
			return err
		}
		if unavailable != "" {
			return errors.New("delegated target is unavailable")
		}
		return run(operation)
	}
	if operation.Origin != nil {
		if h.channels == nil {
			return errors.New("channel authorization is unavailable")
		}
		err = h.channels.WithSessionToolAccess(ctx, operation.SourceSessionID, operation.Origin, operation.WorkspaceID, true, func(_ bool) error { return validate() })
	} else {
		err = validate()
	}
	return err
}
