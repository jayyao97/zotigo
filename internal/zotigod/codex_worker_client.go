package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/gorilla/websocket"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/codexapp"
)

type codexWorkerConfig struct {
	workerClientConfig
	SocketPath       string
	WorkingDirectory string
	Model            string
	ReasoningEffort  string
	ThreadID         string
	SessionStoreRoot string
}

type codexWorkerChannels struct {
	commands     <-chan commandResponse
	boundResults <-chan workerConversationBoundResult
	errors       <-chan error
}

func runCodexWorkerClient(ctx context.Context, cfg codexWorkerConfig) (returnErr error) {
	if cfg.SocketPath == "" || cfg.WorkingDirectory == "" {
		return fmt.Errorf("codex worker requires socket and working directory")
	}
	store, err := zotigosession.NewFileStore(cfg.SessionStoreRoot)
	if err != nil {
		return fmt.Errorf("open session store: %w", err)
	}
	unlock, err := acquireWorkerSessionLock(ctx, store, cfg.SessionID)
	if err != nil {
		_ = store.Close()
		return err
	}
	var workerConn *websocket.Conn
	var generation string
	var writer *workerClientWriter
	var appClient *codexapp.Client
	var runtime *codexWorkerRuntime
	runErr := error(nil)
	httpClient := newWorkerHTTPClient(cfg.AuthToken)
	defer func() {
		if runtime != nil {
			returnErr = errors.Join(returnErr, runtime.Close())
		}
		if runErr == nil {
			runErr = returnErr
		}
		// Preserve structured startup failures before closing the worker socket.
		// Otherwise the daemon can observe the disconnect first and replace the
		// useful error with "worker disconnected before becoming ready".
		if runErr != nil && !isExpectedWorkerClose(runErr) {
			finishCtx, cancel := context.WithTimeout(context.Background(), workerHTTPTimeout)
			_ = reportWorkerFinish(finishCtx, httpClient, strings.TrimRight(cfg.DaemonURL, "/"), cfg.SessionID, generation, runErr)
			cancel()
		}
		if writer != nil {
			writer.Close()
		}
		if appClient != nil {
			_ = appClient.Close()
		}
		if workerConn != nil {
			_ = workerConn.Close()
		}
		if unlockErr := unlock(); unlockErr != nil {
			returnErr = errors.Join(returnErr, unlockErr)
		}
		_ = store.Close()
	}()

	daemonURL := strings.TrimRight(cfg.DaemonURL, "/")
	wsURL, err := workerConnectURL(daemonURL, cfg.SessionID)
	if err != nil {
		return err
	}
	headers := http.Header{}
	if cfg.AuthToken != "" {
		headers.Set("Authorization", "Bearer "+cfg.AuthToken)
	}
	var response *http.Response
	workerConn, response, err = websocket.DefaultDialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		return fmt.Errorf("connect codex worker websocket: %w", workerWebSocketDialError(err, response))
	}
	if response != nil {
		generation = strings.TrimSpace(response.Header.Get(workerGenerationHeader))
	}
	if generation == "" {
		return fmt.Errorf("connect codex worker websocket: missing worker generation")
	}
	writer = newWorkerClientWriter(workerConn, defaultWorkerClientPingInterval, defaultWorkerClientPongWait)
	channels := readCodexWorkerMessages(workerConn)

	appClient, err = codexapp.Dial(ctx, cfg.SocketPath)
	if err != nil {
		return err
	}
	var initialized any
	if err := appClient.Call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "zotigod", "title": "Zotigo", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": true},
	}, &initialized); err != nil {
		return fmt.Errorf("initialize codex worker connection: %w", err)
	}
	if err := appClient.Notify("initialized", map[string]any{}); err != nil {
		return err
	}
	if cfg.ThreadID != "" {
		if err := resumeCodexThread(ctx, appClient, cfg); err != nil {
			return err
		}
	}
	if err := reportWorkerReady(ctx, httpClient, daemonURL, cfg.SessionID, generation); err != nil {
		return fmt.Errorf("report codex worker ready: %w", err)
	}

	runtime = &codexWorkerRuntime{cfg: cfg, store: store, writer: writer, app: appClient, threadID: cfg.ThreadID, messages: make(map[string]string)}
	runtime.toolNames = make(map[string]string)
	for {
		select {
		case err := <-channels.errors:
			runErr = err
			if isExpectedWorkerClose(err) {
				return nil
			}
			return err
		case command, ok := <-channels.commands:
			if !ok {
				runErr = errors.New("codex worker command channel closed")
				return runErr
			}
			if err := runtime.handleCommand(ctx, command, channels.boundResults); err != nil {
				runErr = err
				return err
			}
		case message, ok := <-appClient.Notifications():
			if !ok {
				runErr = errors.New("codex app-server connection closed")
				return runErr
			}
			if len(message.ID) > 0 && message.Method != "" {
				_ = appClient.RespondError(message.ID, -32601, "approval forwarding is not available in this version")
				continue
			}
			if err := runtime.handleNotification(ctx, message); err != nil {
				runErr = err
				return err
			}
		}
	}
}

func resumeCodexThread(ctx context.Context, app codexapp.RPC, cfg codexWorkerConfig) error {
	var resumed any
	if err := app.Call(ctx, "thread/resume", map[string]any{
		"threadId": cfg.ThreadID, "cwd": cfg.WorkingDirectory, "model": cfg.Model, "approvalPolicy": "never",
	}, &resumed); err != nil {
		var rpcErr *codexapp.RPCError
		if errors.As(err, &rpcErr) && rpcErr.Code == -32600 && strings.Contains(strings.ToLower(rpcErr.Message), "active writer") {
			return fmt.Errorf("%w: %s", errRuntimeOccupied, rpcErr.Message)
		}
		return fmt.Errorf("resume codex thread: %w", err)
	}
	return nil
}

type codexWorkerRuntime struct {
	cfg             codexWorkerConfig
	store           zotigosession.Store
	writer          *workerClientWriter
	app             codexapp.RPC
	threadID        string
	activeTurnID    string
	commandSequence uint64
	turnStarted     time.Time
	messages        map[string]string
	messageOrder    []string
	toolNames       map[string]string
	toolOrder       []string
}

type codexThreadItem struct {
	ID               string               `json:"id"`
	Type             string               `json:"type"`
	Text             string               `json:"text,omitempty"`
	Summary          []string             `json:"summary,omitempty"`
	Content          any                  `json:"content,omitempty"`
	Command          string               `json:"command,omitempty"`
	CWD              string               `json:"cwd,omitempty"`
	CommandActions   []codexCommandAction `json:"commandActions,omitempty"`
	AggregatedOutput *string              `json:"aggregatedOutput,omitempty"`
	ExitCode         *int                 `json:"exitCode,omitempty"`
	DurationMS       *int64               `json:"durationMs,omitempty"`
	Changes          any                  `json:"changes,omitempty"`
	Status           string               `json:"status,omitempty"`
	Server           string               `json:"server,omitempty"`
	Namespace        *string              `json:"namespace,omitempty"`
	Tool             string               `json:"tool,omitempty"`
	Arguments        any                  `json:"arguments,omitempty"`
	Result           any                  `json:"result,omitempty"`
	Error            *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
	ContentItems      any                        `json:"contentItems,omitempty"`
	Success           *bool                      `json:"success,omitempty"`
	Prompt            *string                    `json:"prompt,omitempty"`
	Model             *string                    `json:"model,omitempty"`
	ReasoningEffort   *string                    `json:"reasoningEffort,omitempty"`
	ReceiverThreadIDs []string                   `json:"receiverThreadIds,omitempty"`
	AgentsStates      map[string]codexAgentState `json:"agentsStates,omitempty"`
}

type codexAgentState struct {
	Status  string  `json:"status"`
	Message *string `json:"message,omitempty"`
}

type codexCommandAction struct {
	Type    string  `json:"type"`
	Command string  `json:"command,omitempty"`
	Name    string  `json:"name,omitempty"`
	Path    *string `json:"path,omitempty"`
	Query   *string `json:"query,omitempty"`
}

type codexItemNotification struct {
	ThreadID string          `json:"threadId"`
	TurnID   string          `json:"turnId"`
	Item     codexThreadItem `json:"item"`
}

func (r *codexWorkerRuntime) Close() error {
	if r == nil || r.activeTurnID == "" {
		return nil
	}
	turnID := r.activeTurnID
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var response any
	interruptErr := r.app.Call(closeCtx, "turn/interrupt", map[string]any{
		"threadId": r.threadID, "turnId": turnID,
	}, &response)
	for _, itemID := range r.messageOrder {
		if err := r.append(zotigosession.DisplayItem{
			ID: itemID, Type: zotigosession.DisplayItemAssistantMessage, Role: string(protocol.RoleAssistant),
			Content: []zotigosession.DisplayContentPart{{Type: string(protocol.ContentTypeText), Text: r.messages[itemID]}},
		}); err != nil {
			return errors.Join(interruptErr, err)
		}
	}
	if err := r.finishPendingTools("codex worker disconnected"); err != nil {
		return errors.Join(interruptErr, err)
	}
	duration := int64(0)
	if !r.turnStarted.IsZero() {
		duration = time.Since(r.turnStarted).Milliseconds()
	}
	appendErr := r.append(zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnInterrupted, Error: "codex worker disconnected",
		Turn: &zotigosession.DisplayTurn{ID: turnID, Status: "interrupted", DurationMS: duration},
	})
	r.activeTurnID = ""
	return errors.Join(interruptErr, appendErr)
}

func (r *codexWorkerRuntime) handleCommand(ctx context.Context, command commandResponse, boundResults <-chan workerConversationBoundResult) error {
	if command.Sequence > r.commandSequence {
		r.commandSequence = command.Sequence
	}
	switch command.Type {
	case sessionCommandMessage:
		if r.activeTurnID != "" {
			return fmt.Errorf("codex session already has an active turn")
		}
		if command.Message == nil {
			return fmt.Errorf("codex message payload is missing")
		}
		if r.threadID == "" {
			if err := r.startThread(ctx, boundResults); err != nil {
				return err
			}
		}
		stored, err := r.store.Get(ctx, r.cfg.SessionID)
		if err != nil || stored == nil {
			return fmt.Errorf("load codex turn settings: %w", err)
		}
		inputs := codexInputs(command.Message.Text, command.Message.Images)
		var response struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		params := map[string]any{
			"threadId": r.threadID, "clientUserMessageId": command.ID, "input": inputs,
			"cwd": r.cfg.WorkingDirectory, "model": stored.Model, "effort": stored.ReasoningEffort,
		}
		if err := r.app.Call(ctx, "turn/start", params, &response); err != nil {
			return fmt.Errorf("start codex turn: %w", err)
		}
		r.activeTurnID = response.Turn.ID
		r.turnStarted = time.Now()
		return r.append(zotigosession.DisplayItem{
			Type: zotigosession.DisplayItemTurnStarted,
			Turn: &zotigosession.DisplayTurn{ID: r.activeTurnID, Status: "in_progress"},
		})
	case sessionCommandPause:
		if r.activeTurnID == "" {
			return nil
		}
		var response any
		return r.app.Call(ctx, "turn/interrupt", map[string]any{"threadId": r.threadID, "turnId": r.activeTurnID}, &response)
	case sessionCommandSteering:
		if r.activeTurnID == "" || command.Steering == nil {
			return fmt.Errorf("codex steering requires an active turn")
		}
		var response any
		return r.app.Call(ctx, "turn/steer", map[string]any{
			"threadId": r.threadID, "expectedTurnId": r.activeTurnID,
			"clientUserMessageId": command.ID,
			"input":               codexInputs(command.Steering.Text, command.Steering.Images),
		}, &response)
	default:
		return fmt.Errorf("codex runtime does not support command %q", command.Type)
	}
}

func (r *codexWorkerRuntime) startThread(ctx context.Context, boundResults <-chan workerConversationBoundResult) error {
	var response struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := r.app.Call(ctx, "thread/start", map[string]any{
		"cwd": r.cfg.WorkingDirectory, "model": r.cfg.Model,
		"approvalPolicy": "never",
	}, &response); err != nil {
		return fmt.Errorf("start codex thread: %w", err)
	}
	if response.Thread.ID == "" {
		return fmt.Errorf("start codex thread: missing thread id")
	}
	if err := r.writer.SendConversationBound(ctx, response.Thread.ID); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case result, ok := <-boundResults:
			if !ok {
				return errors.New("worker connection closed before binding acknowledgement")
			}
			if result.ConversationID != response.Thread.ID {
				continue
			}
			if result.ErrorCode != "" {
				return fmt.Errorf("bind codex thread: %s: %s", result.ErrorCode, result.Error)
			}
			r.threadID = response.Thread.ID
			return nil
		}
	}
}

func (r *codexWorkerRuntime) handleNotification(ctx context.Context, message codexapp.Message) error {
	switch message.Method {
	case "thread/tokenUsage/updated":
		var updated struct {
			ThreadID   string `json:"threadId"`
			TokenUsage struct {
				Last struct {
					TotalTokens int `json:"totalTokens"`
				} `json:"last"`
				ModelContextWindow int `json:"modelContextWindow"`
			} `json:"tokenUsage"`
		}
		if err := sonic.Unmarshal(message.Params, &updated); err != nil {
			return err
		}
		if updated.ThreadID != r.threadID || updated.TokenUsage.ModelContextWindow <= 0 {
			return nil
		}
		return r.append(zotigosession.DisplayItem{
			Type: zotigosession.DisplayItemContextUsageUpdated,
			ContextUsage: &zotigosession.DisplayContextUsage{
				Tokens: updated.TokenUsage.Last.TotalTokens,
				Window: updated.TokenUsage.ModelContextWindow,
			},
		})
	case "item/agentMessage/delta":
		var delta struct {
			TurnID string `json:"turnId"`
			ItemID string `json:"itemId"`
			Delta  string `json:"delta"`
		}
		if err := sonic.Unmarshal(message.Params, &delta); err != nil {
			return err
		}
		if delta.TurnID != r.activeTurnID || delta.ItemID == "" {
			return nil
		}
		if _, exists := r.messages[delta.ItemID]; !exists {
			r.messageOrder = append(r.messageOrder, delta.ItemID)
		}
		r.messages[delta.ItemID] += delta.Delta
		r.writer.SendDelta(displayDeltaEvent{ItemID: delta.ItemID, Role: string(protocol.RoleAssistant), PartType: string(protocol.ContentTypeText), Delta: delta.Delta})
	case "item/started":
		var started codexItemNotification
		if err := sonic.Unmarshal(message.Params, &started); err != nil {
			return err
		}
		if !r.matchesActiveItem(started.ThreadID, started.TurnID, started.Item.ID) {
			return nil
		}
		name, arguments, ok, err := codexToolCall(started.Item)
		if err != nil || !ok {
			return err
		}
		if r.toolNames == nil {
			r.toolNames = make(map[string]string)
		}
		r.toolNames[started.Item.ID] = name
		r.toolOrder = append(r.toolOrder, started.Item.ID)
		return r.append(zotigosession.DisplayItem{
			ID: started.Item.ID, Type: zotigosession.DisplayItemAssistantMessage, Role: string(protocol.RoleAssistant),
			Content: []zotigosession.DisplayContentPart{{Type: "tool_call", ToolCall: &zotigosession.DisplayToolCall{
				ID: started.Item.ID, Name: name, Arguments: arguments,
			}}},
		})
	case "item/commandExecution/outputDelta":
		var delta struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			ItemID   string `json:"itemId"`
			Delta    string `json:"delta"`
		}
		if err := sonic.Unmarshal(message.Params, &delta); err != nil {
			return err
		}
		if !r.matchesActiveItem(delta.ThreadID, delta.TurnID, delta.ItemID) || r.writer == nil {
			return nil
		}
		r.writer.SendDelta(displayDeltaEvent{
			ItemID: delta.ItemID, Role: string(protocol.RoleAssistant), PartType: "tool_progress", Delta: delta.Delta,
			ToolCallID: delta.ItemID, ToolName: r.toolNames[delta.ItemID],
		})
	case "item/completed":
		var completed codexItemNotification
		if err := sonic.Unmarshal(message.Params, &completed); err != nil {
			return err
		}
		if !r.matchesActiveItem(completed.ThreadID, completed.TurnID, completed.Item.ID) {
			return nil
		}
		switch completed.Item.Type {
		case "agentMessage":
			text := completed.Item.Text
			if text == "" {
				text = r.messages[completed.Item.ID]
			}
			return r.persistAgentMessage(completed.Item.ID, text)
		case "reasoning":
			text := codexReasoningText(completed.Item)
			if text == "" {
				return nil
			}
			return r.append(zotigosession.DisplayItem{
				ID: completed.Item.ID, Type: zotigosession.DisplayItemAssistantMessage, Role: string(protocol.RoleAssistant),
				Content: []zotigosession.DisplayContentPart{{Type: string(protocol.ContentTypeReasoning), Text: text}},
			})
		default:
			result, ok := codexToolResult(completed.Item, r.toolNames[completed.Item.ID])
			if !ok {
				return nil
			}
			r.forgetTool(completed.Item.ID)
			return r.append(zotigosession.DisplayItem{
				Type: zotigosession.DisplayItemAssistantMessage, Role: string(protocol.RoleAssistant),
				Content: []zotigosession.DisplayContentPart{{Type: "tool_result", ToolResult: result}},
			})
		}
	case "turn/completed":
		var completed struct {
			Turn struct {
				ID       string `json:"id"`
				Status   string `json:"status"`
				Duration int64  `json:"durationMs"`
				Error    *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		if err := sonic.Unmarshal(message.Params, &completed); err != nil {
			return err
		}
		if completed.Turn.ID != r.activeTurnID {
			return nil
		}
		for _, itemID := range r.messageOrder {
			if err := r.append(zotigosession.DisplayItem{
				ID: itemID, Type: zotigosession.DisplayItemAssistantMessage, Role: string(protocol.RoleAssistant),
				Content: []zotigosession.DisplayContentPart{{Type: string(protocol.ContentTypeText), Text: r.messages[itemID]}},
			}); err != nil {
				return err
			}
		}
		status := strings.ToLower(completed.Turn.Status)
		itemType := zotigosession.DisplayItemTurnCompleted
		switch status {
		case "failed":
			itemType = zotigosession.DisplayItemTurnFailed
		case "interrupted":
			itemType = zotigosession.DisplayItemTurnInterrupted
		}
		errorText := ""
		if completed.Turn.Error != nil {
			errorText = completed.Turn.Error.Message
		}
		duration := completed.Turn.Duration
		if duration == 0 && !r.turnStarted.IsZero() {
			duration = time.Since(r.turnStarted).Milliseconds()
		}
		if err := r.finishPendingTools("codex turn completed before tool result"); err != nil {
			return err
		}
		if err := r.append(zotigosession.DisplayItem{
			Type: itemType, Error: errorText,
			Turn: &zotigosession.DisplayTurn{ID: r.activeTurnID, Status: status, DurationMS: duration},
		}); err != nil {
			return err
		}
		r.activeTurnID = ""
		commandSequence := r.commandSequence
		r.turnStarted = time.Time{}
		r.messages = make(map[string]string)
		r.messageOrder = nil
		r.toolNames = make(map[string]string)
		r.toolOrder = nil
		if r.writer != nil {
			if err := r.writer.SendIdle(ctx, workerIdle{CommandSequence: commandSequence}); err != nil {
				return err
			}
		}
	}
	return nil
}

func codexReasoningText(item codexThreadItem) string {
	parts := append([]string(nil), item.Summary...)
	if content, ok := item.Content.([]any); ok {
		for _, part := range content {
			if text, ok := part.(string); ok {
				parts = append(parts, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func (r *codexWorkerRuntime) matchesActiveItem(threadID, turnID, itemID string) bool {
	return threadID == r.threadID && turnID == r.activeTurnID && itemID != ""
}

func (r *codexWorkerRuntime) persistAgentMessage(itemID, text string) error {
	delete(r.messages, itemID)
	for index, id := range r.messageOrder {
		if id == itemID {
			r.messageOrder = append(r.messageOrder[:index], r.messageOrder[index+1:]...)
			break
		}
	}
	if text == "" {
		return nil
	}
	return r.append(zotigosession.DisplayItem{
		ID: itemID, Type: zotigosession.DisplayItemAssistantMessage, Role: string(protocol.RoleAssistant),
		Content: []zotigosession.DisplayContentPart{{Type: string(protocol.ContentTypeText), Text: text}},
	})
}

func (r *codexWorkerRuntime) forgetTool(itemID string) {
	delete(r.toolNames, itemID)
	for index, id := range r.toolOrder {
		if id == itemID {
			r.toolOrder = append(r.toolOrder[:index], r.toolOrder[index+1:]...)
			return
		}
	}
}

func (r *codexWorkerRuntime) finishPendingTools(reason string) error {
	for _, itemID := range r.toolOrder {
		name := r.toolNames[itemID]
		if name == "" {
			continue
		}
		if err := r.append(zotigosession.DisplayItem{
			Type: zotigosession.DisplayItemAssistantMessage, Role: string(protocol.RoleAssistant),
			Content: []zotigosession.DisplayContentPart{{Type: "tool_result", ToolResult: &zotigosession.DisplayToolResult{
				ToolCallID: itemID, ToolName: name, ResultType: "text", Text: reason, Reason: reason, IsError: true,
			}}},
		}); err != nil {
			return err
		}
	}
	r.toolNames = make(map[string]string)
	r.toolOrder = nil
	return nil
}

func codexToolCall(item codexThreadItem) (string, string, bool, error) {
	name := ""
	arguments := any(nil)
	switch item.Type {
	case "commandExecution":
		name, arguments = codexCommandTool(item)
	case "fileChange":
		name, arguments = "apply_patch", map[string]any{"changes": item.Changes}
	case "mcpToolCall", "dynamicToolCall":
		name, arguments = item.Tool, item.Arguments
	case "collabAgentToolCall":
		name = codexCollabToolName(item.Tool)
		arguments = map[string]any{
			"description": item.Prompt, "prompt": item.Prompt, "model": item.Model,
			"reasoning_effort": item.ReasoningEffort, "receiver_thread_ids": item.ReceiverThreadIDs,
		}
	default:
		return "", "", false, nil
	}
	encoded, err := sonic.Marshal(arguments)
	return name, string(encoded), true, err
}

func codexCollabToolName(tool string) string {
	switch tool {
	case "spawnAgent":
		return "spawn_agent"
	case "sendInput":
		return "send_message"
	case "resumeAgent":
		return "resume_agent"
	case "closeAgent":
		return "close_agent"
	default:
		return tool
	}
}

func codexCommandTool(item codexThreadItem) (string, any) {
	arguments := map[string]any{"command": item.Command, "cwd": item.CWD}
	if len(item.CommandActions) != 1 {
		return "shell", arguments
	}
	action := item.CommandActions[0]
	switch action.Type {
	case "read":
		arguments["path"] = action.Path
		return "read_file", arguments
	case "listFiles":
		arguments["path"] = action.Path
		return "glob", arguments
	case "search":
		arguments["path"] = action.Path
		arguments["query"] = action.Query
		return "grep", arguments
	default:
		return "shell", arguments
	}
}

func codexToolResult(item codexThreadItem, name string) (*zotigosession.DisplayToolResult, bool) {
	if name == "" {
		name, _, _, _ = codexToolCall(item)
	}
	if name == "" {
		return nil, false
	}
	result := &zotigosession.DisplayToolResult{ToolCallID: item.ID, ToolName: name, ResultType: "text"}
	switch item.Type {
	case "commandExecution":
		if item.AggregatedOutput != nil {
			result.Text = *item.AggregatedOutput
		}
		result.JSON = map[string]any{"exit_code": item.ExitCode, "duration_ms": item.DurationMS, "status": item.Status}
		result.IsError = codexFailedStatus(item.Status) || item.ExitCode != nil && *item.ExitCode != 0
	case "fileChange":
		result.JSON = map[string]any{"changes": item.Changes, "status": item.Status}
		result.Text = item.Status
		result.IsError = strings.EqualFold(item.Status, "failed") || strings.EqualFold(item.Status, "declined")
	case "mcpToolCall":
		result.JSON = item.Result
		result.IsError = codexFailedStatus(item.Status)
		if item.Error != nil {
			result.Text = item.Error.Message
			result.Reason = item.Error.Message
			result.IsError = true
		}
	case "dynamicToolCall":
		result.JSON = item.ContentItems
		result.IsError = codexFailedStatus(item.Status) || item.Success != nil && !*item.Success
	case "collabAgentToolCall":
		result.JSON = map[string]any{"receiver_thread_ids": item.ReceiverThreadIDs, "agents_states": item.AgentsStates, "status": item.Status}
		result.IsError = codexFailedStatus(item.Status)
	default:
		return nil, false
	}
	return result, true
}

func codexFailedStatus(status string) bool {
	return strings.EqualFold(status, "failed") || strings.EqualFold(status, "declined")
}

func (r *codexWorkerRuntime) append(item zotigosession.DisplayItem) error {
	if _, err := r.store.AppendDisplayItem(context.Background(), r.cfg.SessionID, item); err != nil {
		return err
	}
	if r.writer != nil {
		r.writer.SendDisplayWake()
	}
	return nil
}

func codexInputs(text string, images []commandImageData) []map[string]any {
	inputs := make([]map[string]any, 0, 1+len(images))
	if strings.TrimSpace(text) != "" {
		inputs = append(inputs, map[string]any{"type": "text", "text": text, "textElements": []any{}})
	}
	for _, image := range images {
		inputs = append(inputs, map[string]any{
			"type": "image",
			"url":  "data:" + image.MimeType + ";base64," + image.DataBase64,
		})
	}
	return inputs
}

func readCodexWorkerMessages(conn *websocket.Conn) codexWorkerChannels {
	commands := make(chan commandResponse, workerCommandBufferSize)
	boundResults := make(chan workerConversationBoundResult, 1)
	errorsCh := make(chan error, 1)
	go func() {
		defer close(commands)
		defer close(boundResults)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				errorsCh <- err
				return
			}
			var message workerMessage
			if err := sonic.Unmarshal(data, &message); err != nil {
				errorsCh <- err
				return
			}
			switch message.Type {
			case workerMessageCommand:
				if message.Command != nil {
					commands <- *message.Command
				}
			case workerMessageConversationBoundResult:
				if message.ConversationBoundResult != nil {
					boundResults <- *message.ConversationBoundResult
				}
			}
		}
	}()
	return codexWorkerChannels{commands: commands, boundResults: boundResults, errors: errorsCh}
}
