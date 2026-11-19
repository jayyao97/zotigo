package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/gorilla/websocket"
	_ "github.com/jayyao97/zotigo/core/providers/anthropic"
	_ "github.com/jayyao97/zotigo/core/providers/deepseek"
	_ "github.com/jayyao97/zotigo/core/providers/gemini"
	_ "github.com/jayyao97/zotigo/core/providers/openai"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

func Run(ctx context.Context, cfg workerbase.Config) (returnErr error) {
	bootStarted := time.Now()
	if strings.TrimSpace(cfg.SessionID) == "" {
		return fmt.Errorf("session_id is required")
	}
	daemonURL := strings.TrimRight(cfg.DaemonURL, "/")
	if daemonURL == "" {
		return fmt.Errorf("daemon_url is required")
	}

	stepStarted := time.Now()
	store, err := zotigosession.NewFileStore("")
	if err != nil {
		return fmt.Errorf("open session store: %w", err)
	}
	workerbase.LogBootStep(cfg.SessionID, "session_store", stepStarted, bootStarted)
	stepStarted = time.Now()
	unlock, err := workerbase.AcquireSessionLock(ctx, store, cfg.SessionID)
	if err != nil {
		_ = store.Close()
		return err
	}
	workerbase.LogBootStep(cfg.SessionID, "session_lock", stepStarted, bootStarted)
	var runtime *Runtime
	var conn *websocket.Conn
	var generation string
	stopKeepalive := func() {}
	var clientWriter *workerbase.Writer
	var runErr error
	httpClient := workerbase.NewHTTPClient(cfg.AuthToken)
	defer func() {
		if runErr == nil {
			runErr = returnErr
		}
		stopKeepalive()
		if runtime != nil {
			runtime.Close()
		}
		if unlockErr := unlock(); unlockErr != nil {
			wrapped := fmt.Errorf("unlock session %s: %w", cfg.SessionID, unlockErr)
			returnErr = errors.Join(returnErr, wrapped)
			if runErr == nil {
				runErr = wrapped
			}
		}
		if runErr != nil && !workerbase.IsExpectedClose(runErr) {
			finishCtx, cancel := context.WithTimeout(context.Background(), workerbase.HTTPTimeout)
			_ = workerbase.ReportFinish(finishCtx, httpClient, daemonURL, cfg.SessionID, generation, runErr)
			cancel()
		}
		if conn != nil {
			_ = conn.Close()
		}
		_ = store.Close()
	}()

	wsURL, err := workerbase.ConnectURL(daemonURL, cfg.SessionID, cfg.Activation)
	if err != nil {
		return err
	}
	stepStarted = time.Now()
	var resp *http.Response
	requestHeader := http.Header{}
	if cfg.AuthToken != "" {
		requestHeader.Set("Authorization", "Bearer "+cfg.AuthToken)
	}
	conn, resp, err = websocket.DefaultDialer.DialContext(ctx, wsURL, requestHeader)
	if err != nil {
		return fmt.Errorf("connect worker websocket: %w", workerbase.WebSocketDialError(err, resp))
	}
	if resp != nil {
		generation = strings.TrimSpace(resp.Header.Get(wire.GenerationHeader))
	}
	if generation == "" {
		return fmt.Errorf("connect worker websocket: missing worker generation")
	}
	workerbase.LogBootStep(cfg.SessionID, "websocket_connect", stepStarted, bootStarted)
	clientWriter = workerbase.NewWriter(conn, workerbase.DefaultPingInterval, workerbase.DefaultPongWait)
	stopKeepalive = clientWriter.Close
	displayBarrier := workerbase.NewDisplayBarrierClient(clientWriter)
	commandCh, inputCh, approvalCh, runtimeToolResults, readErrCh := ReadMessages(conn, displayBarrier.Acknowledge)
	runtimeToolClient := &workerbase.RuntimeToolClient{Writer: clientWriter, Results: runtimeToolResults}

	stepStarted = time.Now()
	workspaceRoot, err := url.PathUnescape(resp.Header.Get(wire.WorkspaceRootHeader))
	if err != nil {
		return fmt.Errorf("decode worker workspace root: %w", err)
	}
	runtime, err = newWorkerRuntime(ctx, workerRuntimeConfig{
		SessionID:         cfg.SessionID,
		WorkspaceID:       resp.Header.Get(wire.WorkspaceHeader),
		WorkspaceRoot:     workspaceRoot,
		Store:             store,
		SendDelta:         clientWriter.SendDelta,
		NotifyDisplay:     clientWriter.SendDisplayWake,
		NotifyDisplayItem: clientWriter.SendDisplayItem,
		SyncDisplay:       clientWriter.SendDisplayWakeReliable,
		DisplayBarrier:    displayBarrier.Wait,
		NotifyApproval: func(ctx context.Context, approval wire.ApprovalRequest) {
			_ = clientWriter.SendApprovalRequest(ctx, approval)
		},
		NotifyApprovalResolved: func(ctx context.Context, approval wire.ApprovalRequest) {
			_ = clientWriter.SendApprovalResult(ctx, wire.ApprovalResult{Approval: &approval})
		},
		NotifyIdle:    clientWriter.SendIdle,
		NotifyWorking: clientWriter.SendWorking,
		RuntimeTools:  runtimeToolClient,
	})
	if err != nil {
		return err
	}
	workerbase.LogBootStep(cfg.SessionID, "runtime", stepStarted, bootStarted)

	stepStarted = time.Now()
	cursor, err := workerbase.LoadCommandCursor(ctx, store, cfg.SessionID)
	if err != nil {
		return err
	}
	cursor, err = ReplayCommands(ctx, httpClient, daemonURL, cfg.SessionID, runtime, cursor)
	if err != nil {
		return err
	}
	workerbase.LogBootStep(cfg.SessionID, "command_replay", stepStarted, bootStarted)
	select {
	case err := <-readErrCh:
		runErr = err
		return err
	default:
	}

	stepStarted = time.Now()
	if err := workerbase.ReportReady(ctx, httpClient, daemonURL, cfg.SessionID, generation); err != nil {
		return fmt.Errorf("report worker ready: %w", err)
	}
	workerbase.LogBootStep(cfg.SessionID, "ready", stepStarted, bootStarted)
	_ = runtime.NotifyIdleIfIdle(context.Background(), cursor.Sequence)

	for {
		select {
		case err := <-runtime.fatalCh:
			runErr = err
			return err
		case err := <-readErrCh:
			runErr = err
			if workerbase.IsExpectedClose(err) {
				return nil
			}
			return runErr
		case command, ok := <-commandCh:
			if !ok {
				runErr = <-readErrCh
				return runErr
			}
			if command.Type == wire.CommandSteering && command.Sequence == 0 {
				if err := runtime.HandleCommand(ctx, command); err != nil {
					runErr = err
					return err
				}
				continue
			}
			if command.Sequence <= cursor.Sequence {
				continue
			}
			cursor, err = ReplayCommands(ctx, httpClient, daemonURL, cfg.SessionID, runtime, cursor)
			if err != nil {
				runErr = err
				return err
			}
			if err := runtime.NotifyIdleIfIdle(context.Background(), cursor.Sequence); err != nil {
				runErr = err
				return err
			}
		case request, ok := <-inputCh:
			if !ok {
				runErr = <-readErrCh
				return runErr
			}
			result := runtime.AcceptInput(ctx, request)
			if !clientWriter.SendInputResult(ctx, result) {
				runErr = fmt.Errorf("send input result: worker websocket closed")
				return runErr
			}
			if result.Command != nil {
				recovered, recoverErr := workerbase.RecoverCommandCursor(ctx, store, cfg.SessionID)
				if recoverErr != nil {
					runErr = recoverErr
					return recoverErr
				}
				if recovered.Sequence > cursor.Sequence {
					cursor = recovered
					if err := workerbase.SaveCommandCursor(cfg.SessionID, cursor); err != nil {
						runErr = err
						return err
					}
				}
			}
		case decision, ok := <-approvalCh:
			if !ok {
				runErr = <-readErrCh
				return runErr
			}
			result, resolution := runtime.resolveApproval(ctx, decision)
			if !clientWriter.SendApprovalResult(ctx, result) {
				runErr = fmt.Errorf("send approval result: worker websocket closed")
				return runErr
			}
			if resolution != nil {
				if err := runtime.releaseApproval(ctx, *resolution); err != nil {
					runErr = fmt.Errorf("release approval: %w", err)
					return runErr
				}
				if decision.StopTurn {
					// Interrupt in the same worker step that released the
					// denials, before any further command is processed, so
					// the agent's next model call is cancelled.
					if err := runtime.pauseTurn(ctx, &wire.PauseCommandPayload{TurnID: resolution.approval.TurnID, Reason: wire.UserPauseReason}); err != nil {
						runErr = fmt.Errorf("stop turn after denial: %w", err)
						return runErr
					}
				}
			}
		}
	}
}

func ReadMessages(conn *websocket.Conn, acknowledgeDisplayBarrier func(string)) (<-chan wire.Command, <-chan wire.InputRequest, <-chan wire.ApprovalDecision, <-chan wire.RuntimeToolResult, <-chan error) {
	commandCh := make(chan wire.Command, workerbase.CommandBufferSize)
	inputCh := make(chan wire.InputRequest, workerbase.CommandBufferSize)
	approvalCh := make(chan wire.ApprovalDecision, workerbase.CommandBufferSize)
	runtimeToolResults := make(chan wire.RuntimeToolResult, workerbase.CommandBufferSize)
	errCh := make(chan error, 1)
	go func() {
		defer close(commandCh)
		defer close(inputCh)
		defer close(approvalCh)
		defer close(runtimeToolResults)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			var msg wire.Message
			if err := sonic.Unmarshal(data, &msg); err != nil {
				errCh <- fmt.Errorf("decode worker message: %w", err)
				return
			}
			switch msg.Type {
			case wire.MessageCommand:
				if msg.Command == nil {
					continue
				}
				select {
				case commandCh <- *msg.Command:
				default:
					errCh <- fmt.Errorf("worker command buffer full")
					_ = conn.Close()
					return
				}
			case wire.MessageApprovalDecision:
				if msg.ApprovalDecision == nil {
					continue
				}
				select {
				case approvalCh <- *msg.ApprovalDecision:
				default:
					errCh <- fmt.Errorf("worker approval buffer full")
					_ = conn.Close()
					return
				}
			case wire.MessageInputRequest:
				if msg.InputRequest == nil {
					continue
				}
				select {
				case inputCh <- *msg.InputRequest:
				default:
					errCh <- fmt.Errorf("worker input buffer full")
					_ = conn.Close()
					return
				}
			case wire.MessageDisplayBarrierOK:
				if msg.DisplayBarrier == nil || msg.DisplayBarrier.ID == "" {
					continue
				}
				if acknowledgeDisplayBarrier != nil {
					acknowledgeDisplayBarrier(msg.DisplayBarrier.ID)
				}
			case wire.MessageRuntimeToolResult:
				if msg.RuntimeToolResult != nil {
					runtimeToolResults <- *msg.RuntimeToolResult
				}
			}
		}
	}()
	return commandCh, inputCh, approvalCh, runtimeToolResults, errCh
}
