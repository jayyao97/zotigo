package zotigod

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/gorilla/websocket"
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/debug"
	_ "github.com/jayyao97/zotigo/core/providers/anthropic"
	_ "github.com/jayyao97/zotigo/core/providers/deepseek"
	_ "github.com/jayyao97/zotigo/core/providers/gemini"
	_ "github.com/jayyao97/zotigo/core/providers/openai"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigotransport "github.com/jayyao97/zotigo/core/transport"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

const workerHTTPTimeout = 10 * time.Second

const workerDialErrorBodyLimit = 4 * 1024

var errActiveTurn = errors.New("active_turn: a turn is already active")

const (
	defaultWorkerClientPingInterval = 15 * time.Second
	defaultWorkerClientPongWait     = 45 * time.Second
	workerCommandBufferSize         = 32
	workerDeltaBufferSize           = 256
)

type workerClientConfig struct {
	DaemonURL  string
	SessionID  string
	Activation uint64
	AuthToken  string
}

func runWorkerClient(ctx context.Context, cfg workerClientConfig) (returnErr error) {
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
	logWorkerBootStep(cfg.SessionID, "session_store", stepStarted, bootStarted)
	stepStarted = time.Now()
	unlock, err := acquireWorkerSessionLock(ctx, store, cfg.SessionID)
	if err != nil {
		_ = store.Close()
		return err
	}
	logWorkerBootStep(cfg.SessionID, "session_lock", stepStarted, bootStarted)
	var runtime *workerRuntime
	var conn *websocket.Conn
	var generation string
	stopKeepalive := func() {}
	var clientWriter *workerClientWriter
	var runErr error
	httpClient := newWorkerHTTPClient(cfg.AuthToken)
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
		if runErr != nil && !isExpectedWorkerClose(runErr) {
			finishCtx, cancel := context.WithTimeout(context.Background(), workerHTTPTimeout)
			_ = reportWorkerFinish(finishCtx, httpClient, daemonURL, cfg.SessionID, generation, runErr)
			cancel()
		}
		if conn != nil {
			_ = conn.Close()
		}
		_ = store.Close()
	}()

	wsURL, err := workerConnectURL(daemonURL, cfg.SessionID, cfg.Activation)
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
		return fmt.Errorf("connect worker websocket: %w", workerWebSocketDialError(err, resp))
	}
	if resp != nil {
		generation = strings.TrimSpace(resp.Header.Get(wire.GenerationHeader))
	}
	if generation == "" {
		return fmt.Errorf("connect worker websocket: missing worker generation")
	}
	logWorkerBootStep(cfg.SessionID, "websocket_connect", stepStarted, bootStarted)
	clientWriter = newWorkerClientWriter(conn, defaultWorkerClientPingInterval, defaultWorkerClientPongWait)
	stopKeepalive = clientWriter.Close
	displayBarrier := newWorkerDisplayBarrierClient(clientWriter)
	commandCh, inputCh, approvalCh, runtimeToolResults, readErrCh := readWorkerMessages(conn, displayBarrier.Acknowledge)
	runtimeToolClient := &workerRuntimeToolClient{writer: clientWriter, results: runtimeToolResults}

	stepStarted = time.Now()
	runtime, err = newWorkerRuntime(ctx, workerRuntimeConfig{
		SessionID:         cfg.SessionID,
		WorkspaceID:       resp.Header.Get(wire.WorkspaceHeader),
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
	logWorkerBootStep(cfg.SessionID, "runtime", stepStarted, bootStarted)

	stepStarted = time.Now()
	cursor, err := loadWorkerCommandCursor(ctx, store, cfg.SessionID)
	if err != nil {
		return err
	}
	cursor, err = replayWorkerCommands(ctx, httpClient, daemonURL, cfg.SessionID, runtime, cursor)
	if err != nil {
		return err
	}
	logWorkerBootStep(cfg.SessionID, "command_replay", stepStarted, bootStarted)
	select {
	case err := <-readErrCh:
		runErr = err
		return err
	default:
	}

	stepStarted = time.Now()
	if err := reportWorkerReady(ctx, httpClient, daemonURL, cfg.SessionID, generation); err != nil {
		return fmt.Errorf("report worker ready: %w", err)
	}
	logWorkerBootStep(cfg.SessionID, "ready", stepStarted, bootStarted)
	_ = runtime.NotifyIdleIfIdle(context.Background(), cursor.Sequence)

	for {
		select {
		case err := <-runtime.fatalCh:
			runErr = err
			return err
		case err := <-readErrCh:
			runErr = err
			if isExpectedWorkerClose(err) {
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
			cursor, err = replayWorkerCommands(ctx, httpClient, daemonURL, cfg.SessionID, runtime, cursor)
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
				recovered, recoverErr := recoverWorkerCommandCursor(ctx, store, cfg.SessionID)
				if recoverErr != nil {
					runErr = recoverErr
					return recoverErr
				}
				if recovered.Sequence > cursor.Sequence {
					cursor = recovered
					if err := saveWorkerCommandCursor(cfg.SessionID, cursor); err != nil {
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

func readWorkerMessages(conn *websocket.Conn, acknowledgeDisplayBarrier func(string)) (<-chan wire.Command, <-chan wire.InputRequest, <-chan wire.ApprovalDecision, <-chan wire.RuntimeToolResult, <-chan error) {
	commandCh := make(chan wire.Command, workerCommandBufferSize)
	inputCh := make(chan wire.InputRequest, workerCommandBufferSize)
	approvalCh := make(chan wire.ApprovalDecision, workerCommandBufferSize)
	runtimeToolResults := make(chan wire.RuntimeToolResult, workerCommandBufferSize)
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

func ensureWorkerSession(ctx context.Context, store zotigosession.Store, sessionID string, cwd string) (*zotigosession.Session, error) {
	sess, err := store.Get(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	if sess != nil {
		return sess, nil
	}
	now := time.Now().UTC()
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	sess = &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:               sessionID,
			WorkingDirectory: cwd,
			CreatedAt:        now,
			UpdatedAt:        now,
		},
		AgentSnapshot: agent.Snapshot{
			State:     agent.StateIdle,
			CreatedAt: now,
		},
		Turns: make([]zotigosession.Turn, 0),
	}
	if err := store.Put(ctx, sess); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return sess, nil
}

func postWorkerJSON(ctx context.Context, client *http.Client, daemonURL string, path string, value any) error {
	data, err := sonic.Marshal(value)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(daemonURL, "/")+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("worker post %s failed: %s", path, resp.Status)
	}
	return nil
}

func reportWorkerFinish(ctx context.Context, client *http.Client, daemonURL string, sessionID string, generation string, err error) error {
	body := wire.FinishSessionRequest{Generation: generation}
	if err != nil && !isExpectedWorkerClose(err) {
		body.Error = err.Error()
		if errors.Is(err, errRuntimeOccupied) {
			body.ErrorCode = "runtime_occupied"
			body.Error = strings.TrimPrefix(body.Error, errRuntimeOccupied.Error()+": ")
		}
	}
	return postWorkerJSON(ctx, client, daemonURL, "/internal/sessions/"+url.PathEscape(sessionID)+"/worker/finish", body)
}

func reportWorkerReady(ctx context.Context, client *http.Client, daemonURL string, sessionID string, generation string) error {
	return postWorkerJSON(ctx, client, daemonURL, "/internal/sessions/"+url.PathEscape(sessionID)+"/worker/attach", wire.ReadyRequest{Generation: generation})
}

func workerWebSocketDialError(dialErr error, resp *http.Response) error {
	if resp == nil {
		return dialErr
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, workerDialErrorBodyLimit+1))
	if readErr != nil {
		return fmt.Errorf("%s (read response body: %v): %w", resp.Status, readErr, dialErr)
	}
	truncated := len(body) > workerDialErrorBodyLimit
	if truncated {
		body = body[:workerDialErrorBodyLimit]
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		return fmt.Errorf("%s: %w", resp.Status, dialErr)
	}
	if truncated {
		text += "…"
	}
	return fmt.Errorf("%s body=%q: %w", resp.Status, text, dialErr)
}

func logWorkerBootStep(sessionID string, step string, started time.Time, bootStarted time.Time) {
	debug.Logf(
		"worker boot session=%s step=%s elapsed_ms=%d total_ms=%d",
		sessionID,
		step,
		time.Since(started).Milliseconds(),
		time.Since(bootStarted).Milliseconds(),
	)
}

func logWorkerRuntimeStep(sessionID string, step string, started time.Time) {
	debug.Logf("worker runtime session=%s step=%s elapsed_ms=%d", sessionID, step, time.Since(started).Milliseconds())
}

func isExpectedWorkerClose(err error) bool {
	if err == nil {
		return true
	}
	var uncertain *profileStateUncertainError
	var completionUncertain *profileCompletionUncertainError
	return errors.As(err, &uncertain) || errors.As(err, &completionUncertain) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, zotigotransport.ErrTransportClosed) ||
		strings.Contains(err.Error(), "websocket: close") ||
		strings.Contains(err.Error(), "use of closed network connection") ||
		strings.Contains(err.Error(), "EOF")
}

func workerConnectURL(daemonURL string, sessionID string, activation uint64) (string, error) {
	parsed, err := url.Parse(daemonURL)
	if err != nil {
		return "", fmt.Errorf("parse daemon url: %w", err)
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported daemon url scheme %q", parsed.Scheme)
	}
	parsed.Path = "/internal/workers/connect"
	values := parsed.Query()
	values.Set("session_id", sessionID)
	values.Set("activation", strconv.FormatUint(activation, 10))
	parsed.RawQuery = values.Encode()
	return parsed.String(), nil
}

func acquireWorkerSessionLock(ctx context.Context, store zotigosession.Store, sessionID string) (func() error, error) {
	if err := store.Lock(ctx, sessionID); err != nil {
		return nil, fmt.Errorf("lock session %s: %w", sessionID, err)
	}
	return func() error {
		return store.Unlock(context.Background(), sessionID)
	}, nil
}
