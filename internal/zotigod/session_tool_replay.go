package zotigod

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
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
	parts := strings.Split(input.CommandID, ":")
	if len(parts) != 3 || parts[0] != "session_tool" {
		return "", errors.New("invalid delegated command ID")
	}
	for _, key := range parts[1:] {
		if decoded, err := hex.DecodeString(key); err != nil || len(decoded) != 32 {
			return "", errors.New("invalid delegated operation key")
		}
	}
	root := h.sessionStoreRoot()
	if root == "" {
		return "", errors.New("delegated operation storage is unavailable")
	}
	data, err := os.ReadFile(filepath.Join(root, "runtime-tool-operations", parts[1], parts[2]+".json"))
	if err != nil {
		return "", err
	}
	var operation sessionToolOperation
	if err := json.Unmarshal(data, &operation); err != nil {
		return "", err
	}
	if !operation.DispatchStarted || operation.TargetID != targetID || toolDigest(operation.SourceSessionID, operation.SourceTurnID) != parts[1] || toolDigest(operation.CallID) != parts[2] {
		return "", errors.New("delegated operation does not match the target")
	}
	source, err := h.store.Get(ctx, operation.SourceSessionID)
	if err != nil {
		return "", err
	}
	if source == nil {
		return "", errSessionNotFound
	}
	target, err := h.store.Get(ctx, targetID)
	if err != nil {
		return "", err
	}
	if target == nil {
		return "", errSessionNotFound
	}
	caller := sessionToolCaller{SessionID: source.ID, WorkspaceID: operation.WorkspaceID, Stored: source, Origin: operation.Origin}
	validate := func() error {
		if err := h.authorizeToolTarget(ctx, caller, source.ID, false); err != nil {
			return err
		}
		if err := h.authorizeToolTarget(ctx, caller, targetID, false); err != nil {
			return err
		}
		if source.ApprovalPolicy != operation.ApprovalPolicy || source.PromptConfig != operation.PromptConfig || target.ApprovalPolicy != operation.ApprovalPolicy || target.PromptConfig != operation.PromptConfig || target.Capabilities.ChannelToolsVersion != 0 {
			return errors.New("delegated target policy changed")
		}
		text := operation.Input.Text
		if operation.Name == "create_session" {
			text = operation.Input.InitialMessage
		}
		_, found, err := sessionlog.FindExistingInput(ctx, h.items, targetID, wire.Command{ID: input.CommandID, Message: &wire.MessageCommandPayload{Text: text, RequestContext: operation.Origin}}, root)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("delegated command is not durably admitted")
		}
		unavailable, err := h.ensureSessionActivatable(ctx, targetID)
		if err != nil {
			return err
		}
		if unavailable != "" {
			return errors.New("delegated target is unavailable")
		}
		return nil
	}
	if operation.Origin != nil {
		if h.channels == nil {
			return "", errors.New("channel authorization is unavailable")
		}
		err = h.channels.WithSessionToolAccess(ctx, source.ID, operation.Origin, operation.WorkspaceID, true, func(_ bool) error { return validate() })
	} else {
		err = validate()
	}
	if err != nil {
		return "", err
	}
	return `{"authorized":true}`, nil
}
