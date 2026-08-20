package zotigod

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
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
	"github.com/jayyao97/zotigo/core/debug"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/lsp"
	"github.com/jayyao97/zotigo/core/middleware"
	"github.com/jayyao97/zotigo/core/observability"
	"github.com/jayyao97/zotigo/core/protocol"
	_ "github.com/jayyao97/zotigo/core/providers/anthropic"
	_ "github.com/jayyao97/zotigo/core/providers/deepseek"
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

const workerDialErrorBodyLimit = 4 * 1024

const (
	defaultWorkerClientPingInterval = 15 * time.Second
	defaultWorkerClientPongWait     = 45 * time.Second
	workerCommandBufferSize         = 32
	workerDeltaBufferSize           = 256
)

type workerClientConfig struct {
	DaemonURL string
	SessionID string
	AuthToken string
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

	wsURL, err := workerConnectURL(daemonURL, cfg.SessionID)
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
		generation = strings.TrimSpace(resp.Header.Get(workerGenerationHeader))
	}
	if generation == "" {
		return fmt.Errorf("connect worker websocket: missing worker generation")
	}
	logWorkerBootStep(cfg.SessionID, "websocket_connect", stepStarted, bootStarted)
	clientWriter = newWorkerClientWriter(conn, defaultWorkerClientPingInterval, defaultWorkerClientPongWait)
	stopKeepalive = clientWriter.Close
	displayBarrier := newWorkerDisplayBarrierClient(clientWriter)
	commandCh, approvalCh, readErrCh := readWorkerMessages(conn, displayBarrier.Acknowledge)

	stepStarted = time.Now()
	runtime, err = newWorkerRuntime(ctx, workerRuntimeConfig{
		SessionID:      cfg.SessionID,
		Store:          store,
		SendDelta:      clientWriter.SendDelta,
		NotifyDisplay:  clientWriter.SendDisplayWake,
		SyncDisplay:    clientWriter.SendDisplayWakeReliable,
		DisplayBarrier: displayBarrier.Wait,
		NotifyApproval: clientWriter.SendApprovalRequest,
		NotifyApprovalResolved: func(ctx context.Context, approval approvalRequestResponse) {
			_ = clientWriter.SendApprovalResult(ctx, workerApprovalResult{Approval: &approval})
		},
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
			if command.Type == sessionCommandSteering && command.Sequence == 0 {
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
			}
		}
	}
}

type workerCommandCursor struct {
	Offset   int64  `json:"offset"`
	Sequence uint64 `json:"sequence"`
}

type workerRuntimeConfig struct {
	SessionID              string
	Store                  zotigosession.Store
	SendDelta              func(displayDeltaEvent)
	NotifyDisplay          func()
	SyncDisplay            func(context.Context) error
	DisplayBarrier         func(context.Context) error
	NotifyApproval         func(context.Context, approvalRequestResponse)
	NotifyApprovalResolved func(context.Context, approvalRequestResponse)
}

func readWorkerMessages(conn *websocket.Conn, acknowledgeDisplayBarrier func(string)) (<-chan commandResponse, <-chan workerApprovalDecision, <-chan error) {
	commandCh := make(chan commandResponse, workerCommandBufferSize)
	approvalCh := make(chan workerApprovalDecision, workerCommandBufferSize)
	errCh := make(chan error, 1)
	go func() {
		defer close(commandCh)
		defer close(approvalCh)
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
			switch msg.Type {
			case workerMessageCommand:
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
			case workerMessageApprovalDecision:
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
			case workerMessageDisplayBarrierOK:
				if msg.DisplayBarrier == nil || msg.DisplayBarrier.ID == "" {
					continue
				}
				if acknowledgeDisplayBarrier != nil {
					acknowledgeDisplayBarrier(msg.DisplayBarrier.ID)
				}
			}
		}
	}()
	return commandCh, approvalCh, errCh
}

type workerRuntime struct {
	sessionID        string
	workDir          string
	store            zotigosession.Store
	agent            *agent.Agent
	runner           *runner.Runner
	transport        *workerRuntimeTransport
	display          *workerDisplayLog
	observer         observability.Observer
	runtimeWAL       *workerRuntimeWAL
	cleanup          func()
	storeMu          sync.Mutex
	profileMu        sync.Mutex
	profileEpoch     uint64
	profileOrderMu   sync.Mutex
	profileOrderTail <-chan struct{}
	fatalCh          chan error
	fatalMu          sync.Mutex
	fatalErr         error

	mu         sync.Mutex
	turnCancel context.CancelFunc
	turnActive bool
	turnReady  chan struct{}
	turnDone   chan struct{}
	readyDone  bool
	doneDone   bool
}

func newWorkerRuntime(ctx context.Context, cfg workerRuntimeConfig) (*workerRuntime, error) {
	stepStarted := time.Now()
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolve workdir: %w", err)
	}
	sess, err := ensureWorkerSession(ctx, cfg.Store, cfg.SessionID, cwd)
	if err != nil {
		return nil, err
	}
	logWorkerRuntimeStep(cfg.SessionID, "ensure_session", stepStarted)
	if strings.TrimSpace(sess.WorkingDirectory) != "" {
		cwd = sess.WorkingDirectory
	}

	stepStarted = time.Now()
	cm := config.NewManager()
	appConfig, err := cm.LoadForDir(cwd)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	profileName, profile, err := resolveWorkerProfile(sess, appConfig)
	if err != nil {
		return nil, err
	}
	approvalPolicy, err := normalizeSessionApprovalPolicy(sess.ApprovalPolicy, true)
	if err != nil {
		return nil, fmt.Errorf("load approval policy: %w", err)
	}
	logWorkerRuntimeStep(cfg.SessionID, "config", stepStarted)

	stepStarted = time.Now()
	localExec, err := executor.NewLocalExecutor(cwd)
	if err != nil {
		return nil, fmt.Errorf("create executor: %w", err)
	}
	logWorkerRuntimeStep(cfg.SessionID, "executor", stepStarted)

	readTracker := tools.NewReadTracker(cwd)
	stepStarted = time.Now()
	skills, err := wiring.NewSkillManager(cwd)
	if err != nil {
		_ = localExec.Close()
		return nil, fmt.Errorf("load skills: %w", err)
	}
	logWorkerRuntimeStep(cfg.SessionID, "skills", stepStarted)
	home, _ := os.UserHomeDir()
	transcriptDir := filepath.Join(home, ".zotigo", "sessions", "compacted")
	observer := wiring.NewObserver(appConfig.Observability, cfg.SessionID, map[string]any{
		"zotigo_session": cfg.SessionID,
		"process_start":  time.Now().UTC().Format(time.RFC3339),
		"worker":         true,
	})
	display := newWorkerDisplayLog(cfg.SessionID, storedDisplayItemSource{store: cfg.Store})
	display.delta = cfg.SendDelta
	display.wake = func(context.Context) {
		if cfg.NotifyDisplay != nil {
			cfg.NotifyDisplay()
		}
	}
	display.barrier = cfg.DisplayBarrier
	display.wakeSync = cfg.SyncDisplay
	runtimeWAL := newWorkerRuntimeWAL(cfg.Store, cfg.SessionID)
	recoveredRuntimeWAL, err := recoverRuntimeWAL(ctx, cfg.Store, sess)
	if err != nil {
		_ = observer.Close(context.Background())
		_ = localExec.Close()
		return nil, fmt.Errorf("recover runtime WAL: %w", err)
	}
	if !recoveredRuntimeWAL {
		err = recoverInterruptedToolExecutions(ctx, cfg.Store, sess)
	}
	if err != nil {
		_ = observer.Close(context.Background())
		_ = localExec.Close()
		return nil, fmt.Errorf("recover interrupted tool executions: %w", err)
	}
	if err := recoverUnansweredToolCalls(ctx, cfg.Store, sess); err != nil {
		_ = observer.Close(context.Background())
		_ = localExec.Close()
		return nil, fmt.Errorf("recover unanswered tool calls: %w", err)
	}
	_, err = display.ResolvePendingApprovalsForOpenTurn(ctx, approvalWorkerRestartedReason)
	if err != nil {
		_ = observer.Close(context.Background())
		_ = localExec.Close()
		return nil, fmt.Errorf("recover pending approval: %w", err)
	}
	if sess.AgentSnapshot.State == agent.StatePaused {
		sess.AgentSnapshot = agent.InterruptPendingSnapshot(sess.AgentSnapshot, approvalWorkerRestartedReason)
		sessionadapter.ApplySnapshot(sess, sess.AgentSnapshot, sessionadapter.LastUserPrompt(sess.AgentSnapshot.History))
		if err := cfg.Store.Put(ctx, sess); err != nil {
			_ = observer.Close(context.Background())
			_ = localExec.Close()
			return nil, fmt.Errorf("persist recovered approval snapshot: %w", err)
		}
	}

	stepStarted = time.Now()
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
		ApprovalPolicy:      approvalPolicy,
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
	logWorkerRuntimeStep(cfg.SessionID, "agent", stepStarted)
	agent.WithSkillManager(skills)(ag)
	durabilityRecorder := &workerDurabilityRecorder{display: display, wal: runtimeWAL}
	if runtimeWAL != nil {
		agent.WithDurabilityRecorder(durabilityRecorder)(ag)
	} else {
		agent.WithToolExecutionRecorder(durabilityRecorder)(ag)
	}
	ag.Restore(sess.AgentSnapshot)

	stepStarted = time.Now()
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
	logWorkerRuntimeStep(cfg.SessionID, "tools", stepStarted)

	if err := display.InterruptOpenTurn(ctx, workerRestartedReason); err != nil {
		_ = observer.Close(context.Background())
		_ = lspManager.StopAll()
		_ = localExec.Close()
		return nil, fmt.Errorf("repair open display turn: %w", err)
	}
	transport := newWorkerRuntimeTransport(cfg.SessionID, display, cfg.NotifyApproval)
	transport.notifyApprovalResolved = cfg.NotifyApprovalResolved
	runtime := &workerRuntime{
		sessionID:  cfg.SessionID,
		workDir:    cwd,
		store:      cfg.Store,
		agent:      ag,
		transport:  transport,
		display:    display,
		observer:   observer,
		runtimeWAL: runtimeWAL,
		fatalCh:    make(chan error, 1),
	}
	if runtimeWAL != nil {
		runtimeWAL.onError = func(err error) {
			runtime.fail(err)
			runtime.cancelCurrentTurn()
		}
	}
	runtime.runner = runner.New(ag, transport, runner.WithListeners(runner.Listeners{
		AfterTurn: func(snap agent.Snapshot) {
			if err := runtime.saveSnapshot(context.Background(), snap); err != nil {
				runtime.fail(fmt.Errorf("persist completed turn: %w", err))
				runtime.cancelCurrentTurn()
			}
		},
		OnPause: func(snap agent.Snapshot) {
			if err := runtime.saveSnapshot(context.Background(), snap); err != nil {
				runtime.fail(fmt.Errorf("persist approval checkpoint: %w", err))
				runtime.cancelCurrentTurn()
			}
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
	cancelTurn := r.turnCancel
	r.mu.Unlock()
	approvalRegistration := r.transport != nil && r.transport.hasApprovalRegistration()
	snapshot := agent.Snapshot{}
	if r.agent != nil {
		snapshot = r.agent.Snapshot()
	}
	if active && snapshot.State == agent.StatePaused && r.transport != nil {
		if len(snapshot.PendingActions) > 0 {
			pending := make([]zotigotransport.PendingToolCall, 0, len(snapshot.PendingActions))
			for _, action := range snapshot.PendingActions {
				pending = append(pending, zotigotransport.PendingToolCall{
					ID:          action.ToolCallID,
					Name:        action.Name,
					Arguments:   action.Arguments,
					Description: action.Decision.Reason,
				})
			}
			_, _, _ = r.transport.ensureApproval(context.Background(), pending)
			approvalRegistration = r.transport.hasApprovalRegistration()
		}
	}
	preserveApprovalTurn := approvalRegistration && snapshot.State == agent.StatePaused
	interruptedThroughTransport := false
	if active && !preserveApprovalTurn && r.transport != nil && r.display != nil {
		turnID := r.display.CurrentTurnID()
		if turnID != "" {
			interrupted, _ := r.transport.interruptTurn(context.Background(), turnID, controlChannelClosedReason, cancelTurn)
			interruptedThroughTransport = interrupted
			preserveApprovalTurn = !interrupted
		}
	}
	if cancelTurn != nil && !interruptedThroughTransport {
		cancelTurn()
	}
	if active && !preserveApprovalTurn && !interruptedThroughTransport && r.display != nil {
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

func (r *workerRuntime) resolveApproval(ctx context.Context, decision workerApprovalDecision) (workerApprovalResult, *workerApprovalResolution) {
	result := workerApprovalResult{RequestID: decision.RequestID}
	if r.transport == nil {
		result.Error = "approval transport is not configured"
		return result, nil
	}
	resolution, err := r.transport.resolveApproval(ctx, decision.ApprovalID, decision.Decisions)
	if err != nil {
		result.Error = err.Error()
		return result, nil
	}
	result.Approval = &resolution.approval
	return result, &resolution
}

func (r *workerRuntime) releaseApproval(ctx context.Context, resolution workerApprovalResolution) error {
	if r.transport == nil {
		return fmt.Errorf("approval transport is not configured")
	}
	if err := r.beginRuntimeWAL(ctx); err != nil {
		return err
	}
	return r.transport.releaseApproval(ctx, resolution)
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
		return nil, r.queueTurnUserInput(ctx, command)
	case sessionCommandProfile:
		return r.switchProfile(ctx, command.ID, command.Profile)
	case sessionCommandApprovalPolicy:
		return nil, r.setApprovalPolicy(ctx, command.ID, command.ApprovalPolicy)
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
	if command.ApprovalPolicy != nil {
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
	case sessionCommandApprovalPolicy:
		if command.ApprovalPolicy == nil || payloads != 1 {
			return fmt.Errorf("invalid approval policy command payload")
		}
		if _, err := normalizeSessionApprovalPolicy(command.ApprovalPolicy.Policy, false); err != nil {
			return fmt.Errorf("invalid approval policy command payload: %w", err)
		}
	default:
		return nil
	}
	return nil
}

func (r *workerRuntime) setApprovalPolicy(ctx context.Context, commandID string, command *approvalPolicyCommandPayload) error {
	target, err := normalizeSessionApprovalPolicy(command.Policy, false)
	if err != nil {
		return err
	}
	r.storeMu.Lock()
	defer r.storeMu.Unlock()
	sess, err := ensureWorkerSession(ctx, r.store, r.sessionID, r.workDir)
	if err != nil {
		return err
	}
	from := sess.ApprovalPolicy
	sess.ApprovalPolicy = target
	sess.UpdatedAt = time.Now().UTC()
	loweringPermissions := from == agent.ApprovalPolicyBypass && target == agent.ApprovalPolicyAuto
	if loweringPermissions {
		// Stop bypassing approval before publishing the safer persisted value.
		// If persistence fails, keeping the Agent on Auto is the safe outcome.
		r.agent.SetApprovalPolicy(target)
	}
	if err := persistSessionApprovalPolicy(ctx, r.store, sess); err != nil {
		return fmt.Errorf("persist approval policy: %w", err)
	}
	if !loweringPermissions {
		r.agent.SetApprovalPolicy(target)
	}
	if err := r.display.ApprovalPolicyChanged(ctx, commandID, string(from), string(target)); err != nil {
		return fmt.Errorf("record approval policy change: %w", err)
	}
	return nil
}

func (r *workerRuntime) switchProfile(ctx context.Context, commandID string, command *profileCommandPayload) (<-chan error, error) {
	completion := make(chan error, 1)
	predecessor, ordered := r.nextProfileOrder()
	target := strings.TrimSpace(command.Name)
	epoch, err := r.nextProfileEpoch()
	if err != nil {
		close(ordered)
		close(completion)
		return completion, err
	}
	r.agent.SupersedePendingRuntimeProfile()
	appConfig, err := config.NewManager().LoadForDir(r.workDir)
	if err != nil {
		r.completeProfileFailure(predecessor, ordered, completion, commandID, target, fmt.Errorf("load profiles: %w", err))
		return completion, nil
	}
	_, profile, err := appConfig.ResolveProfile(target)
	if err != nil {
		r.completeProfileFailure(predecessor, ordered, completion, commandID, target, err)
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
		r.completeProfileFailure(predecessor, ordered, completion, commandID, target, err)
		return completion, nil
	}
	runtimeProfile.BeforeApply = func() error {
		<-predecessor
		if err := r.currentFatalError(); err != nil {
			return err
		}
		from := r.agent.ActiveProfileName()
		commitCtx, cancel := context.WithTimeout(context.Background(), workerHTTPTimeout)
		defer cancel()
		return r.commitLatestProfileSwitch(commitCtx, epoch, commandID, from, target)
	}
	result := r.agent.QueueRuntimeProfile(runtimeProfile)
	go r.finishProfileSwitch(predecessor, ordered, commandID, target, result, completion)
	return completion, nil
}

func (r *workerRuntime) nextProfileOrder() (<-chan struct{}, chan struct{}) {
	r.profileOrderMu.Lock()
	defer r.profileOrderMu.Unlock()
	predecessor := r.profileOrderTail
	if predecessor == nil {
		ready := make(chan struct{})
		close(ready)
		predecessor = ready
	}
	ordered := make(chan struct{})
	r.profileOrderTail = ordered
	return predecessor, ordered
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

func (r *workerRuntime) finishProfileSwitch(predecessor <-chan struct{}, ordered chan struct{}, commandID string, target string, result <-chan error, completion chan<- error) {
	defer close(completion)
	err := <-result
	<-predecessor
	defer close(ordered)
	if fatalErr := r.currentFatalError(); fatalErr != nil {
		completion <- fatalErr
		return
	}
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
			err = &profileCompletionUncertainError{cause: err}
			r.fail(err)
		}
		completion <- err
		return
	}
	if err != nil {
		err = r.recordProfileFailure(ctx, commandID, target, err)
		if err != nil {
			err = &profileCompletionUncertainError{cause: err}
			r.fail(err)
		}
		completion <- err
		return
	}
	completion <- nil
}

func (r *workerRuntime) completeProfileFailure(predecessor <-chan struct{}, ordered chan struct{}, completion chan<- error, commandID string, target string, cause error) {
	defer close(completion)
	<-predecessor
	defer close(ordered)
	if fatalErr := r.currentFatalError(); fatalErr != nil {
		completion <- fatalErr
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := r.recordProfileFailure(ctx, commandID, target, cause)
	if err != nil {
		err = &profileCompletionUncertainError{cause: err}
		r.fail(err)
	}
	completion <- err
}

type profileStateUncertainError struct {
	cause error
}

func (e *profileStateUncertainError) Error() string {
	return "profile state is uncertain: " + e.cause.Error()
}
func (e *profileStateUncertainError) Unwrap() error { return e.cause }

type profileCompletionUncertainError struct{ cause error }

func (e *profileCompletionUncertainError) Error() string {
	return "profile command completion is uncertain: " + e.cause.Error()
}
func (e *profileCompletionUncertainError) Unwrap() error { return e.cause }

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
	_, err := r.transport.interruptTurn(ctx, currentTurnID, command.Reason, r.cancelCurrentTurn)
	return err
}

func (r *workerRuntime) queueTurnUserInput(ctx context.Context, command commandResponse) error {
	payload := command.Steering
	msg, err := userMessageFromCommand(payload.Text, payload.Images, "steering")
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
	if payload.TurnID != "" && currentTurnID != "" && payload.TurnID != currentTurnID {
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
	if currentTurnID == "" || (payload.TurnID != "" && payload.TurnID != currentTurnID) {
		return nil
	}
	msg.ID = command.ID
	r.display.QueueSteering(command)
	if err := r.agent.QueueTurnUserMessage(msg); err != nil {
		r.display.DiscardSteering(command.ID)
		if isStaleTurnUserInputError(err) {
			return nil
		}
		return err
	}
	snapshot := r.agent.Snapshot()
	if snapshot.State == agent.StatePaused && len(snapshot.PendingActions) > 0 {
		if err := r.beginRuntimeWAL(ctx); err != nil {
			return err
		}
		if err := r.transport.interruptApprovalForSteering(ctx, currentTurnID); err != nil {
			_ = r.saveSnapshot(context.Background(), snapshot)
			return err
		}
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

	turnID, err := r.display.StartTurn(ctx)
	if err != nil {
		r.finishTurn()
		return err
	}
	if err := r.beginRuntimeWAL(ctx); err != nil {
		r.finishTurn()
		return fmt.Errorf("begin runtime WAL: %w", err)
	}
	go func() {
		err := r.runner.RunFullTurnStarted(turnCtx, msg, r.markTurnReady)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, zotigotransport.ErrTransportClosed) {
			_ = r.display.Fail(context.Background(), err)
		}
		_ = r.agent.WaitForRuntimeIdle(context.Background())
		snapshot := r.snapshotAfterTurn(turnID)
		if err := r.saveSnapshot(context.Background(), snapshot); err != nil {
			r.fail(fmt.Errorf("persist terminal turn state: %w", err))
		}
		r.finishTurn()
	}()
	return nil
}

func (r *workerRuntime) snapshotAfterTurn(turnID string) agent.Snapshot {
	snapshot := r.agent.Snapshot()
	if r.transport == nil {
		return snapshot
	}
	if reason, interrupted := r.transport.interruptedTurn(turnID); interrupted && snapshot.State == agent.StatePaused {
		snapshot = agent.InterruptPendingSnapshot(snapshot, reason)
		r.agent.Restore(snapshot)
	}
	return snapshot
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
	if r.runtimeWAL != nil {
		return r.runtimeWAL.Commit(ctx, sess, r.store.Put)
	}
	sess.SnapshotVersion++
	return r.store.Put(ctx, sess)
}

func (r *workerRuntime) beginRuntimeWAL(ctx context.Context) error {
	if r.runtimeWAL == nil {
		return nil
	}
	r.storeMu.Lock()
	defer r.storeMu.Unlock()
	sess, err := ensureWorkerSession(ctx, r.store, r.sessionID, "")
	if err != nil {
		return err
	}
	return r.runtimeWAL.Begin(ctx, sess, r.agent.Snapshot(), r.display.CurrentTurnID())
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
	body := finishSessionRequest{Generation: generation}
	if err != nil && !isExpectedWorkerClose(err) {
		body.Error = err.Error()
	}
	return postWorkerJSON(ctx, client, daemonURL, "/internal/sessions/"+url.PathEscape(sessionID)+"/worker/finish", body)
}

func reportWorkerReady(ctx context.Context, client *http.Client, daemonURL string, sessionID string, generation string) error {
	return postWorkerJSON(ctx, client, daemonURL, "/internal/sessions/"+url.PathEscape(sessionID)+"/worker/attach", workerReadyRequest{Generation: generation})
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

func replayWorkerCommands(ctx context.Context, client *http.Client, daemonURL string, sessionID string, runtime *workerRuntime, cursor workerCommandCursor) (workerCommandCursor, error) {
	type pendingProfile struct {
		sequence   uint64
		completion <-chan error
	}
	pendingProfiles := make([]pendingProfile, 0)
	completedProfiles, completedApprovalPolicies, err := completedWorkerCommandIDs(ctx, runtime)
	if err != nil {
		return cursor, err
	}
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
			if command.Type == sessionCommandProfile && completedProfiles[command.ID] {
				if err := flushProfiles(); err != nil {
					return cursor, err
				}
				cursor.Sequence = command.Sequence
				continue
			}
			if command.Type == sessionCommandApprovalPolicy && completedApprovalPolicies[command.ID] {
				if err := flushProfiles(); err != nil {
					return cursor, err
				}
				cursor.Sequence = command.Sequence
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

func completedWorkerCommandIDs(ctx context.Context, runtime *workerRuntime) (map[string]bool, map[string]bool, error) {
	completedProfiles := make(map[string]bool)
	completedApprovalPolicies := make(map[string]bool)
	pending := make(map[string]bool)
	if runtime == nil || runtime.display == nil || runtime.display.items == nil {
		return completedProfiles, completedApprovalPolicies, nil
	}
	items, _, err := runtime.display.items.LoadItems(ctx, runtime.sessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("load worker command completions: %w", err)
	}
	for _, item := range items {
		if item.Type == zotigosession.DisplayItemApprovalPolicyChanged && item.ApprovalPolicy != nil && item.ApprovalPolicy.CommandID != "" {
			completedApprovalPolicies[item.ApprovalPolicy.CommandID] = true
		}
		if item.Command != nil && item.Command.Type == sessionCommandProfile {
			pending[item.ID] = true
		}
		if item.Type != zotigosession.DisplayItemProfileChanged && item.Type != zotigosession.DisplayItemProfileFailed {
			continue
		}
		if item.Profile != nil && item.Profile.CommandID != "" {
			completedProfiles[item.Profile.CommandID] = true
			delete(pending, item.Profile.CommandID)
		} else if item.Type == zotigosession.DisplayItemProfileChanged && item.Profile != nil {
			for commandID := range pending {
				completedProfiles[commandID] = true
			}
			clear(pending)
		}
	}
	return completedProfiles, completedApprovalPolicies, nil
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
		if item.Command != nil && item.Command.Type != "" && item.Command.Type != sessionCommandSteering {
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
	pendingApprovalPolicies := make(map[string]uint64)

	for _, item := range items {
		if item.Command != nil && item.Command.Type != "" && item.Command.Type != sessionCommandSteering {
			commandSeqs = append(commandSeqs, item.Sequence)
			switch item.Command.Type {
			case sessionCommandMessage:
				pendingMessages = append(pendingMessages, item.Sequence)
			case sessionCommandPause:
				if item.Command.TurnID != "" {
					pendingByTurn[item.Command.TurnID] = append(pendingByTurn[item.Command.TurnID], item.Sequence)
				}
			case sessionCommandProfile:
				pendingProfiles[item.ID] = item.Sequence
			case sessionCommandApprovalPolicy:
				pendingApprovalPolicies[item.ID] = item.Sequence
			default:
				safe[item.Sequence] = true
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
				} else if item.Type == zotigosession.DisplayItemProfileChanged && item.Profile.CommandID == "" {
					for commandID, pendingSeq := range pendingProfiles {
						safe[pendingSeq] = true
						delete(pendingProfiles, commandID)
					}
				}
			}
		case zotigosession.DisplayItemApprovalPolicyChanged:
			if item.ApprovalPolicy != nil {
				if seq, ok := pendingApprovalPolicies[item.ApprovalPolicy.CommandID]; ok {
					safe[seq] = true
					delete(pendingApprovalPolicies, item.ApprovalPolicy.CommandID)
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
	var completionUncertain *profileCompletionUncertainError
	return errors.As(err, &uncertain) || errors.As(err, &completionUncertain) ||
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

type workerClientWriter struct {
	conn                  *websocket.Conn
	pingEvery             time.Duration
	displayControlTimeout time.Duration
	sendCh                chan workerMessage
	done                  chan struct{}
	closeOnce             sync.Once
}

type workerDisplayBarrierClient struct {
	writer  *workerClientWriter
	timeout time.Duration
	mu      sync.Mutex
	waiters map[string]chan struct{}
}

func newWorkerDisplayBarrierClient(writer *workerClientWriter) *workerDisplayBarrierClient {
	return &workerDisplayBarrierClient{
		writer:  writer,
		timeout: workerHTTPTimeout,
		waiters: make(map[string]chan struct{}),
	}
}

func (b *workerDisplayBarrierClient) Wait(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	barrier := workerDisplayBarrier{ID: newZotigodID("display_barrier")}
	acknowledged := make(chan struct{})
	b.mu.Lock()
	b.waiters[barrier.ID] = acknowledged
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.waiters, barrier.ID)
		b.mu.Unlock()
	}()
	if err := b.writer.SendDisplayBarrier(waitCtx, barrier); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return errors.New("display barrier acknowledgement timed out")
		}
		return err
	}
	select {
	case <-waitCtx.Done():
		if ctx.Err() == nil {
			return errors.New("display barrier acknowledgement timed out")
		}
		return ctx.Err()
	case <-b.writer.done:
		return errors.New("worker connection is closed")
	case <-acknowledged:
		return nil
	}
}

func (b *workerDisplayBarrierClient) Acknowledge(id string) {
	b.mu.Lock()
	acknowledged := b.waiters[id]
	if acknowledged != nil {
		delete(b.waiters, id)
		close(acknowledged)
	}
	b.mu.Unlock()
}

func newWorkerClientWriter(conn *websocket.Conn, pingInterval time.Duration, pongWait time.Duration) *workerClientWriter {
	if pongWait > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(pongWait))
		})
	}
	writer := &workerClientWriter{
		conn:                  conn,
		pingEvery:             pingInterval,
		displayControlTimeout: workerHTTPTimeout,
		sendCh:                make(chan workerMessage, workerDeltaBufferSize),
		done:                  make(chan struct{}),
	}
	go writer.run()
	return writer
}

func (w *workerClientWriter) SendDelta(delta displayDeltaEvent) {
	msg := workerMessage{Type: workerMessageDelta, Delta: &delta}
	select {
	case <-w.done:
		return
	default:
	}
	select {
	case w.sendCh <- msg:
	case <-w.done:
	default:
		// Deltas are previews. Never stall model generation; the completed
		// durable item replaces any partial preview that was dropped here.
	}
}

func (w *workerClientWriter) SendDisplayWake() {
	msg := workerMessage{Type: workerMessageDisplayWake}
	select {
	case <-w.done:
		return
	default:
	}
	select {
	case w.sendCh <- msg:
	case <-w.done:
	default:
		// Periodic SSE catch-up makes ordinary display wakes optional.
	}
}

func (w *workerClientWriter) SendDisplayWakeReliable(ctx context.Context) error {
	timeout := w.displayControlTimeout
	if timeout <= 0 {
		timeout = workerHTTPTimeout
	}
	wakeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	msg := workerMessage{Type: workerMessageDisplayWake}
	select {
	case <-wakeCtx.Done():
		return wakeCtx.Err()
	case <-w.done:
		return errors.New("worker connection is closed")
	case w.sendCh <- msg:
		return nil
	}
}

func (w *workerClientWriter) SendDisplayBarrier(ctx context.Context, barrier workerDisplayBarrier) error {
	msg := workerMessage{Type: workerMessageDisplayBarrier, DisplayBarrier: &barrier}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		return errors.New("worker connection is closed")
	case w.sendCh <- msg:
		return nil
	}
}

func (w *workerClientWriter) SendApprovalResult(ctx context.Context, result workerApprovalResult) bool {
	msg := workerMessage{Type: workerMessageApprovalResult, ApprovalResult: &result}
	select {
	case <-ctx.Done():
		return false
	case <-w.done:
		return false
	case w.sendCh <- msg:
		return true
	}
}

func (w *workerClientWriter) SendApprovalRequest(ctx context.Context, approval approvalRequestResponse) {
	msg := workerMessage{Type: workerMessageApprovalRequest, ApprovalRequest: &approval}
	select {
	case <-ctx.Done():
		return
	case <-w.done:
		return
	case w.sendCh <- msg:
		return
	}
}

func (w *workerClientWriter) Close() {
	w.closeOnce.Do(func() { close(w.done) })
}

func (w *workerClientWriter) run() {
	defer w.Close()
	var ticker *time.Ticker
	var ticks <-chan time.Time
	if w.pingEvery > 0 {
		ticker = time.NewTicker(w.pingEvery)
		ticks = ticker.C
		defer ticker.Stop()
	}
	for {
		select {
		case <-w.done:
			return
		case msg := <-w.sendCh:
			if err := w.writeMessage(websocket.TextMessage, msg); err != nil {
				_ = w.conn.Close()
				return
			}
		case <-ticks:
			if err := w.writeMessage(websocket.PingMessage, nil); err != nil {
				_ = w.conn.Close()
				return
			}
		}
	}
}

func (w *workerClientWriter) writeMessage(messageType int, value any) error {
	var data []byte
	var err error
	if value != nil {
		data, err = sonic.Marshal(value)
		if err != nil {
			return err
		}
	}
	if err := w.conn.SetWriteDeadline(time.Now().Add(workerWriteWait)); err != nil {
		return err
	}
	return w.conn.WriteMessage(messageType, data)
}

func startWorkerClientKeepalive(conn *websocket.Conn, pingInterval time.Duration, pongWait time.Duration) func() {
	writer := newWorkerClientWriter(conn, pingInterval, pongWait)
	return writer.Close
}

func acquireWorkerSessionLock(ctx context.Context, store zotigosession.Store, sessionID string) (func() error, error) {
	if err := store.Lock(ctx, sessionID); err != nil {
		return nil, fmt.Errorf("lock session %s: %w", sessionID, err)
	}
	return func() error {
		return store.Unlock(context.Background(), sessionID)
	}, nil
}
