package zotigod

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/gorilla/websocket"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/codexapp"
	"github.com/jayyao97/zotigo/internal/hooks"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

type codexWorkerConfig struct {
	workerClientConfig
	SocketPath            string
	WorkingDirectory      string
	WorkspaceID           string
	Model                 string
	ReasoningEffort       string
	ApprovalPolicy        string
	ThreadID              string
	SessionStoreRoot      string
	HookDispatcher        hookEventDispatcher
	DeveloperInstructions string
	AutoReview            bool
	AutoReviewPolicy      string
	ChannelToolsVersion   uint64
	SessionToolsVersion   uint64
}

type codexWorkerChannels struct {
	commands     <-chan wire.Command
	inputs       <-chan wire.InputRequest
	approvals    <-chan wire.ApprovalDecision
	interactions <-chan wire.InteractionResponse
	boundResults <-chan wire.ConversationBoundResult
	interrupts   <-chan wire.InterruptTurn
	runtimeTools <-chan wire.RuntimeToolResult
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
	storedSession, err := store.Get(ctx, cfg.SessionID)
	if err != nil {
		_ = unlock()
		_ = store.Close()
		return fmt.Errorf("load Codex session prompt: %w", err)
	}
	if storedSession == nil {
		_ = unlock()
		_ = store.Close()
		return errSessionNotFound
	}
	cfg.DeveloperInstructions = codexDeveloperInstructions(storedSession.PromptConfig)
	cfg.AutoReview = storedSession.PromptConfig.ReviewAllTools
	cfg.AutoReviewPolicy = codexAutoReviewPolicy(storedSession.PromptConfig)
	cfg.ChannelToolsVersion = storedSession.Capabilities.ChannelToolsVersion
	cfg.SessionToolsVersion = storedSession.Capabilities.SessionToolsVersion
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
			returnErr = errors.Join(returnErr, runtime.cleanupSkillFiles())
		}
		if appClient != nil {
			_ = appClient.Close()
		}
		// Finish reporting takes the daemon's session operation lock. Release
		// storage first so workspace archival can wait for shutdown under that lock.
		if unlockErr := unlock(); unlockErr != nil {
			returnErr = errors.Join(returnErr, unlockErr)
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
		if workerConn != nil {
			_ = workerConn.Close()
		}
		_ = store.Close()
	}()

	daemonURL := strings.TrimRight(cfg.DaemonURL, "/")
	wsURL, err := workerConnectURL(daemonURL, cfg.SessionID, cfg.Activation)
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
		generation = strings.TrimSpace(response.Header.Get(wire.GenerationHeader))
	}
	if generation == "" {
		return fmt.Errorf("connect codex worker websocket: missing worker generation")
	}
	cfg.WorkspaceID = response.Header.Get(wire.WorkspaceHeader)
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
	hookDispatcher := cfg.HookDispatcher
	var ownedHookDispatcher *hooks.Dispatcher
	if hookDispatcher == nil {
		hookLogger := log.New(log.Writer(), "[zotigo-hooks] ", log.LstdFlags)
		ownedHookDispatcher, err = hooks.LoadDefault(hookLogger)
		if err != nil {
			hookLogger.Printf("hook_config outcome=failed error=%q", err)
		} else {
			hookDispatcher = ownedHookDispatcher
		}
	}
	if ownedHookDispatcher != nil {
		defer closeHookDispatcher(ownedHookDispatcher)
	}

	runtime = &codexWorkerRuntime{cfg: cfg, store: store, writer: writer, app: appClient, threadID: cfg.ThreadID, messages: make(map[string]string), interactions: make(map[string]codexPendingInteraction), approvals: make(map[string]codexPendingApproval), hooks: hookDispatcher, runtimeToolResults: channels.runtimeTools}
	runtime.toolNames = make(map[string]string)
	runtime.toolArguments = make(map[string]string)
	runtime.toolNativeNames = make(map[string]string)
	go serveCodexWorkerInterrupts(ctx, appClient, channels.interrupts)
	if cfg.ThreadID != "" {
		items, _, err := store.ListDisplayItems(ctx, cfg.SessionID)
		if err != nil {
			return fmt.Errorf("restore Codex display turn: %w", err)
		}
		runtime.activeTurnID, runtime.turnStarted = lastOpenTurn(items)
	}
	cursor, err := recoverWorkerCommandCursor(ctx, store, cfg.SessionID)
	if err != nil {
		return err
	}
	runtime.commandSequence = cursor.Sequence
	pendingCommands, err := loadCodexPendingCommands(ctx, httpClient, daemonURL, cfg.SessionID, cursor.Sequence)
	if err != nil {
		return err
	}
	for {
		if len(pendingCommands) > 0 && runtime.canApplyCommand(pendingCommands[0]) {
			command := pendingCommands[0]
			if err := runtime.handleCommand(ctx, command, channels.boundResults); err != nil {
				runErr = err
				return err
			}
			pendingCommands = pendingCommands[1:]
			continue
		}
		runtime.messagePending = len(pendingCommands) > 0 && pendingCommands[0].Type == wire.CommandMessage
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
			pendingCommands = enqueueCodexCommand(pendingCommands, runtime.commandSequence, command)
		case request, ok := <-channels.inputs:
			if !ok {
				runErr = errors.New("codex worker input channel closed")
				return runErr
			}
			result := runtime.AcceptInput(ctx, request, channels.boundResults, appClient.Notifications(), channels)
			if !writer.SendInputResult(ctx, result) {
				runErr = fmt.Errorf("send input result: worker websocket closed")
				return runErr
			}
		case response, ok := <-channels.interactions:
			if !ok {
				runErr = errors.New("codex worker interaction channel closed")
				return runErr
			}
			result, fatalErr := runtime.resolveInteraction(response)
			if !writer.SendInteractionResult(ctx, result) {
				runErr = errors.New("send interaction result: worker websocket closed")
				return runErr
			}
			if fatalErr != nil {
				runErr = fatalErr
				return fatalErr
			}
		case decision, ok := <-channels.approvals:
			if !ok {
				runErr = errors.New("codex worker approval channel closed")
				return runErr
			}
			result, fatalErr := runtime.resolveCodexApproval(decision)
			if !writer.SendApprovalResult(ctx, result) {
				runErr = errors.New("send approval result: worker websocket closed")
				return runErr
			}
			if fatalErr != nil {
				runErr = fatalErr
				return fatalErr
			}
		case message, ok := <-appClient.Notifications():
			if !ok {
				runErr = errors.New("codex app-server connection closed")
				return runErr
			}
			if len(message.ID) > 0 && message.Method != "" {
				if err := runtime.handleServerRequest(ctx, message); err != nil {
					var fatal *codexRequestFatalError
					if errors.As(err, &fatal) {
						respondCodexRequestError(appClient, message, -32603, err.Error())
						runErr = fatal
						return fatal
					}
					respondCodexRequestError(appClient, message, -32601, err.Error())
				}
				continue
			}
			if err := runtime.handleNotification(ctx, message); err != nil {
				runErr = err
				return err
			}
		}
	}
}

func loadCodexPendingCommands(ctx context.Context, client *http.Client, daemonURL, sessionID string, appliedSequence uint64) ([]wire.Command, error) {
	pending := make([]wire.Command, 0)
	cursor := workerCommandCursor{}
	for {
		previousOffset := cursor.Offset
		commands, err := fetchWorkerCommands(ctx, client, daemonURL, sessionID, cursor)
		if err != nil {
			return nil, fmt.Errorf("load Codex worker commands: %w", err)
		}
		for _, command := range commands.Commands {
			pending = enqueueCodexCommand(pending, appliedSequence, command)
		}
		cursor.Offset = commands.NextOffset
		if commands.NextOffset == previousOffset || len(commands.Commands) < wire.MaxCommandsLimit {
			return pending, nil
		}
	}
}

func enqueueCodexCommand(pending []wire.Command, appliedSequence uint64, command wire.Command) []wire.Command {
	if command.Sequence > 0 && command.Sequence <= appliedSequence {
		return pending
	}
	for _, queued := range pending {
		if (command.Sequence > 0 && queued.Sequence == command.Sequence) || (command.ID != "" && queued.ID == command.ID) {
			return pending
		}
	}
	return append(pending, command)
}

func resumeCodexThread(ctx context.Context, app codexapp.RPC, cfg codexWorkerConfig) error {
	params := codexThreadParams(cfg, false)
	params["threadId"] = cfg.ThreadID
	var resumed any
	if err := app.Call(ctx, "thread/resume", params, &resumed); err != nil {
		var rpcErr *codexapp.RPCError
		if errors.As(err, &rpcErr) && rpcErr.Code == -32600 && strings.Contains(strings.ToLower(rpcErr.Message), "active writer") {
			return fmt.Errorf("%w: %s", errRuntimeOccupied, rpcErr.Message)
		}
		return fmt.Errorf("resume codex thread: %w", err)
	}
	return nil
}

func readCodexWorkerMessages(conn *websocket.Conn) codexWorkerChannels {
	commands := make(chan wire.Command, workerCommandBufferSize)
	inputs := make(chan wire.InputRequest, workerCommandBufferSize)
	approvals := make(chan wire.ApprovalDecision, workerCommandBufferSize)
	interactions := make(chan wire.InteractionResponse, workerCommandBufferSize)
	boundResults := make(chan wire.ConversationBoundResult, 1)
	interrupts := make(chan wire.InterruptTurn, 1)
	runtimeTools := make(chan wire.RuntimeToolResult, workerCommandBufferSize)
	errorsCh := make(chan error, 1)
	go func() {
		defer close(commands)
		defer close(inputs)
		defer close(approvals)
		defer close(interactions)
		defer close(boundResults)
		defer close(interrupts)
		defer close(runtimeTools)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				errorsCh <- err
				return
			}
			var message wire.Message
			if err := sonic.Unmarshal(data, &message); err != nil {
				errorsCh <- err
				return
			}
			switch message.Type {
			case wire.MessageCommand:
				if message.Command != nil {
					commands <- *message.Command
				}
			case wire.MessageInputRequest:
				if message.InputRequest != nil {
					inputs <- *message.InputRequest
				}
			case wire.MessageApprovalDecision:
				if message.ApprovalDecision != nil {
					approvals <- *message.ApprovalDecision
				}
			case wire.MessageInteractionResponse:
				if message.InteractionResponse != nil {
					interactions <- *message.InteractionResponse
				}
			case wire.MessageConversationBoundResult:
				if message.ConversationBoundResult != nil {
					boundResults <- *message.ConversationBoundResult
				}
			case wire.MessageInterruptTurn:
				if message.InterruptTurn != nil {
					select {
					case interrupts <- *message.InterruptTurn:
					default:
						errorsCh <- fmt.Errorf("codex worker interrupt buffer full")
						return
					}
				}
			case wire.MessageRuntimeToolResult:
				if message.RuntimeToolResult != nil {
					runtimeTools <- *message.RuntimeToolResult
				}
			}
		}
	}()
	return codexWorkerChannels{commands: commands, inputs: inputs, approvals: approvals, interactions: interactions, boundResults: boundResults, interrupts: interrupts, runtimeTools: runtimeTools, errors: errorsCh}
}

func serveCodexWorkerInterrupts(ctx context.Context, app codexapp.RPC, interrupts <-chan wire.InterruptTurn) {
	for {
		select {
		case <-ctx.Done():
			return
		case interrupt, ok := <-interrupts:
			if !ok {
				return
			}
			interruptCtx, cancel := context.WithTimeout(ctx, workerHTTPTimeout)
			var response any
			_ = app.Call(interruptCtx, "turn/interrupt", map[string]any{
				"threadId": interrupt.ThreadID,
				"turnId":   interrupt.TurnID,
			}, &response)
			cancel()
		}
	}
}
