package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
	"github.com/jayyao97/zotigo/internal/channels"
	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

const channelTaskTimeout = 2 * time.Hour

func (h *handler) EnsureChannelSessionPrompt(ctx context.Context, sessionID string, prompt channels.SessionPromptConfig) (channels.SessionPromptConfig, error) {
	if h.store == nil {
		return channels.SessionPromptConfig{}, errors.New("session store is unavailable")
	}
	unlock := h.sessionOps.lock(sessionID)
	defer unlock()
	stored, err := h.store.Get(ctx, sessionID)
	if err != nil {
		return channels.SessionPromptConfig{}, fmt.Errorf("load channel session prompt: %w", err)
	}
	if stored == nil {
		return channels.SessionPromptConfig{}, errSessionNotFound
	}
	if stored.ApprovalPolicy != agent.ApprovalPolicyAuto {
		return channels.SessionPromptConfig{}, errors.New("channel sessions must use auto approval policy")
	}
	desired := zotigosession.PromptConfig{AgentInstructions: prompt.AgentInstructions, ApprovalInstructions: prompt.ApprovalInstructions, ReviewAllTools: prompt.ReviewAllTools}
	if stored.PromptConfig.Revision != 0 {
		current := stored.PromptConfig
		return channels.SessionPromptConfig{AgentInstructions: current.AgentInstructions, ApprovalInstructions: current.ApprovalInstructions, ReviewAllTools: current.ReviewAllTools}, nil
	}
	if live, ok := h.registry.Get(sessionID); ok && live.State != SessionStateCreated && live.State != SessionStateOffline {
		return channels.SessionPromptConfig{}, errors.New("legacy channel prompt snapshot requires an idle session")
	}
	desired.Revision = 1
	stored.PromptConfig = desired
	stored.UpdatedAt = time.Now().UTC()
	if err := h.store.Put(ctx, stored); err != nil {
		return channels.SessionPromptConfig{}, err
	}
	return prompt, nil
}

func (h *handler) ProvisionChannelSession(ctx context.Context, workspaceID, title string, prompt channels.SessionPromptConfig, runtime channels.SessionRuntimeConfig, bind func(sessionID string, resolved channels.SessionRuntimeConfig) error) error {
	if h.store == nil {
		return errors.New("session store is unavailable")
	}
	if h.catalog == nil {
		return errors.New("workspace catalog is unavailable")
	}
	workspaceID = strings.TrimSpace(workspaceID)
	workspace, releaseWorkspace, err := h.lockWorkspaceForUse(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("lock channel workspace: %w", err)
	}
	defer releaseWorkspace()
	if workspace.Status != zotigoworkspace.WorkspaceStatusReady {
		return fmt.Errorf("channel workspace is not ready: %w", zotigoworkspace.ErrConflict)
	}
	agentKind := zotigoruntime.AgentKind(strings.TrimSpace(runtime.Agent))
	if agentKind == "" {
		agentKind = zotigoruntime.AgentZotigo
	}
	resolved := channels.SessionRuntimeConfig{Agent: string(agentKind)}
	var profileName string
	switch agentKind {
	case zotigoruntime.AgentZotigo:
		appConfig, configErr := config.NewManager().LoadForDir(workspace.RootPath)
		if configErr != nil {
			return fmt.Errorf("load channel workspace profiles: %w", configErr)
		}
		profileName, _, err = appConfig.ResolveProfile(strings.TrimSpace(runtime.ProfileName))
		if err != nil {
			return fmt.Errorf("resolve channel workspace profile: %w", err)
		}
		resolved.ProfileName = profileName
	case zotigoruntime.AgentCodex:
		resolved.Model = strings.TrimSpace(runtime.Model)
		resolved.ReasoningEffort = strings.TrimSpace(runtime.ReasoningEffort)
		if err = h.validateCodexSettings(ctx, resolved.Model, resolved.ReasoningEffort); err != nil {
			return err
		}
	default:
		return errors.New("unsupported channel session agent")
	}
	session := newSession(workspace.RootPath, profileName)
	session.Agent = string(agentKind)
	session.Model = resolved.Model
	session.ReasoningEffort = resolved.ReasoningEffort
	session.ChannelToolsVersion = channels.RuntimeToolsVersion
	if err := h.persistSession(ctx, session); err != nil {
		return fmt.Errorf("persist channel session: %w", err)
	}
	cleanup := func(cause error) error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var cleanupErr error
		if h.catalog != nil {
			if err := h.catalog.RemoveSessionOrganization(cleanupCtx, session.ID); err != nil && !errors.Is(err, zotigoworkspace.ErrNotFound) {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove channel session organization: %w", err))
			}
		}
		if err := h.store.Delete(cleanupCtx, session.ID); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete channel session: %w", err))
		}
		return errors.Join(cause, cleanupErr)
	}
	storedSession, err := h.store.Get(ctx, session.ID)
	if err != nil {
		return cleanup(fmt.Errorf("load new channel session: %w", err))
	}
	if storedSession == nil {
		return cleanup(errors.New("load new channel session: session is unavailable"))
	}
	storedSession.PromptConfig = zotigosession.PromptConfig{AgentInstructions: prompt.AgentInstructions, ApprovalInstructions: prompt.ApprovalInstructions, ReviewAllTools: prompt.ReviewAllTools, Revision: 1}
	if err := h.store.Put(ctx, storedSession); err != nil {
		return cleanup(fmt.Errorf("persist channel prompt snapshot: %w", err))
	}
	if _, err := h.catalog.AssignSession(ctx, session.ID, workspaceID); err != nil {
		return cleanup(fmt.Errorf("assign channel session workspace: %w", err))
	}
	if strings.TrimSpace(title) != "" {
		if _, err := h.catalog.SetSessionTitle(ctx, session.ID, strings.TrimSpace(title)); err != nil {
			return cleanup(fmt.Errorf("set channel session title: %w", err))
		}
	}
	if err := ctx.Err(); err != nil {
		return cleanup(err)
	}
	if bind == nil {
		return cleanup(errors.New("channel session bind callback is unavailable"))
	}
	if err := bind(session.ID, resolved); err != nil {
		return cleanup(fmt.Errorf("bind channel session: %w", err))
	}
	h.registry.Add(session)
	return nil
}

func (h *handler) StopChannelTask(parent context.Context, task channels.Task, progress func(channels.Progress), admissionComplete func()) (channels.TaskResult, error) {
	var admissionOnce sync.Once
	completeAdmission := func() {
		if admissionComplete != nil {
			admissionOnce.Do(admissionComplete)
		}
	}
	defer completeAdmission()
	ctx, cancel := context.WithTimeout(parent, h.inputStopTimeout)
	defer cancel()
	session, ok := h.registry.Get(task.SessionID)
	if !ok || session.State != SessionStateRunning {
		completeAdmission()
		if ok && session.State == SessionStatePausing {
			return channels.TaskResult{Text: "A stop is already in progress."}, nil
		}
		return channels.TaskResult{Text: "There is no active task to stop."}, nil
	}
	if _, err := h.pauseActiveSession(ctx, task.SessionID, session, ""); err != nil {
		completeAdmission()
		switch {
		case errors.Is(err, errNoActiveTurn), errors.Is(err, errSessionBusy), errors.Is(err, errInvalidSessionTransition):
			return channels.TaskResult{Text: "There is no active task to stop."}, nil
		case errors.Is(err, errWorkerInputStopping):
			return channels.TaskResult{Text: "A stop is already in progress."}, nil
		default:
			return channels.TaskResult{}, fmt.Errorf("stop bound session: %w", err)
		}
	}
	// Keep the channel configuration fence until the target turn is captured
	// and its durable pause command has been admitted. Progress delivery takes
	// the same read lock, so signal before emitting it.
	completeAdmission()
	if progress != nil {
		progress(channels.Progress{State: "running", Events: []channels.PublicExecutionEvent{{ID: "channel-stop:" + task.MessageID, Type: channels.ExecutionRunStarted, Timestamp: time.Now().UTC()}}})
	}
	return channels.TaskResult{Text: "Stop requested for the active task."}, nil
}

func (h *handler) DispatchChannelTask(parent context.Context, task channels.Task, progress func(channels.Progress), admissionComplete func()) (channels.TaskResult, error) {
	var admissionOnce sync.Once
	completeAdmission := func() {
		if admissionComplete != nil {
			admissionOnce.Do(admissionComplete)
		}
	}
	defer completeAdmission()
	ctx, cancel := context.WithTimeout(parent, channelTaskTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return channels.TaskResult{}, err
	}

	desired := zotigosession.PromptConfig{AgentInstructions: task.AgentInstructions, ApprovalInstructions: task.ApprovalInstructions, ReviewAllTools: task.ReviewAllTools}
	if strings.TrimSpace(desired.ApprovalInstructions) != "" && !desired.ReviewAllTools {
		return channels.TaskResult{}, errors.New("channel approval instructions require review_all_tools")
	}
	var stored *zotigosession.Session
	err := func() error {
		unlock := h.sessionOps.lock(task.SessionID)
		defer unlock()
		var loadErr error
		stored, loadErr = h.store.Get(ctx, task.SessionID)
		if loadErr != nil {
			return fmt.Errorf("load bound session: %w", loadErr)
		}
		if stored == nil {
			return errSessionNotFound
		}
		if stored.ApprovalPolicy != agent.ApprovalPolicyAuto {
			return errors.New("channel sessions must use auto approval policy")
		}
		current := stored.PromptConfig
		if current.AgentInstructions != desired.AgentInstructions || current.ApprovalInstructions != desired.ApprovalInstructions || current.ReviewAllTools != desired.ReviewAllTools {
			return errors.New("channel task prompt does not match the bound session snapshot")
		}
		desired.Revision = current.Revision
		return nil
	}()
	if err != nil {
		return channels.TaskResult{}, err
	}

	_, exists, err := h.items.LoadItems(ctx, task.SessionID)
	if err != nil {
		return channels.TaskResult{}, fmt.Errorf("load session items: %w", err)
	}
	if !exists {
		return channels.TaskResult{}, errSessionNotFound
	}
	fallbackRuntime := channelSessionRuntimeAttribution(stored)
	events, unsubscribe := h.events.Subscribe(task.SessionID)
	defer unsubscribe()

	if _, err = h.ensureSessionRunning(ctx, task.SessionID); err != nil {
		return channels.TaskResult{}, err
	}
	if !h.ensureWorkerOnline(ctx, task.SessionID) {
		return channels.TaskResult{}, errWorkerOffline
	}
	images, err := channelMessageImages(task.Images)
	if err != nil {
		return channels.TaskResult{}, err
	}
	commandID := "channel:" + task.ConnectionID + ":" + task.MessageID
	requestContext := &protocol.RequestContext{
		Source: task.Origin.Provider, ConnectionID: task.ConnectionID, ConnectionName: task.Origin.ConnectionName,
		Channel:        &protocol.RequestIdentity{ID: task.Origin.ChannelID, DisplayName: task.Origin.ChannelName},
		ConversationID: task.Origin.ConversationID, ConversationName: task.Origin.ConversationName,
		ConversationType: task.Origin.ConversationType, ExternalConversation: task.Origin.ExternalConversation,
		ExternalConversationName: task.Origin.ExternalConversationName,
		ExternalRootID:           task.Origin.ExternalRootID,
		ExternalThreadID:         task.Origin.ExternalThreadID,
		ExternalMessageID:        task.Origin.ExternalMessageID,
		ExternalParentMessageID:  task.Origin.ExternalParentMessageID,
		Actor:                    protocol.RequestActor{ID: task.Origin.Sender.ID, DisplayName: task.Origin.Sender.DisplayName, Role: task.Origin.ActorRole},
	}
	accepted, err := h.acceptSessionInputCommand(ctx, task.SessionID, commandID, task.Text, images, nil, "", false, true, &sessionInputConstraints{approvalPolicy: agent.ApprovalPolicyAuto, promptRevision: desired.Revision, requestContext: requestContext})
	if err != nil {
		if errors.Is(err, errActiveTurn) || errors.Is(err, errSessionBusy) {
			return channels.TaskResult{}, errors.New("the bound session already has an active turn")
		}
		return channels.TaskResult{}, err
	}
	completeAdmission()
	// Durable display items for this turn are appended after the accepted
	// channel command. This excludes unrelated turns that completed while the
	// channel request was waiting for worker admission.
	after := accepted.Sequence
	projected := after

	ticker := time.NewTicker(displayEventCatchUpInterval)
	defer ticker.Stop()
	turnID := ""
	approvalReported := false
	for {
		result, done, waitErr := h.channelTaskResult(ctx, task.SessionID, after, &projected, &turnID, &approvalReported, fallbackRuntime, progress)
		if done || waitErr != nil {
			return result, waitErr
		}
		select {
		case <-ctx.Done():
			return channels.TaskResult{}, ctx.Err()
		case <-events:
		case <-ticker.C:
		}
	}
}

func channelMessageImages(inputs []channels.InboundImage) ([]messageImage, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	if len(inputs) > maxMessageImages {
		return nil, fmt.Errorf("channel message images must contain at most %d items", maxMessageImages)
	}
	images := make([]messageImage, 0, len(inputs))
	totalBytes := 0
	for index, input := range inputs {
		if len(input.Data) == 0 {
			return nil, fmt.Errorf("channel message image %d is empty", index)
		}
		if len(input.Data) > maxMessageImageBytes {
			return nil, fmt.Errorf("channel message image %d exceeds %d bytes", index, maxMessageImageBytes)
		}
		totalBytes += len(input.Data)
		if totalBytes > maxMessageTotalImageBytes {
			return nil, fmt.Errorf("channel message images exceed %d total bytes", maxMessageTotalImageBytes)
		}
		mimeType := strings.ToLower(strings.TrimSpace(strings.Split(http.DetectContentType(input.Data), ";")[0]))
		if !isAllowedMessageImageMimeType(mimeType) {
			return nil, fmt.Errorf("channel message image %d has unsupported MIME type %q", index, mimeType)
		}
		width, height, err := validateMessageImageData(mimeType, input.Data)
		if err != nil {
			return nil, fmt.Errorf("validate channel message image %d: %w", index, err)
		}
		images = append(images, messageImage{MimeType: mimeType, Data: input.Data, SizeBytes: len(input.Data), Width: width, Height: height})
	}
	return images, nil
}

func (h *handler) channelTaskResult(ctx context.Context, sessionID string, after uint64, projected *uint64, turnID *string, approvalReported *bool, fallbackRuntime channels.RuntimeAttribution, progress func(channels.Progress)) (channels.TaskResult, bool, error) {
	items, exists, err := h.items.LoadItems(ctx, sessionID)
	if err != nil {
		return channels.TaskResult{}, false, err
	}
	if !exists {
		return channels.TaskResult{}, false, errSessionNotFound
	}
	answer := ""
	runtime := fallbackRuntime
	var publicEvents []channels.PublicExecutionEvent
	var projectedThrough uint64
	for _, item := range items {
		if item.Sequence <= after {
			continue
		}
		if *turnID == "" && item.Type == zotigosession.DisplayItemTurnStarted && item.Turn != nil {
			*turnID = item.Turn.ID
		}
		if *turnID == "" {
			continue
		}
		if item.Type == zotigosession.DisplayItemTurnStarted && item.Turn != nil && item.Turn.ID == *turnID && item.Turn.Runtime != nil {
			runtime = channels.RuntimeAttribution{Agent: item.Turn.Runtime.Agent, ProfileName: item.Turn.Runtime.ProfileName, Model: item.Turn.Runtime.Model, ReasoningEffort: item.Turn.Runtime.ReasoningEffort}
		}
		if item.Type == zotigosession.DisplayItemApprovalRequest && item.Approval != nil && item.Approval.TurnID == *turnID && !*approvalReported {
			*approvalReported = true
		}
		if item.Sequence > *projected {
			publicEvents = append(publicEvents, projectChannelItem(item, *turnID)...)
			if item.Sequence > projectedThrough {
				projectedThrough = item.Sequence
			}
		}
		if item.Type == zotigosession.DisplayItemAssistantMessage && (item.Turn == nil || item.Turn.ID == "" || item.Turn.ID == *turnID) {
			var text strings.Builder
			for _, part := range item.Content {
				if part.Type == "text" {
					text.WriteString(part.Text)
				}
			}
			if text.Len() > 0 {
				answer = text.String()
			}
		}
		if item.Turn == nil || item.Turn.ID != *turnID {
			continue
		}
		switch item.Type {
		case zotigosession.DisplayItemTurnCompleted:
			if len(publicEvents) > 0 {
				progress(channels.Progress{State: "running", Sequence: projectedThrough, Events: publicEvents})
				*projected = projectedThrough
			}
			if item.Turn.LastAgentMessage != "" {
				answer = item.Turn.LastAgentMessage
			}
			if answer == "" {
				answer = "Completed."
			}
			return channels.TaskResult{Text: answer, Runtime: runtime, DurationMS: item.Turn.DurationMS, Usage: item.Turn.Usage}, true, nil
		case zotigosession.DisplayItemTurnFailed:
			if len(publicEvents) > 0 {
				progress(channels.Progress{State: "running", Sequence: projectedThrough, Events: publicEvents})
				*projected = projectedThrough
			}
			if item.Error != "" {
				return channels.TaskResult{}, true, errors.New(item.Error)
			}
			return channels.TaskResult{}, true, errors.New("agent turn failed")
		case zotigosession.DisplayItemTurnInterrupted:
			if len(publicEvents) > 0 {
				progress(channels.Progress{State: "running", Sequence: projectedThrough, Events: publicEvents})
				*projected = projectedThrough
			}
			return channels.TaskResult{}, true, errors.New("agent turn was interrupted")
		}
	}
	if len(publicEvents) > 0 {
		state, text := "running", "Zotigo is working…"
		for _, event := range publicEvents {
			if event.Type == channels.ExecutionApproval {
				state, text = "approval", "Waiting for approval in Zotigo."
			}
		}
		progress(channels.Progress{State: state, Text: text, Sequence: projectedThrough, Events: publicEvents})
		*projected = projectedThrough
	}
	return channels.TaskResult{}, false, nil
}

func channelSessionRuntimeAttribution(session *zotigosession.Session) channels.RuntimeAttribution {
	if session == nil {
		return channels.RuntimeAttribution{}
	}
	// Older turns do not have durable per-turn runtime metadata. Report only the
	// Session fields we can prove; resolving the current profile here could
	// misattribute a historical turn after that profile changes.
	return channels.RuntimeAttribution{Agent: session.Agent, ProfileName: session.ProfileName, Model: session.Model, ReasoningEffort: session.ReasoningEffort}
}

func projectChannelItem(item zotigosession.DisplayItem, turnID string) []channels.PublicExecutionEvent {
	at := item.CreatedAt
	event := func(suffix, eventType string, tool *channels.PublicToolActivity) channels.PublicExecutionEvent {
		return channels.PublicExecutionEvent{ID: item.ID + suffix, Type: eventType, Timestamp: at, Tool: tool}
	}
	switch item.Type {
	case zotigosession.DisplayItemTurnStarted:
		if item.Turn != nil && item.Turn.ID == turnID {
			return []channels.PublicExecutionEvent{event("", channels.ExecutionRunStarted, nil)}
		}
	case zotigosession.DisplayItemToolExecutionStarted:
		if item.ToolExecution != nil && item.ToolExecution.TurnID == turnID && item.ToolExecution.ToolCallID != "" {
			activity := &channels.PublicToolActivity{CallID: item.ToolExecution.ToolCallID, Kind: publicToolKind(item.ToolExecution.ToolName), DisplayName: publicToolName(item.ToolExecution.ToolName), Status: "running"}
			return []channels.PublicExecutionEvent{event("", channels.ExecutionToolStarted, activity)}
		}
	case zotigosession.DisplayItemApprovalRequest:
		if item.Approval != nil && item.Approval.TurnID == turnID {
			projected := event("", channels.ExecutionApproval, nil)
			projected.CorrelationID = item.Approval.ID
			return []channels.PublicExecutionEvent{projected}
		}
	case zotigosession.DisplayItemApprovalDecision:
		if item.Approval != nil && item.Approval.TurnID == turnID {
			projected := event("", channels.ExecutionApprovalDone, nil)
			projected.CorrelationID = item.Approval.ID
			return []channels.PublicExecutionEvent{projected}
		}
	case zotigosession.DisplayItemAssistantMessage:
		if item.Turn != nil && item.Turn.ID != "" && item.Turn.ID != turnID {
			return nil
		}
		var events []channels.PublicExecutionEvent
		for index, part := range item.Content {
			if part.ToolResult == nil || part.ToolResult.ToolCallID == "" {
				continue
			}
			status := "completed"
			if part.ToolResult.IsError {
				status = "failed"
			}
			activity := &channels.PublicToolActivity{CallID: part.ToolResult.ToolCallID, Kind: publicToolKind(part.ToolResult.ToolName), DisplayName: publicToolName(part.ToolResult.ToolName), Status: status}
			events = append(events, event(fmt.Sprintf(":%d", index), channels.ExecutionToolFinished, activity))
		}
		return events
	}
	return nil
}

func publicToolName(name string) string {
	switch name {
	case "shell", "exec_command":
		return "Run command"
	case "read_file":
		return "Read file"
	case "write_file", "edit":
		return "Edit file"
	case "grep", "glob":
		return "Search workspace"
	case "web_search", "web_fetch":
		return "Search the web"
	case "read_messages", "channel_read_messages":
		return "Read group messages"
	case "spawn_agent", "send_message", "wait_agent":
		return "Delegate task"
	default:
		return "Use tool"
	}
}

func publicToolKind(name string) string {
	switch name {
	case "shell", "exec_command", "read_file", "write_file", "edit", "grep", "glob", "web_search", "web_fetch", "spawn_agent", "send_message", "wait_agent", "read_messages", "channel_read_messages":
		return name
	default:
		return "tool"
	}
}
