package zotigod

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/gorilla/websocket"
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/lsp"
	"github.com/jayyao97/zotigo/core/middleware"
	"github.com/jayyao97/zotigo/core/observability"
	"github.com/jayyao97/zotigo/core/protocol"
	_ "github.com/jayyao97/zotigo/core/providers/anthropic"
	_ "github.com/jayyao97/zotigo/core/providers/gemini"
	_ "github.com/jayyao97/zotigo/core/providers/openai"
	"github.com/jayyao97/zotigo/core/runner"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/tools"
	"github.com/jayyao97/zotigo/core/tools/builtin"
	zotigotransport "github.com/jayyao97/zotigo/core/transport"
	"github.com/jayyao97/zotigo/internal/sessionadapter"
	"github.com/jayyao97/zotigo/internal/wiring"
)

const workerHTTPTimeout = 10 * time.Second

const (
	defaultWorkerClientPingInterval = 15 * time.Second
	defaultWorkerClientPongWait     = 45 * time.Second
	workerCommandBufferSize         = 32
)

type workerClientConfig struct {
	DaemonURL string
	SessionID string
}

func runWorkerClient(ctx context.Context, cfg workerClientConfig) error {
	if strings.TrimSpace(cfg.SessionID) == "" {
		return fmt.Errorf("session_id is required")
	}
	daemonURL := strings.TrimRight(cfg.DaemonURL, "/")
	if daemonURL == "" {
		return fmt.Errorf("daemon_url is required")
	}

	store, err := zotigosession.NewFileStore("")
	if err != nil {
		return fmt.Errorf("open session store: %w", err)
	}
	unlock, err := acquireWorkerSessionLock(ctx, store, cfg.SessionID)
	if err != nil {
		_ = store.Close()
		return err
	}
	defer unlock()
	defer func() { _ = store.Close() }()

	httpClient := &http.Client{Timeout: workerHTTPTimeout}
	runtime, err := newWorkerRuntime(ctx, workerRuntimeConfig{
		SessionID:  cfg.SessionID,
		DaemonURL:  daemonURL,
		Store:      store,
		HTTPClient: httpClient,
	})
	if err != nil {
		return err
	}
	defer runtime.Close()

	cursor, err := loadWorkerCommandCursor(ctx, store, cfg.SessionID)
	if err != nil {
		return err
	}
	cursor, err = replayWorkerCommands(ctx, httpClient, daemonURL, cfg.SessionID, runtime, cursor)
	if err != nil {
		return err
	}

	wsURL, err := workerConnectURL(daemonURL, cfg.SessionID)
	if err != nil {
		return err
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("connect worker websocket: %w", err)
	}
	defer func() { _ = conn.Close() }()
	stopKeepalive := startWorkerClientKeepalive(conn, defaultWorkerClientPingInterval, defaultWorkerClientPongWait)
	defer stopKeepalive()

	var runErr error
	defer func() {
		if runErr == nil || isExpectedWorkerClose(runErr) {
			return
		}
		finishCtx, cancel := context.WithTimeout(context.Background(), workerHTTPTimeout)
		defer cancel()
		_ = reportWorkerFinish(finishCtx, httpClient, daemonURL, cfg.SessionID, runErr)
	}()

	commandCh, readErrCh := readWorkerCommands(conn)
	for {
		select {
		case err := <-readErrCh:
			runErr = err
			return runErr
		case command, ok := <-commandCh:
			if !ok {
				runErr = <-readErrCh
				return runErr
			}
			if command.Sequence <= cursor.Sequence {
				continue
			}
			if err := runtime.HandleCommand(ctx, command); err != nil {
				runErr = err
				return err
			}
			cursor.Sequence = command.Sequence
			cursor.Offset = advanceWorkerCommandOffset(ctx, store, cfg.SessionID, cursor.Offset, cursor.Sequence)
			if err := saveWorkerCommandCursor(cfg.SessionID, cursor); err != nil {
				runErr = err
				return err
			}
		}
	}
}

type workerCommandCursor struct {
	Offset   int64  `json:"offset"`
	Sequence uint64 `json:"sequence"`
}

type workerRuntimeConfig struct {
	SessionID  string
	DaemonURL  string
	Store      zotigosession.Store
	HTTPClient *http.Client
}

func readWorkerCommands(conn *websocket.Conn) (<-chan commandResponse, <-chan error) {
	commandCh := make(chan commandResponse, workerCommandBufferSize)
	errCh := make(chan error, 1)
	go func() {
		defer close(commandCh)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			var msg workerMessage
			if err := sonic.Unmarshal(data, &msg); err != nil {
				errCh <- fmt.Errorf("decode worker message: %w", err)
				return
			}
			if msg.Type != workerMessageCommand || msg.Command == nil {
				continue
			}
			select {
			case commandCh <- *msg.Command:
			default:
				errCh <- fmt.Errorf("worker command buffer full")
				_ = conn.Close()
				return
			}
		}
	}()
	return commandCh, errCh
}

type workerRuntime struct {
	sessionID string
	store     zotigosession.Store
	agent     *agent.Agent
	runner    *runner.Runner
	transport *workerRuntimeTransport
	display   *workerDisplayLog
	cleanup   func()

	mu         sync.Mutex
	turnCancel context.CancelFunc
	turnActive bool
	turnReady  chan struct{}
	turnDone   chan struct{}
	readyDone  bool
	doneDone   bool
}

func newWorkerRuntime(ctx context.Context, cfg workerRuntimeConfig) (*workerRuntime, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolve workdir: %w", err)
	}
	sess, err := ensureWorkerSession(ctx, cfg.Store, cfg.SessionID, cwd)
	if err != nil {
		return nil, err
	}

	cm := config.NewManager()
	appConfig, err := cm.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	profileName := appConfig.DefaultProfile
	profile, ok := appConfig.Profiles[profileName]
	if !ok {
		return nil, fmt.Errorf("profile %q not found in config", profileName)
	}

	localExec, err := executor.NewLocalExecutor(cwd)
	if err != nil {
		return nil, fmt.Errorf("create executor: %w", err)
	}

	readTracker := tools.NewReadTracker(cwd)
	skills, err := wiring.NewSkillManager(cwd)
	if err != nil {
		_ = localExec.Close()
		return nil, fmt.Errorf("load skills: %w", err)
	}
	home, _ := os.UserHomeDir()
	transcriptDir := filepath.Join(home, ".zotigo", "sessions", "compacted")

	ag, err := wiring.NewAgent(wiring.AgentConfig{
		Config:      appConfig,
		ProfileName: profileName,
		Profile:     profile,
		Executor:    localExec,
		PromptBuilder: wiring.NewSystemPromptBuilder(wiring.PromptConfig{
			WorkDir:      cwd,
			SkillManager: skills,
		}),
		UserContextBuilder: wiring.NewUserContextBuilder(wiring.PromptConfig{
			WorkDir:                    cwd,
			IncludeProjectInstructions: true,
		}),
		ApprovalPolicy:      agent.ApprovalPolicyAuto,
		TranscriptDir:       transcriptDir,
		Observer:            observability.Noop{},
		ConfigureClassifier: true,
		Middleware: []agent.Middleware{
			middleware.ReadTracker(readTracker),
		},
	})
	if err != nil {
		_ = localExec.Close()
		return nil, fmt.Errorf("create agent: %w", err)
	}
	agent.WithSkillManager(skills)(ag)
	ag.Restore(sess.AgentSnapshot)

	lspManager := lsp.NewManager(cwd)
	if err := wiring.RegisterDefaultTools(ag, wiring.ToolSetConfig{
		Config:      appConfig,
		Profile:     profile,
		ShellPolicy: builtin.DefaultShellPolicy(),
		LSPManager:  lspManager,
		Spawn:       true,
	}); err != nil {
		_ = lspManager.StopAll()
		_ = localExec.Close()
		return nil, fmt.Errorf("register tools: %w", err)
	}

	display := newWorkerDisplayLog(cfg.SessionID, storedDisplayItemSource{store: cfg.Store})
	if err := display.InterruptOpenTurn(ctx, workerRestartedReason); err != nil {
		_ = lspManager.StopAll()
		_ = localExec.Close()
		return nil, fmt.Errorf("repair open display turn: %w", err)
	}
	transport := newWorkerRuntimeTransport(cfg.SessionID, cfg.DaemonURL, cfg.HTTPClient, display)
	runtime := &workerRuntime{
		sessionID: cfg.SessionID,
		store:     cfg.Store,
		agent:     ag,
		transport: transport,
		display:   display,
	}
	runtime.runner = runner.New(ag, transport, runner.WithListeners(runner.Listeners{
		AfterTurn: func(snap agent.Snapshot) {
			_ = runtime.saveSnapshot(context.Background(), snap)
		},
		OnPause: func(snap agent.Snapshot) {
			_ = runtime.saveSnapshot(context.Background(), snap)
		},
	}))

	runtime.cleanup = func() {
		_ = lspManager.StopAll()
		_ = localExec.Close()
	}
	return runtime, nil
}

func (r *workerRuntime) Close() {
	r.mu.Lock()
	active := r.turnActive
	if r.turnCancel != nil {
		r.turnCancel()
	}
	r.mu.Unlock()
	if active && r.display != nil {
		_ = r.display.Interrupt(context.Background(), controlChannelClosedReason)
	}
	if r.transport != nil {
		_ = r.transport.Close()
	}
	if r.cleanup != nil {
		r.cleanup()
	}
}

func (r *workerRuntime) HandleCommand(ctx context.Context, command commandResponse) error {
	switch command.Type {
	case sessionCommandMessage:
		return r.startMessageTurn(ctx, command.Text)
	case sessionCommandPause:
		return r.pauseTurn(ctx, command)
	case sessionCommandSteering:
		return r.queueTurnUserInput(ctx, command)
	default:
		return nil
	}
}

func (r *workerRuntime) pauseTurn(ctx context.Context, command commandResponse) error {
	currentTurnID := r.display.CurrentTurnID()
	if currentTurnID == "" {
		return nil
	}
	if command.TurnID != "" && command.TurnID != currentTurnID {
		return nil
	}
	r.cancelCurrentTurn()
	return r.display.Interrupt(ctx, command.Reason)
}

func (r *workerRuntime) queueTurnUserInput(ctx context.Context, command commandResponse) error {
	text := strings.TrimSpace(command.Text)
	if text == "" {
		return nil
	}

	r.mu.Lock()
	active := r.turnActive
	ready := r.turnReady
	done := r.turnDone
	r.mu.Unlock()
	if !active || ready == nil || done == nil {
		return nil
	}

	currentTurnID := r.display.CurrentTurnID()
	if command.TurnID != "" && currentTurnID != "" && command.TurnID != currentTurnID {
		return nil
	}
	select {
	case <-ready:
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	currentTurnID = r.display.CurrentTurnID()
	if currentTurnID == "" || (command.TurnID != "" && command.TurnID != currentTurnID) {
		return nil
	}
	if err := r.agent.QueueTurnUserInput(text); err != nil {
		if isStaleTurnUserInputError(err) {
			return nil
		}
		return err
	}
	return nil
}

func isStaleTurnUserInputError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "agent is not running")
}

func (r *workerRuntime) startMessageTurn(ctx context.Context, text string) error {
	r.mu.Lock()
	if r.turnActive {
		r.mu.Unlock()
		return nil
	}
	turnCtx, cancel := context.WithCancel(context.Background())
	r.turnCancel = cancel
	r.turnActive = true
	r.turnReady = make(chan struct{})
	r.turnDone = make(chan struct{})
	r.readyDone = false
	r.doneDone = false
	r.mu.Unlock()

	if _, err := r.display.StartTurn(ctx); err != nil {
		r.finishTurn()
		return err
	}
	go func() {
		err := r.runner.RunFullTurnStarted(turnCtx, protocol.NewUserMessage(text), r.markTurnReady)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, zotigotransport.ErrTransportClosed) {
			_ = r.display.Fail(context.Background(), err)
		}
		_ = r.saveSnapshot(context.Background(), r.agent.Snapshot())
		r.finishTurn()
	}()
	return nil
}

func (r *workerRuntime) cancelCurrentTurn() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.turnCancel != nil {
		r.turnCancel()
	}
}

func (r *workerRuntime) finishTurn() {
	r.agent.ClearPendingTurnUserInput()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeTurnReadyLocked()
	r.closeTurnDoneLocked()
	r.turnCancel = nil
	r.turnActive = false
	r.turnReady = nil
	r.turnDone = nil
}

func (r *workerRuntime) markTurnReady() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeTurnReadyLocked()
}

func (r *workerRuntime) closeTurnReadyLocked() {
	if r.turnReady == nil || r.readyDone {
		return
	}
	close(r.turnReady)
	r.readyDone = true
}

func (r *workerRuntime) closeTurnDoneLocked() {
	if r.turnDone == nil || r.doneDone {
		return
	}
	close(r.turnDone)
	r.doneDone = true
}

func (r *workerRuntime) saveSnapshot(ctx context.Context, snap agent.Snapshot) error {
	sess, err := ensureWorkerSession(ctx, r.store, r.sessionID, "")
	if err != nil {
		return err
	}
	sessionadapter.ApplySnapshot(sess, snap, sessionadapter.LastUserPrompt(snap.History))
	return r.store.Put(ctx, sess)
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

func reportWorkerFinish(ctx context.Context, client *http.Client, daemonURL string, sessionID string, err error) error {
	body := finishSessionRequest{}
	if err != nil && !isExpectedWorkerClose(err) {
		body.Error = err.Error()
	}
	return postWorkerJSON(ctx, client, daemonURL, "/internal/sessions/"+url.PathEscape(sessionID)+"/worker/finish", body)
}

func replayWorkerCommands(ctx context.Context, client *http.Client, daemonURL string, sessionID string, runtime *workerRuntime, cursor workerCommandCursor) (workerCommandCursor, error) {
	for {
		previousOffset := cursor.Offset
		commands, err := fetchWorkerCommands(ctx, client, daemonURL, sessionID, cursor)
		if err != nil {
			return cursor, err
		}
		for _, command := range commands.Commands {
			if command.Sequence <= cursor.Sequence {
				continue
			}
			if err := runtime.HandleCommand(ctx, command); err != nil {
				return cursor, err
			}
			cursor.Sequence = command.Sequence
		}
		cursor.Offset = commands.NextOffset
		if err := saveWorkerCommandCursor(sessionID, cursor); err != nil {
			return cursor, err
		}
		if commands.NextOffset == previousOffset || len(commands.Commands) < maxCommandsLimit {
			return cursor, nil
		}
	}
}

func fetchWorkerCommands(ctx context.Context, client *http.Client, daemonURL string, sessionID string, cursor workerCommandCursor) (commandsResponse, error) {
	endpoint := strings.TrimRight(daemonURL, "/") + "/internal/sessions/" + url.PathEscape(sessionID) + "/commands?offset=" + strconv.FormatInt(cursor.Offset, 10) + "&limit=" + strconv.Itoa(maxCommandsLimit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return commandsResponse{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return commandsResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return commandsResponse{}, fmt.Errorf("fetch worker commands failed: %s", resp.Status)
	}
	var body commandsResponse
	if err := sonic.ConfigDefault.NewDecoder(resp.Body).Decode(&body); err != nil {
		return commandsResponse{}, err
	}
	return body, nil
}

func advanceWorkerCommandOffset(ctx context.Context, store zotigosession.Store, sessionID string, offset int64, sequence uint64) int64 {
	if offset < 0 {
		offset = 0
	}
	originalOffset := offset
	type offsetStore interface {
		ListDisplayItemsFromOffset(ctx context.Context, id string, offset int64, maxLines int) ([]zotigosession.DisplayItem, bool, int64, error)
	}
	offsetReader, ok := store.(offsetStore)
	if !ok {
		return offset
	}
	for {
		items, _, nextOffset, err := offsetReader.ListDisplayItemsFromOffset(ctx, sessionID, offset, commandOffsetScanLines)
		if err != nil || nextOffset == offset {
			return offset
		}
		for _, item := range items {
			if item.Sequence == sequence {
				if item.LogOffset > 0 {
					return item.LogOffset
				}
				return nextOffset
			}
			if item.Sequence > sequence {
				return originalOffset
			}
		}
		offset = nextOffset
		if len(items) < commandOffsetScanLines {
			return originalOffset
		}
	}
}

func loadWorkerCommandCursor(ctx context.Context, store zotigosession.Store, sessionID string) (workerCommandCursor, error) {
	data, err := os.ReadFile(workerCommandCursorPath(sessionID))
	if os.IsNotExist(err) {
		return workerCommandCursor{}, nil
	}
	if err != nil {
		return workerCommandCursor{}, err
	}
	var cursor workerCommandCursor
	if err := sonic.Unmarshal(data, &cursor); err == nil {
		return validateWorkerCommandCursor(ctx, store, sessionID, cursor)
	}
	sequence, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return recoverWorkerCommandCursor(ctx, store, sessionID)
	}
	return validateWorkerCommandCursor(ctx, store, sessionID, workerCommandCursor{Sequence: sequence})
}

func validateWorkerCommandCursor(ctx context.Context, store zotigosession.Store, sessionID string, cursor workerCommandCursor) (workerCommandCursor, error) {
	if cursor.Offset < 0 {
		return recoverWorkerCommandCursor(ctx, store, sessionID)
	}
	if store == nil {
		return cursor, nil
	}
	items, _, err := store.ListDisplayItems(ctx, sessionID)
	if err != nil {
		return workerCommandCursor{}, fmt.Errorf("validate worker command cursor: %w", err)
	}
	safeSequence := recoverAppliedCommandSequence(items)
	if cursor.Sequence > safeSequence {
		return workerCommandCursor{Sequence: safeSequence}, nil
	}
	if cursor.Offset > 0 {
		if cursor.Sequence < latestCommandSequence(items) {
			return workerCommandCursor{Sequence: cursor.Sequence}, nil
		}
		type offsetStore interface {
			ListDisplayItemsFromOffset(ctx context.Context, id string, offset int64, maxLines int) ([]zotigosession.DisplayItem, bool, int64, error)
		}
		if offsetReader, ok := store.(offsetStore); ok {
			if _, _, _, err := offsetReader.ListDisplayItemsFromOffset(ctx, sessionID, cursor.Offset, 1); err != nil {
				return workerCommandCursor{Sequence: safeSequence}, nil
			}
		}
	}
	return cursor, nil
}

func latestCommandSequence(items []zotigosession.DisplayItem) uint64 {
	var latest uint64
	for _, item := range items {
		if item.Command != nil && item.Command.Type != "" {
			latest = item.Sequence
		}
		if item.Type == zotigosession.DisplayItemSteeringMessage && commandText(item.Content) != "" {
			latest = item.Sequence
		}
	}
	return latest
}

func recoverWorkerCommandCursor(ctx context.Context, store zotigosession.Store, sessionID string) (workerCommandCursor, error) {
	if store == nil {
		return workerCommandCursor{}, nil
	}
	items, _, err := store.ListDisplayItems(ctx, sessionID)
	if err != nil {
		return workerCommandCursor{}, fmt.Errorf("recover worker command cursor: %w", err)
	}
	return workerCommandCursor{Sequence: recoverAppliedCommandSequence(items)}, nil
}

func recoverAppliedCommandSequence(items []zotigosession.DisplayItem) uint64 {
	commandSeqs := make([]uint64, 0)
	safe := make(map[uint64]bool)
	pendingMessages := make([]uint64, 0)
	pendingByTurn := make(map[string][]uint64)

	for _, item := range items {
		if item.Command != nil && item.Command.Type != "" {
			commandSeqs = append(commandSeqs, item.Sequence)
			switch item.Command.Type {
			case sessionCommandMessage:
				pendingMessages = append(pendingMessages, item.Sequence)
			case sessionCommandPause:
				if item.Command.TurnID != "" {
					pendingByTurn[item.Command.TurnID] = append(pendingByTurn[item.Command.TurnID], item.Sequence)
				}
			default:
				safe[item.Sequence] = true
			}
		}
		if item.Type == zotigosession.DisplayItemSteeringMessage && commandText(item.Content) != "" {
			commandSeqs = append(commandSeqs, item.Sequence)
			if item.Turn != nil && item.Turn.ID != "" {
				pendingByTurn[item.Turn.ID] = append(pendingByTurn[item.Turn.ID], item.Sequence)
			}
		}

		switch item.Type {
		case zotigosession.DisplayItemTurnStarted:
			if len(pendingMessages) > 0 {
				safe[pendingMessages[0]] = true
				pendingMessages = pendingMessages[1:]
			}
		case zotigosession.DisplayItemTurnCompleted, zotigosession.DisplayItemTurnFailed, zotigosession.DisplayItemTurnInterrupted:
			if item.Turn == nil || item.Turn.ID == "" {
				continue
			}
			for _, seq := range pendingByTurn[item.Turn.ID] {
				safe[seq] = true
			}
			delete(pendingByTurn, item.Turn.ID)
		}
	}

	var cursor uint64
	for _, seq := range commandSeqs {
		if !safe[seq] {
			return cursor
		}
		cursor = seq
	}
	return cursor
}

func saveWorkerCommandCursor(sessionID string, cursor workerCommandCursor) error {
	path := workerCommandCursorPath(sessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := sonic.Marshal(cursor)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func workerCommandCursorPath(sessionID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "zotigo-"+sessionID+".cursor")
	}
	return filepath.Join(home, ".zotigo", "sessions", sessionID+".worker.cursor")
}

func isExpectedWorkerClose(err error) bool {
	if err == nil {
		return true
	}
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, zotigotransport.ErrTransportClosed) ||
		strings.Contains(err.Error(), "websocket: close") ||
		strings.Contains(err.Error(), "use of closed network connection") ||
		strings.Contains(err.Error(), "EOF")
}

func workerConnectURL(daemonURL string, sessionID string) (string, error) {
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
	parsed.RawQuery = values.Encode()
	return parsed.String(), nil
}

func startWorkerClientKeepalive(conn *websocket.Conn, pingInterval time.Duration, pongWait time.Duration) func() {
	if pongWait > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(pongWait))
		})
	}
	if pingInterval <= 0 {
		return func() {}
	}

	done := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if err := conn.SetWriteDeadline(time.Now().Add(workerWriteWait)); err != nil {
					_ = conn.Close()
					return
				}
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()
	return func() {
		stopOnce.Do(func() {
			close(done)
		})
	}
}

func acquireWorkerSessionLock(ctx context.Context, store zotigosession.Store, sessionID string) (func(), error) {
	if err := store.Lock(ctx, sessionID); err != nil {
		return nil, fmt.Errorf("lock session %s: %w", sessionID, err)
	}
	return func() {
		_ = store.Unlock(context.Background(), sessionID)
	}, nil
}
