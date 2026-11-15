package zotigod

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/skills"
	"github.com/jayyao97/zotigo/internal/codexapp"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

func (r *codexWorkerRuntime) AcceptInput(ctx context.Context, request wire.InputRequest, boundResults <-chan wire.ConversationBoundResult, notifications <-chan codexapp.Message, workerChannels ...codexWorkerChannels) wire.InputResult {
	result := wire.InputResult{RequestID: request.RequestID}
	command, err := r.acceptInput(ctx, request, boundResults, notifications, workerChannels...)
	if err != nil {
		result.ErrorCode, result.Error = sessionlog.InputErrorDetails(err)
		return result
	}
	result.Command = &command
	return result
}

func (r *codexWorkerRuntime) acceptInput(ctx context.Context, request wire.InputRequest, boundResults <-chan wire.ConversationBoundResult, notifications <-chan codexapp.Message, workerChannels ...codexWorkerChannels) (wire.Command, error) {
	source := sessionlog.StoredItemSource{Store: r.store}
	if existing, found, err := sessionlog.FindExistingInput(ctx, source, r.cfg.SessionID, request.Command, r.cfg.SessionStoreRoot); err != nil {
		return wire.Command{}, err
	} else if found {
		if request.SteeringOnly && existing.Type != wire.CommandSteering {
			return wire.Command{}, sessionlog.ErrCommandIDConflict
		}
		if request.StartOnly && existing.Type != wire.CommandMessage {
			return wire.Command{}, sessionlog.ErrCommandIDConflict
		}
		if request.ExpectedTurnID != "" && (existing.Steering == nil || existing.Steering.TurnID != request.ExpectedTurnID) {
			return wire.Command{}, sessionlog.ErrCommandIDConflict
		}
		return existing, nil
	}

	if request.RequiredApprovalPolicy != "" || request.RequirePromptRevision {
		stored, err := r.store.Get(ctx, r.cfg.SessionID)
		if err != nil {
			return wire.Command{}, err
		}
		if stored == nil {
			return wire.Command{}, workerbase.ErrSessionNotFound
		}
		policy, err := workerbase.NormalizeSessionApprovalPolicy(stored.ApprovalPolicy, true)
		if err != nil {
			return wire.Command{}, err
		}
		if (request.RequiredApprovalPolicy != "" && policy != request.RequiredApprovalPolicy) ||
			(request.RequirePromptRevision && stored.PromptConfig.Revision != request.RequiredPromptRevision) {
			return wire.Command{}, sessionlog.ErrInputPolicyMismatch
		}
	}
	if err := sessionlog.ValidateCompletionSteering(ctx, source, r.cfg.SessionID, request.Command); err != nil {
		return wire.Command{}, err
	}
	activeTurnID := r.activeTurnID
	if request.StartOnly && activeTurnID != "" {
		return wire.Command{}, sessionlog.ErrActiveTurn
	}
	if request.ExpectedTurnID != "" && request.ExpectedTurnID != activeTurnID {
		return wire.Command{}, sessionlog.ErrTurnMismatch
	}
	if request.SteeringOnly {
		if activeTurnID == "" {
			return wire.Command{}, sessionlog.ErrNoActiveTurn
		}
		command := sessionlog.SteeringCommandForRequest(request, activeTurnID)
		if err := r.callTurnSteer(ctx, command, activeTurnID); err != nil {
			return wire.Command{}, r.classifyTurnSteerError(ctx, activeTurnID, err)
		}
		stored, err := sessionlog.AppendAcceptedInput(ctx, source, r.cfg.SessionID, command)
		if err != nil {
			return wire.Command{}, err
		}
		if err := r.persistSteering(stored, activeTurnID); err != nil {
			return wire.Command{}, err
		}
		r.acknowledgeCommand(stored.Sequence)
		return stored, nil
	}
	message := sessionlog.MessageCommandForRequest(request)
	turnID, runtime, err := r.callTurnStart(ctx, message, boundResults)
	if err != nil {
		return wire.Command{}, err
	}
	if activeTurnID != "" && turnID == activeTurnID {
		steering := sessionlog.SteeringCommandForRequest(request, activeTurnID)
		stored, err := sessionlog.AppendAcceptedInput(ctx, source, r.cfg.SessionID, steering)
		if err != nil {
			return wire.Command{}, err
		}
		if err := r.persistSteering(stored, activeTurnID); err != nil {
			return wire.Command{}, err
		}
		r.acknowledgeCommand(stored.Sequence)
		return stored, nil
	}
	if activeTurnID != "" {
		var channels codexWorkerChannels
		if len(workerChannels) > 0 {
			channels = workerChannels[0]
		}
		if err := r.consumeThroughTurnCompletion(ctx, activeTurnID, channels, notifications); err != nil {
			return wire.Command{}, err
		}
	}
	stored, err := sessionlog.AppendAcceptedInput(ctx, source, r.cfg.SessionID, message)
	if err != nil {
		return wire.Command{}, err
	}
	if err := r.recordTurnStarted(ctx, stored, turnID, runtime); err != nil {
		return wire.Command{}, err
	}
	return stored, nil
}

func (r *codexWorkerRuntime) classifyTurnSteerError(ctx context.Context, expectedTurnID string, steerErr error) error {
	var rpcErr *codexapp.RPCError
	if !errors.As(steerErr, &rpcErr) || rpcErr.Code != -32600 {
		return steerErr
	}
	turns, err := listCodexThreadTurns(ctx, r.app, r.threadID, "notLoaded")
	if err != nil {
		return steerErr
	}
	activeTurnID := ""
	for index := len(turns) - 1; index >= 0; index-- {
		if turns[index].Status == "inProgress" {
			activeTurnID = turns[index].ID
			break
		}
	}
	if activeTurnID == "" {
		return sessionlog.ErrNoActiveTurn
	}
	if activeTurnID != expectedTurnID {
		return sessionlog.ErrTurnMismatch
	}
	return steerErr
}

func (r *codexWorkerRuntime) callTurnStart(ctx context.Context, command wire.Command, boundResults <-chan wire.ConversationBoundResult) (string, zotigosession.DisplayRuntime, error) {
	if command.Message == nil {
		return "", zotigosession.DisplayRuntime{}, fmt.Errorf("codex message payload is missing")
	}
	if r.threadID == "" {
		if err := r.startThread(ctx, boundResults); err != nil {
			return "", zotigosession.DisplayRuntime{}, err
		}
	}
	stored, err := r.store.Get(ctx, r.cfg.SessionID)
	if err != nil || stored == nil {
		return "", zotigosession.DisplayRuntime{}, fmt.Errorf("load codex turn settings: %w", err)
	}
	inputs, err := r.codexInputs(command.Message.Text, command.Message.Images, command.Message.Skills)
	if err != nil {
		return "", zotigosession.DisplayRuntime{}, err
	}
	var response struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	params := map[string]any{
		"threadId": r.threadID, "clientUserMessageId": command.ID, "input": inputs,
		"cwd": r.cfg.WorkingDirectory, "model": stored.Model, "effort": stored.ReasoningEffort,
		"approvalPolicy": codexApprovalPolicy(string(stored.ApprovalPolicy)),
	}
	requestContext, err := codexAdditionalContext(command.Message.RequestContext)
	if err != nil {
		return "", zotigosession.DisplayRuntime{}, err
	}
	if requestContext != nil {
		params["additionalContext"] = requestContext
	}
	err = r.app.Call(ctx, "turn/start", params, &response)
	if err != nil {
		return "", zotigosession.DisplayRuntime{}, fmt.Errorf("start codex turn: %w", err)
	}
	return response.Turn.ID, zotigosession.DisplayRuntime{Agent: "codex", Model: stored.Model, ReasoningEffort: stored.ReasoningEffort}, nil
}

func (r *codexWorkerRuntime) callTurnSteer(ctx context.Context, command wire.Command, turnID string) error {
	if command.Steering == nil {
		return fmt.Errorf("codex steering payload is missing")
	}
	var response struct {
		TurnID string `json:"turnId"`
	}
	if err := r.app.Call(ctx, "turn/steer", map[string]any{
		"threadId": r.threadID, "expectedTurnId": turnID,
		"clientUserMessageId": command.ID,
		"input":               codexInputs(command.Steering.Text, command.Steering.Images),
	}, &response); err != nil {
		return err
	}
	if response.TurnID != "" && response.TurnID != turnID {
		return sessionlog.ErrTurnMismatch
	}
	return nil
}

func (r *codexWorkerRuntime) recordTurnStarted(ctx context.Context, command wire.Command, turnID string, runtime zotigosession.DisplayRuntime) error {
	r.activeTurnID = turnID
	r.turnStarted = time.Now()
	r.turnModel = runtime.Model
	r.turnUsage = protocol.Usage{}
	if command.Message != nil && command.Message.RequestContext != nil {
		requestContext := *command.Message.RequestContext
		r.activeRequestContext = &requestContext
	} else {
		r.activeRequestContext = nil
	}
	if err := r.append(zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: turnID, Status: "in_progress", Runtime: &runtime},
	}); err != nil {
		return err
	}
	workerbase.DispatchTurnStartHook(r.hooks, r.cfg.SessionID, "codex", r.cfg.WorkingDirectory, turnID, runtime.Model)
	workerbase.DispatchUserPromptSubmitHook(r.hooks, r.cfg.SessionID, "codex", r.cfg.WorkingDirectory, turnID, command.Message.Text)
	r.acknowledgeCommand(command.Sequence)
	r.messagePending = false
	if r.writer != nil {
		_ = r.writer.SendWorking(ctx)
	}
	return nil
}

func (r *codexWorkerRuntime) persistSteering(command wire.Command, turnID string) error {
	// turn/steer acknowledges admission, not an item/generation boundary.
	// Anchor only items already in flight; later item/started notifications
	// belong after this input. Keep the first boundary for repeated steering.
	for id, active := range r.displayItems {
		if active.beforeInputID != "" {
			continue
		}
		active.beforeInputID = command.ID
		r.displayItems[id] = active
		if r.writer != nil && (active.partType == "text" || active.partType == "reasoning") {
			r.writer.SendDelta(wire.DisplayDelta{
				ItemID: id, Role: "assistant", PartType: active.partType, BeforeInputID: command.ID,
			})
		}
	}
	images := sessionlog.MessageImagesFromCommand(command.Steering.Images)
	item := sessionlog.MessageItem(zotigosession.DisplayItemSteeringMessage, command.Steering.Text, images)
	item.ID = command.ID
	item.CreatedAt = command.CreatedAt
	item.Turn = &zotigosession.DisplayTurn{ID: turnID}
	item.Command = &zotigosession.DisplayCommand{
		Type: wire.CommandSteering, Text: command.Steering.Text, Images: sessionlog.CommandImages(images), Skills: append([]string(nil), command.Steering.Skills...), TurnID: turnID,
	}
	workerbase.DispatchUserPromptSubmitHook(r.hooks, r.cfg.SessionID, "codex", r.cfg.WorkingDirectory, turnID, command.Steering.Text)
	return r.append(item)
}

func (r *codexWorkerRuntime) consumeThroughTurnCompletion(ctx context.Context, turnID string, channels codexWorkerChannels, notifications <-chan codexapp.Message) error {
	for r.activeTurnID == turnID {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case response, ok := <-channels.interactions:
			if !ok {
				return errors.New("codex worker interaction channel closed")
			}
			result, fatalErr := r.resolveInteraction(response)
			if !r.writer.SendInteractionResult(ctx, result) {
				return errors.New("send interaction result: worker websocket closed")
			}
			if fatalErr != nil {
				return fatalErr
			}
		case decision, ok := <-channels.approvals:
			if !ok {
				return errors.New("codex worker approval channel closed")
			}
			result, fatalErr := r.resolveCodexApproval(decision)
			if !r.writer.SendApprovalResult(ctx, result) {
				return errors.New("send approval result: worker websocket closed")
			}
			if fatalErr != nil {
				return fatalErr
			}
		case message, ok := <-notifications:
			if !ok {
				return errors.New("codex app-server connection closed")
			}
			if len(message.ID) > 0 && message.Method != "" {
				if err := r.handleServerRequest(ctx, message); err != nil {
					var fatal *codexRequestFatalError
					if errors.As(err, &fatal) {
						respondCodexRequestError(r.app, message, -32603, err.Error())
						return fatal
					}
					respondCodexRequestError(r.app, message, -32601, err.Error())
				}
				continue
			}
			if err := r.handleNotification(ctx, message); err != nil {
				return err
			}
		}
	}
	return nil
}

func codexInputs(text string, images []wire.CommandImageData) []map[string]any {
	inputs, _ := buildCodexInputs(text, images, nil)
	return inputs
}

func (r *codexWorkerRuntime) codexInputs(text string, images []wire.CommandImageData, names []string) ([]map[string]any, error) {
	if len(names) == 0 {
		return codexInputs(text, images), nil
	}
	if r.skills == nil {
		r.skills = skills.NewSkillManager(r.cfg.WorkingDirectory)
	}
	if err := r.skills.Reload(); err != nil {
		return nil, fmt.Errorf("reload Codex skills: %w", err)
	}
	selected, err := r.skills.ResolveExplicit(names)
	if err != nil {
		return nil, fmt.Errorf("resolve Codex skills: %w", err)
	}
	resolved := make([]*skills.SkillDefinition, 0, len(selected))
	for _, skill := range selected {
		path, pathErr := r.codexSkillPath(skill)
		if pathErr != nil {
			return nil, pathErr
		}
		copy := *skill
		copy.Path = path
		resolved = append(resolved, &copy)
	}
	return buildCodexInputs(text, images, resolved)
}

func buildCodexInputs(text string, images []wire.CommandImageData, selected []*skills.SkillDefinition) ([]map[string]any, error) {
	mentions := make(map[string]bool)
	for _, name := range skills.DetectSkillMentions(text) {
		mentions[name] = true
	}
	var prefix []string
	for _, skill := range selected {
		if !mentions[skill.Name] {
			prefix = append(prefix, "$"+skill.Name)
		}
	}
	if len(prefix) > 0 {
		text = strings.Join(prefix, " ") + " " + text
	}
	inputs := make([]map[string]any, 0, 1+len(images)+len(selected))
	if strings.TrimSpace(text) != "" {
		inputs = append(inputs, map[string]any{"type": "text", "text": text, "textElements": []any{}})
	}
	for _, skill := range selected {
		if !filepath.IsAbs(skill.Path) {
			return nil, fmt.Errorf("codex skill %q does not have an absolute SKILL.md path", skill.Name)
		}
		inputs = append(inputs, map[string]any{"type": "skill", "name": skill.Name, "path": skill.Path})
	}
	for _, image := range images {
		inputs = append(inputs, map[string]any{
			"type": "image",
			"url":  "data:" + image.MimeType + ";base64," + image.DataBase64,
		})
	}
	return inputs, nil
}

func (r *codexWorkerRuntime) codexSkillPath(skill *skills.SkillDefinition) (string, error) {
	if filepath.IsAbs(skill.Path) {
		return skill.Path, nil
	}
	if skill.Source != skills.SkillSourceBuiltin {
		return "", fmt.Errorf("codex skill %q does not have an absolute SKILL.md path", skill.Name)
	}
	if path := r.skillPaths[skill.Name]; path != "" {
		return path, nil
	}
	if r.skillTempDir == "" {
		dir, err := os.MkdirTemp("", "zotigo-codex-skills-")
		if err != nil {
			return "", fmt.Errorf("create Codex builtin skill directory: %w", err)
		}
		r.skillTempDir = dir
		r.skillPaths = make(map[string]string)
	}
	dir, err := os.MkdirTemp(r.skillTempDir, "skill-")
	if err != nil {
		return "", fmt.Errorf("create Codex builtin skill: %w", err)
	}
	path := filepath.Join(dir, skills.SkillFileName)
	if err := os.WriteFile(path, []byte(skill.Content), 0600); err != nil {
		return "", fmt.Errorf("write Codex builtin skill: %w", err)
	}
	r.skillPaths[skill.Name] = path
	return path, nil
}

func (r *codexWorkerRuntime) cleanupSkillFiles() error {
	if r == nil || r.skillTempDir == "" {
		return nil
	}
	err := os.RemoveAll(r.skillTempDir)
	r.skillTempDir = ""
	r.skillPaths = nil
	return err
}
