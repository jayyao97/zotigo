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
	SocketPath        string
	ProjectID         string
	WorkingDirectory  string
	Model             string
	ReasoningEffort   string
	ThreadID          string
	WorkspaceRevision uint64
	SessionStoreRoot  string
}

type codexWorkerChannels struct {
	commands     <-chan commandResponse
	boundResults <-chan workerConversationBoundResult
	errors       <-chan error
}

func runCodexWorkerClient(ctx context.Context, cfg codexWorkerConfig) (returnErr error) {
	if cfg.SocketPath == "" || cfg.ProjectID == "" || cfg.WorkingDirectory == "" {
		return fmt.Errorf("codex worker requires socket, project, and working directory")
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
		if runErr != nil && !isExpectedWorkerClose(runErr) {
			finishCtx, cancel := context.WithTimeout(context.Background(), workerHTTPTimeout)
			_ = reportWorkerFinish(finishCtx, httpClient, strings.TrimRight(cfg.DaemonURL, "/"), cfg.SessionID, generation, runErr)
			cancel()
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
	headers.Set(workerWorkspaceBindingRevisionHeader, fmt.Sprintf("%d", cfg.WorkspaceRevision))
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
	for {
		select {
		case err := <-channels.errors:
			runErr = err
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
	var resumed struct {
		Thread struct {
			ProjectID string `json:"projectId"`
		} `json:"thread"`
	}
	if err := app.Call(ctx, "thread/resume", map[string]any{
		"threadId": cfg.ThreadID, "cwd": cfg.WorkingDirectory, "model": cfg.Model, "approvalPolicy": "never",
	}, &resumed); err != nil {
		return fmt.Errorf("resume codex thread: %w", err)
	}
	if resumed.Thread.ProjectID == cfg.ProjectID {
		return nil
	}
	var updated struct {
		Thread struct {
			ProjectID string `json:"projectId"`
		} `json:"thread"`
	}
	if err := app.Call(ctx, "thread/metadata/update", map[string]any{
		"threadId": cfg.ThreadID, "projectId": cfg.ProjectID,
	}, &updated); err != nil {
		return fmt.Errorf("assign resumed codex thread to project: %w", err)
	}
	if updated.Thread.ProjectID != cfg.ProjectID {
		return fmt.Errorf("assign resumed codex thread to project: response did not confirm assignment")
	}
	return nil
}

type codexWorkerRuntime struct {
	cfg          codexWorkerConfig
	store        zotigosession.Store
	writer       *workerClientWriter
	app          codexapp.RPC
	threadID     string
	activeTurnID string
	turnStarted  time.Time
	messages     map[string]string
	messageOrder []string
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
			ID        string `json:"id"`
			ProjectID string `json:"projectId"`
		} `json:"thread"`
	}
	if err := r.app.Call(ctx, "thread/start", map[string]any{
		"cwd": r.cfg.WorkingDirectory, "model": r.cfg.Model,
		"projectId": r.cfg.ProjectID, "approvalPolicy": "never",
	}, &response); err != nil {
		return fmt.Errorf("start codex thread: %w", err)
	}
	if response.Thread.ID == "" {
		return fmt.Errorf("start codex thread: missing thread id")
	}
	if response.Thread.ProjectID != r.cfg.ProjectID {
		return fmt.Errorf("start codex thread: response did not confirm project assignment")
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
		if err := r.append(zotigosession.DisplayItem{
			Type: itemType, Error: errorText,
			Turn: &zotigosession.DisplayTurn{ID: r.activeTurnID, Status: status, DurationMS: duration},
		}); err != nil {
			return err
		}
		r.activeTurnID = ""
		r.turnStarted = time.Time{}
		r.messages = make(map[string]string)
		r.messageOrder = nil
	}
	return nil
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
