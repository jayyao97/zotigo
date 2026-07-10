package zotigod

import (
	"bytes"
	"context"
	"encoding/base64"
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

func runWorkerClient(ctx context.Context, cfg workerClientConfig) (returnErr error) {
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
	var runtime *workerRuntime
	var conn *websocket.Conn
	stopKeepalive := func() {}
	var runErr error
	httpClient := &http.Client{Timeout: workerHTTPTimeout}
	defer func() {
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
		if conn != nil {
			_ = conn.Close()
		}
		if runErr != nil && !isExpectedWorkerClose(runErr) {
			finishCtx, cancel := context.WithTimeout(context.Background(), workerHTTPTimeout)
			defer cancel()
			_ = reportWorkerFinish(finishCtx, httpClient, daemonURL, cfg.SessionID, runErr)
		}
		_ = store.Close()
	}()

	runtime, err = newWorkerRuntime(ctx, workerRuntimeConfig{
		SessionID:  cfg.SessionID,
		DaemonURL:  daemonURL,
		Store:      store,
		HTTPClient: httpClient,
	})
	if err != nil {
		return err
	}

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
	conn, _, err = websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("connect worker websocket: %w", err)
	}
	stopKeepalive = startWorkerClientKeepalive(conn, defaultWorkerClientPingInterval, defaultWorkerClientPongWait)

	commandCh, readErrCh := readWorkerCommands(conn)
	for {
		select {
		case err := <-runtime.fatalCh:
			runErr = err
			return err
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
			cursor, err = replayWorkerCommands(ctx, httpClient, daemonURL, cfg.SessionID, runtime, cursor)
			if err != nil {
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
	sessionID    string
	workDir      string
	store        zotigosession.Store
	agent        *agent.Agent
	runner       *runner.Runner
	transport    *workerRuntimeTransport
	display      *workerDisplayLog
	observer     observability.Observer
	cleanup      func()
	storeMu      sync.Mutex
	profileMu    sync.Mutex
	profileEpoch uint64
	fatalCh      chan error
	fatalMu      sync.Mutex
	fatalErr     error

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
	if strings.TrimSpace(sess.WorkingDirectory) != "" {
		cwd = sess.WorkingDirectory
	}

	cm := config.NewManager()
	appConfig, err := cm.LoadForDir(cwd)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	profileName, profile, err := resolveWorkerProfile(sess, appConfig)
	if err != nil {
		return nil, err
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
	observer := wiring.NewObserver(appConfig.Observability, cfg.SessionID, map[string]any{
		"zotigo_session": cfg.SessionID,
		"process_start":  time.Now().UTC().Format(time.RFC3339),
		"worker":         true,
	})

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
		Observer:            observer,
		ConfigureClassifier: true,
		Middleware: []agent.Middleware{
			middleware.ReadTracker(readTracker),
		},
	})
	if err != nil {
		_ = observer.Close(context.Background())
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
		_ = observer.Close(context.Background())
		_ = lspManager.StopAll()
		_ = localExec.Close()
		return nil, fmt.Errorf("register tools: %w", err)
	}

	display := newWorkerDisplayLog(cfg.SessionID, storedDisplayItemSource{store: cfg.Store})
	if err := display.InterruptOpenTurn(ctx, workerRestartedReason); err != nil {
		_ = observer.Close(context.Background())
		_ = lspManager.StopAll()
		_ = localExec.Close()
		return nil, fmt.Errorf("repair open display turn: %w", err)
	}
	transport := newWorkerRuntimeTransport(cfg.SessionID, cfg.DaemonURL, cfg.HTTPClient, display)
	runtime := &workerRuntime{
		sessionID: cfg.SessionID,
		workDir:   cwd,
		store:     cfg.Store,
		agent:     ag,
		transport: transport,
		display:   display,
		observer:  observer,
		fatalCh:   make(chan error, 1),
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
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = observer.Close(closeCtx)
		_ = lspManager.StopAll()
		_ = localExec.Close()
	}
	return runtime, nil
}

func resolveWorkerProfile(sess *zotigosession.Session, appConfig *config.Config) (string, config.ProfileConfig, error) {
	return appConfig.ResolveProfile(sess.ProfileName)
}

func (r *workerRuntime) Close() {
	r.mu.Lock()
	active := r.turnActive
	done := r.turnDone
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
	if active && done != nil {
		<-done
	}
	if r.agent != nil {
		_ = r.agent.WaitForRuntimeIdle(context.Background())
	}
	if r.cleanup != nil {
		r.cleanup()
	}
}

func (r *workerRuntime) HandleCommand(ctx context.Context, command commandResponse) error {
	_, err := r.handleCommand(ctx, command)
	return err
}

func (r *workerRuntime) handleCommand(ctx context.Context, command commandResponse) (<-chan error, error) {
	if err := r.currentFatalError(); err != nil {
		return nil, err
	}
	if err := validateWorkerCommand(command); err != nil {
		return nil, err
	}
	switch command.Type {
	case sessionCommandMessage:
		return nil, r.startMessageTurn(ctx, command.ID, command.Message)
	case sessionCommandPause:
		return nil, r.pauseTurn(ctx, command.Pause)
	case sessionCommandSteering:
		return nil, r.queueTurnUserInput(ctx, command.Steering)
	case sessionCommandProfile:
		return r.switchProfile(ctx, command.ID, command.Profile)
	default:
		return nil, nil
	}
}

func validateWorkerCommand(command commandResponse) error {
	payloads := 0
	if command.Message != nil {
		payloads++
	}
	if command.Pause != nil {
		payloads++
	}
	if command.Steering != nil {
		payloads++
	}
	if command.Profile != nil {
		payloads++
	}
	switch command.Type {
	case sessionCommandMessage:
		if command.Message == nil || payloads != 1 {
			return fmt.Errorf("invalid message command payload")
		}
	case sessionCommandPause:
		if command.Pause == nil || payloads != 1 {
			return fmt.Errorf("invalid pause command payload")
		}
	case sessionCommandSteering:
		if command.Steering == nil || payloads != 1 {
			return fmt.Errorf("invalid steering command payload")
		}
	case sessionCommandProfile:
		if command.Profile == nil || strings.TrimSpace(command.Profile.Name) == "" || payloads != 1 {
			return fmt.Errorf("invalid profile command payload")
		}
	default:
		return nil
	}
	return nil
}

func (r *workerRuntime) switchProfile(ctx context.Context, commandID string, command *profileCommandPayload) (<-chan error, error) {
	completion := make(chan error, 1)
	target := strings.TrimSpace(command.Name)
	epoch, err := r.nextProfileEpoch()
	if err != nil {
		close(completion)
		return completion, err
	}
	r.agent.SupersedePendingRuntimeProfile()
	appConfig, err := config.NewManager().LoadForDir(r.workDir)
	if err != nil {
		r.completeProfileFailure(ctx, completion, commandID, target, fmt.Errorf("load profiles: %w", err))
		return completion, nil
	}
	_, profile, err := appConfig.ResolveProfile(target)
	if err != nil {
		r.completeProfileFailure(ctx, completion, commandID, target, err)
		return completion, nil
	}
	runtimeProfile, err := wiring.NewRuntimeProfile(wiring.AgentConfig{
		Config:              appConfig,
		ProfileName:         target,
		Profile:             profile,
		Observer:            r.observer,
		ConfigureClassifier: true,
	})
	if err != nil {
		r.completeProfileFailure(ctx, completion, commandID, target, err)
		return completion, nil
	}
	from := r.agent.ActiveProfileName()
	runtimeProfile.BeforeApply = func() error {
		commitCtx, cancel := context.WithTimeout(context.Background(), workerHTTPTimeout)
		defer cancel()
		return r.commitLatestProfileSwitch(commitCtx, epoch, commandID, from, target)
	}
	result := r.agent.QueueRuntimeProfile(runtimeProfile)
	go r.finishProfileSwitch(commandID, target, result, completion)
	return completion, nil
}

func (r *workerRuntime) nextProfileEpoch() (uint64, error) {
	r.profileMu.Lock()
	defer r.profileMu.Unlock()
	if err := r.currentFatalError(); err != nil {
		return 0, err
	}
	r.profileEpoch++
	return r.profileEpoch, nil
}

func (r *workerRuntime) commitLatestProfileSwitch(ctx context.Context, epoch uint64, commandID string, from string, target string) error {
	r.profileMu.Lock()
	defer r.profileMu.Unlock()
	if r.profileEpoch != epoch {
		return agent.ErrRuntimeProfileSuperseded
	}
	err := r.commitProfileSwitch(ctx, commandID, from, target)
	var uncertain *profileStateUncertainError
	if errors.As(err, &uncertain) {
		r.fail(uncertain)
	}
	return err
}

func (r *workerRuntime) finishProfileSwitch(commandID string, target string, result <-chan error, completion chan<- error) {
	defer close(completion)
	err := <-result
	var uncertain *profileStateUncertainError
	if errors.As(err, &uncertain) {
		r.fail(uncertain)
		completion <- uncertain
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if errors.Is(err, agent.ErrRuntimeProfileSuperseded) {
		err = r.recordProfileFailure(ctx, commandID, target, agent.ErrRuntimeProfileSuperseded)
		if err != nil {
			r.fail(err)
		}
		completion <- err
		return
	}
	if err != nil {
		err = r.recordProfileFailure(ctx, commandID, target, err)
		if err != nil {
			r.fail(err)
		}
		completion <- err
		return
	}
	completion <- nil
}

func (r *workerRuntime) completeProfileFailure(ctx context.Context, completion chan<- error, commandID string, target string, cause error) {
	err := r.recordProfileFailure(ctx, commandID, target, cause)
	if err != nil {
		r.fail(err)
	}
	completion <- err
	close(completion)
}

type profileStateUncertainError struct {
	cause error
}

func (e *profileStateUncertainError) Error() string {
	return "profile state is uncertain: " + e.cause.Error()
}
func (e *profileStateUncertainError) Unwrap() error { return e.cause }

func (r *workerRuntime) fail(err error) {
	if err == nil {
		return
	}
	r.fatalMu.Lock()
	if r.fatalErr == nil {
		r.fatalErr = err
	}
	fatalErr := r.fatalErr
	r.fatalMu.Unlock()
	if r.fatalCh == nil {
		return
	}
	select {
	case r.fatalCh <- fatalErr:
	default:
	}
}

func (r *workerRuntime) currentFatalError() error {
	r.fatalMu.Lock()
	defer r.fatalMu.Unlock()
	return r.fatalErr
}

func (r *workerRuntime) commitProfileSwitch(ctx context.Context, commandID string, from string, target string) error {
	r.storeMu.Lock()
	defer r.storeMu.Unlock()
	sess, err := ensureWorkerSession(ctx, r.store, r.sessionID, r.workDir)
	if err != nil {
		return err
	}
	previousProfile := sess.ProfileName
	previousUpdatedAt := sess.UpdatedAt
	sess.ProfileName = target
	sess.UpdatedAt = time.Now().UTC()
	if err := persistSessionProfile(ctx, r.store, sess); err != nil {
		if errors.Is(err, zotigosession.ErrProfileStateUncertain) {
			return &profileStateUncertainError{cause: err}
		}
		return err
	}
	if err := r.display.ProfileChanged(ctx, commandID, from, target); err != nil {
		sess.ProfileName = previousProfile
		sess.UpdatedAt = previousUpdatedAt
		if rollbackErr := persistSessionProfile(ctx, r.store, sess); rollbackErr != nil {
			return &profileStateUncertainError{cause: errors.Join(
				fmt.Errorf("append profile changed: %w", err),
				fmt.Errorf("rollback session profile: %w", rollbackErr),
			)}
		}
		return fmt.Errorf("append profile changed: %w", err)
	}
	return nil
}

func (r *workerRuntime) recordProfileFailure(ctx context.Context, commandID string, target string, err error) error {
	if appendErr := r.display.ProfileFailed(ctx, commandID, r.agent.ActiveProfileName(), target, err); appendErr != nil {
		return appendErr
	}
	return nil
}

func (r *workerRuntime) pauseTurn(ctx context.Context, command *pauseCommandPayload) error {
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

func (r *workerRuntime) queueTurnUserInput(ctx context.Context, command *steeringCommandPayload) error {
	msg, err := userMessageFromCommand(command.Text, command.Images, "steering")
	if err != nil {
		return err
	}
	if len(msg.Content) == 0 {
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
	if err := r.agent.QueueTurnUserMessage(msg); err != nil {
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

func (r *workerRuntime) startMessageTurn(ctx context.Context, commandID string, command *messageCommandPayload) error {
	msg, err := messageFromCommand(commandID, command)
	if err != nil {
		return err
	}

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
		err := r.runner.RunFullTurnStarted(turnCtx, msg, r.markTurnReady)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, zotigotransport.ErrTransportClosed) {
			_ = r.display.Fail(context.Background(), err)
		}
		_ = r.agent.WaitForRuntimeIdle(context.Background())
		_ = r.saveSnapshot(context.Background(), r.agent.Snapshot())
		r.finishTurn()
	}()
	return nil
}

func messageFromCommand(commandID string, command *messageCommandPayload) (protocol.Message, error) {
	return userMessageFromCommand(command.Text, command.Images, fmt.Sprintf("message command %q", commandID))
}

func userMessageFromCommand(text string, images []commandImageData, label string) (protocol.Message, error) {
	msg := protocol.Message{
		Role:      protocol.RoleUser,
		Content:   make([]protocol.ContentPart, 0, 1+len(images)),
		CreatedAt: time.Now(),
	}
	if text = strings.TrimSpace(text); text != "" {
		msg.Content = append(msg.Content, protocol.ContentPart{
			Type: protocol.ContentTypeText,
			Text: text,
		})
	}
	for idx, img := range images {
		if img.DataBase64 == "" {
			return protocol.Message{}, fmt.Errorf("%s image payload unavailable for image %d", label, idx)
		}
		data, err := base64.StdEncoding.Strict().DecodeString(img.DataBase64)
		if err != nil {
			return protocol.Message{}, fmt.Errorf("decode %s image %d: %w", label, idx, err)
		}
		msg.Content = append(msg.Content, protocol.ContentPart{
			Type: protocol.ContentTypeImage,
			Image: &protocol.MediaPart{
				Data:      data,
				MediaType: img.MimeType,
			},
		})
	}
	return msg, nil
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
	r.storeMu.Lock()
	defer r.storeMu.Unlock()
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
	type pendingProfile struct {
		sequence   uint64
		completion <-chan error
	}
	pendingProfiles := make([]pendingProfile, 0)
	flushProfiles := func() error {
		for _, pending := range pendingProfiles {
			select {
			case err := <-pending.completion:
				if err != nil {
					return err
				}
				cursor.Sequence = pending.sequence
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		pendingProfiles = pendingProfiles[:0]
		return nil
	}

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
			if command.Type != sessionCommandProfile {
				if err := flushProfiles(); err != nil {
					return cursor, err
				}
			}
			completion, err := runtime.handleCommand(ctx, command)
			if err != nil {
				return cursor, err
			}
			if completion != nil {
				pendingProfiles = append(pendingProfiles, pendingProfile{sequence: command.Sequence, completion: completion})
				continue
			}
			cursor.Sequence = command.Sequence
		}
		if err := flushProfiles(); err != nil {
			return cursor, err
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
	pendingProfiles := make(map[string]uint64)

	for _, item := range items {
		recordedCommand := false
		if item.Command != nil && item.Command.Type != "" {
			commandSeqs = append(commandSeqs, item.Sequence)
			recordedCommand = true
			switch item.Command.Type {
			case sessionCommandMessage:
				pendingMessages = append(pendingMessages, item.Sequence)
			case sessionCommandPause, sessionCommandSteering:
				if item.Command.TurnID != "" {
					pendingByTurn[item.Command.TurnID] = append(pendingByTurn[item.Command.TurnID], item.Sequence)
				}
			case sessionCommandProfile:
				pendingProfiles[item.ID] = item.Sequence
			default:
				safe[item.Sequence] = true
			}
		}
		if !recordedCommand && item.Type == zotigosession.DisplayItemSteeringMessage && commandText(item.Content) != "" {
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
		case zotigosession.DisplayItemProfileChanged, zotigosession.DisplayItemProfileFailed:
			if item.Profile != nil {
				if seq, ok := pendingProfiles[item.Profile.CommandID]; ok {
					safe[seq] = true
					delete(pendingProfiles, item.Profile.CommandID)
				}
			}
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
	var uncertain *profileStateUncertainError
	return errors.As(err, &uncertain) ||
		errors.Is(err, context.Canceled) ||
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

func acquireWorkerSessionLock(ctx context.Context, store zotigosession.Store, sessionID string) (func() error, error) {
	if err := store.Lock(ctx, sessionID); err != nil {
		return nil, fmt.Errorf("lock session %s: %w", sessionID, err)
	}
	return func() error {
		return store.Unlock(context.Background(), sessionID)
	}, nil
}
