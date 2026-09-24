package zotigod

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Live admission already holds the source Channel configuration fence. Only
// durable command replay performs this second RPC, avoiding recursive RLocks
// while the daemon waits for a live worker admission.
func authorizeRuntimeToolReplay(ctx context.Context, client *workerRuntimeToolClient, commandID string) error {
	if !strings.HasPrefix(commandID, "session_tool:") {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	arguments, _ := json.Marshal(map[string]string{"command_id": commandID})
	_, err := client.request(ctx, workerRuntimeToolRequest{Namespace: "host", Name: "authorize_input", Arguments: arguments})
	return err
}

func (h *handler) authorizeDelegatedReplay(ctx context.Context, targetID string, raw json.RawMessage) (string, error) {
	var input struct {
		CommandID string `json:"command_id"`
	}
	if err := decodeSessionToolArguments(raw, &input); err != nil {
		return "", err
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
		_, found, err := findExistingSessionInput(ctx, h.items, targetID, commandResponse{ID: input.CommandID, Message: &messageCommandPayload{Text: text, RequestContext: operation.Origin}}, root)
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
