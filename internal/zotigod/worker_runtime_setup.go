package zotigod

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/lsp"
	"github.com/jayyao97/zotigo/core/observability"
	_ "github.com/jayyao97/zotigo/core/providers/anthropic"
	_ "github.com/jayyao97/zotigo/core/providers/deepseek"
	_ "github.com/jayyao97/zotigo/core/providers/gemini"
	_ "github.com/jayyao97/zotigo/core/providers/openai"
	"github.com/jayyao97/zotigo/core/runner"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/skills"
	"github.com/jayyao97/zotigo/core/tools/builtin"
	zotigotransport "github.com/jayyao97/zotigo/core/transport"
	"github.com/jayyao97/zotigo/internal/channels"
	"github.com/jayyao97/zotigo/internal/hooks"
	"github.com/jayyao97/zotigo/internal/sessionadapter"
	"github.com/jayyao97/zotigo/internal/wiring"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

type workerRuntimeConfig struct {
	SessionID              string
	WorkspaceID            string
	Store                  zotigosession.Store
	SendDelta              func(wire.DisplayDelta)
	NotifyDisplayItem      func(zotigosession.DisplayItem)
	NotifyDisplay          func()
	SyncDisplay            func(context.Context) error
	DisplayBarrier         func(context.Context) error
	NotifyApproval         func(context.Context, wire.ApprovalRequest)
	NotifyApprovalResolved func(context.Context, wire.ApprovalRequest)
	NotifyIdle             func(context.Context, wire.Idle) error
	NotifyWorking          func(context.Context) error
	HookDispatcher         *hooks.Dispatcher
	RuntimeTools           *workerbase.RuntimeToolClient
}

type workerRuntime struct {
	sessionID        string
	workDir          string
	skills           *skills.SkillManager
	store            zotigosession.Store
	agent            *agent.Agent
	runner           *runner.Runner
	transport        *workerRuntimeTransport
	display          *sessionlog.Log
	observer         observability.Observer
	runtimeWAL       *workerRuntimeWAL
	cleanup          func()
	storeMu          sync.Mutex
	profileMu        sync.Mutex
	profileEpoch     uint64
	hookModel        string
	promptConfig     zotigosession.PromptConfig
	profileOrderMu   sync.Mutex
	profileOrderTail <-chan struct{}
	fatalCh          chan error
	fatalMu          sync.Mutex
	fatalErr         error
	notifyIdle       func(context.Context, wire.Idle) error
	notifyWorking    func(context.Context) error
	hooks            workerbase.HookDispatcher
	channelTools     *workerbase.ChannelRuntimeToolState
	runtimeTools     *workerbase.RuntimeToolClient

	mu                  sync.Mutex
	turnCancel          context.CancelFunc
	turnActive          bool
	turnCompacting      bool
	turnStopping        bool
	turnReady           chan struct{}
	turnDone            chan struct{}
	readyDone           bool
	doneDone            bool
	turnCommandSequence uint64
}

func newWorkerRuntime(ctx context.Context, cfg workerRuntimeConfig) (*workerRuntime, error) {
	stepStarted := time.Now()
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolve workdir: %w", err)
	}
	sess, err := workerbase.EnsureSession(ctx, cfg.Store, cfg.SessionID, cwd)
	if err != nil {
		return nil, err
	}
	workerbase.LogRuntimeStep(cfg.SessionID, "ensure_session", stepStarted)
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
	approvalPolicy, err := workerbase.NormalizeSessionApprovalPolicy(sess.ApprovalPolicy, true)
	if err != nil {
		return nil, fmt.Errorf("load approval policy: %w", err)
	}
	workerbase.LogRuntimeStep(cfg.SessionID, "config", stepStarted)

	stepStarted = time.Now()
	localExec, err := executor.NewLocalExecutor(cwd)
	if err != nil {
		return nil, fmt.Errorf("create executor: %w", err)
	}
	workerbase.LogRuntimeStep(cfg.SessionID, "executor", stepStarted)

	stepStarted = time.Now()
	skills, err := wiring.NewSkillManager(cwd)
	if err != nil {
		_ = localExec.Close()
		return nil, fmt.Errorf("load skills: %w", err)
	}
	workerbase.LogRuntimeStep(cfg.SessionID, "skills", stepStarted)
	transcriptDir := workerbase.CompactedTranscriptDir()
	observer := wiring.NewObserver(appConfig.Observability, cfg.SessionID, map[string]any{
		"zotigo_session": cfg.SessionID,
		"process_start":  time.Now().UTC().Format(time.RFC3339),
		"worker":         true,
	})
	display := sessionlog.NewLog(cfg.SessionID, sessionlog.StoredItemSource{Store: cfg.Store})
	display.Delta = cfg.SendDelta
	display.Persisted = cfg.NotifyDisplayItem
	display.Wake = func(context.Context) {
		if cfg.NotifyDisplay != nil {
			cfg.NotifyDisplay()
		}
	}
	display.Barrier = cfg.DisplayBarrier
	display.WakeSync = cfg.SyncDisplay
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
	_, err = display.ResolvePendingApprovalsForOpenTurn(ctx, workerbase.ApprovalWorkerRestartedReason)
	if err != nil {
		_ = observer.Close(context.Background())
		_ = localExec.Close()
		return nil, fmt.Errorf("recover pending approval: %w", err)
	}
	if sess.AgentSnapshot.State == agent.StatePaused {
		sess.AgentSnapshot = agent.InterruptPendingSnapshot(sess.AgentSnapshot, workerbase.ApprovalWorkerRestartedReason)
		sessionadapter.ApplySnapshot(sess, sess.AgentSnapshot, sessionadapter.LastUserPrompt(sess.AgentSnapshot.History))
		if err := cfg.Store.Put(ctx, sess); err != nil {
			_ = observer.Close(context.Background())
			_ = localExec.Close()
			return nil, fmt.Errorf("persist recovered approval snapshot: %w", err)
		}
	}

	hookDispatcher := cfg.HookDispatcher
	ownsHookDispatcher := false
	if hookDispatcher == nil {
		hookLogger := log.New(log.Writer(), "[zotigo-hooks] ", log.LstdFlags)
		hookDispatcher, err = hooks.LoadDefault(hookLogger)
		if err != nil {
			hookLogger.Printf("hook_config outcome=failed error=%q", err)
		} else {
			ownsHookDispatcher = true
		}
	}

	stepStarted = time.Now()
	ag, err := wiring.NewAgent(wiring.AgentConfig{
		Config:      appConfig,
		ProfileName: profileName,
		Profile:     profile,
		Executor:    localExec,
		PromptBuilder: wiring.NewSystemPromptBuilder(wiring.PromptConfig{
			SessionID:         cfg.SessionID,
			WorkspaceID:       cfg.WorkspaceID,
			WorkDir:           cwd,
			SkillManager:      skills,
			AgentInstructions: sess.PromptConfig.AgentInstructions,
		}),
		UserContextBuilder: wiring.NewUserContextBuilder(wiring.PromptConfig{
			WorkDir:                    cwd,
			IncludeProjectInstructions: true,
		}),
		ApprovalPolicy:       approvalPolicy,
		TranscriptDir:        transcriptDir,
		SafeDirs:             skills.Dirs(),
		Observer:             observer,
		ConfigureClassifier:  true,
		ApprovalInstructions: sess.PromptConfig.ApprovalInstructions,
		ReviewAllTools:       sess.PromptConfig.ReviewAllTools,
		Middleware: []agent.Middleware{
			hooks.ToolMiddleware(hookDispatcher, hooks.ToolContext{
				SessionID: cfg.SessionID,
				Agent:     "zotigo",
				CWD:       cwd,
				TurnID:    display.CurrentTurnID,
			}),
		},
	})
	if err != nil {
		if ownsHookDispatcher {
			workerbase.CloseHookDispatcher(hookDispatcher)
		}
		_ = observer.Close(context.Background())
		_ = localExec.Close()
		return nil, fmt.Errorf("create agent: %w", err)
	}
	workerbase.LogRuntimeStep(cfg.SessionID, "agent", stepStarted)
	durabilityRecorder := &workerDurabilityRecorder{display: display, wal: runtimeWAL}
	if runtimeWAL != nil {
		agent.WithDurabilityRecorder(durabilityRecorder)(ag)
	} else {
		agent.WithToolExecutionRecorder(durabilityRecorder)(ag)
	}
	ag.Restore(sess.AgentSnapshot)
	transport := newWorkerRuntimeTransport(cfg.SessionID, display, cfg.NotifyApproval)
	transport.notifyApprovalResolved = cfg.NotifyApprovalResolved

	stepStarted = time.Now()
	lspManager := lsp.NewManager(cwd)
	if err := wiring.RegisterDefaultTools(ag, wiring.ToolSetConfig{
		Config:                 appConfig,
		Profile:                profile,
		ShellPolicy:            builtin.DefaultShellPolicy(),
		LSPManager:             lspManager,
		Spawn:                  true,
		SpawnApprovalRequester: transport,
	}); err != nil {
		if ownsHookDispatcher {
			workerbase.CloseHookDispatcher(hookDispatcher)
		}
		_ = observer.Close(context.Background())
		_ = lspManager.StopAll()
		_ = localExec.Close()
		return nil, fmt.Errorf("register tools: %w", err)
	}
	var channelTools *workerbase.ChannelRuntimeToolState
	if sess.Capabilities.SessionToolsVersion >= 1 && cfg.RuntimeTools != nil {
		for _, spec := range workerbase.SessionToolSpecs() {
			ag.RegisterTool(&workerbase.SessionRuntimeTool{Spec: spec, Client: cfg.RuntimeTools, TurnID: display.CurrentTurnID})
		}
	}
	if sess.Capabilities.ChannelToolsVersion >= channels.RuntimeToolsVersion && cfg.RuntimeTools != nil {
		channelTools = &workerbase.ChannelRuntimeToolState{Client: cfg.RuntimeTools}
		ag.RegisterTool(&workerbase.ChannelReadMessagesTool{State: channelTools})
	}
	workerbase.LogRuntimeStep(cfg.SessionID, "tools", stepStarted)

	if err := display.InterruptOpenTurn(ctx, sessionlog.WorkerRestartedReason); err != nil {
		if ownsHookDispatcher {
			workerbase.CloseHookDispatcher(hookDispatcher)
		}
		_ = observer.Close(context.Background())
		_ = lspManager.StopAll()
		_ = localExec.Close()
		return nil, fmt.Errorf("repair open display turn: %w", err)
	}
	runtime := &workerRuntime{
		sessionID:     cfg.SessionID,
		workDir:       cwd,
		skills:        skills,
		store:         cfg.Store,
		agent:         ag,
		transport:     transport,
		display:       display,
		observer:      observer,
		runtimeWAL:    runtimeWAL,
		fatalCh:       make(chan error, 1),
		notifyIdle:    cfg.NotifyIdle,
		notifyWorking: cfg.NotifyWorking,
		hooks:         hookDispatcher,
		channelTools:  channelTools,
		runtimeTools:  cfg.RuntimeTools,
		hookModel:     profile.Model,
		promptConfig:  sess.PromptConfig,
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
		if ownsHookDispatcher {
			workerbase.CloseHookDispatcher(hookDispatcher)
		}
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
			interrupted, _ := r.transport.interruptTurn(context.Background(), turnID, workerbase.ControlChannelClosedReason, cancelTurn)
			interruptedThroughTransport = interrupted
			preserveApprovalTurn = !interrupted
		}
	}
	if cancelTurn != nil && !interruptedThroughTransport {
		cancelTurn()
	}
	if active && !preserveApprovalTurn && !interruptedThroughTransport && r.display != nil {
		_ = r.display.Interrupt(context.Background(), workerbase.ControlChannelClosedReason)
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
