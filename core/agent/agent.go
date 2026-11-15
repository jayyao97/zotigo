package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jayyao97/zotigo/core/agent/loopguard"
	"github.com/jayyao97/zotigo/core/agent/prompt"
	"github.com/jayyao97/zotigo/core/compaction"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/observability"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

var ErrNotRunning = errors.New("agent is not running")

// ErrCompacting means input was not admitted while a summary was in progress.
var ErrCompacting = errors.New("context compaction is in progress")

// Agent orchestrates provider, tools, and conversation history.
type Agent struct {
	cfg                 config.ProfileConfig
	profileName         string
	provider            providers.Provider
	executor            executor.Executor
	tools               map[string]tools.Tool
	policy              ApprovalPolicy
	requestedPolicy     ApprovalPolicy
	profileForcesManual bool
	promptBuilder       *prompt.SystemPromptBuilder
	userContext         *prompt.UserContextBuilder

	reminderBuilder *prompt.ReminderBuilder

	// Services
	loopDetector                *loopguard.LoopDetector
	compressor                  *compaction.Compressor
	classifier                  SafetyClassifier
	classifierProfileName       string
	classifierProfile           config.ProfileConfig
	classifierUnavailableReason string
	reviewAllTools              bool
	extraSafeDirs               []string
	turnSafety                  TurnSafetyState

	observer observability.Observer // Noop by default; never nil

	// activeTraceCtx is a values-only snapshot (context.WithoutCancel)
	// of the in-flight turn's ctx, kept while the agent is paused for
	// approval. On resume we overlay the trace identity onto the
	// caller's live ctx via observer.ResumeTrace — the caller's ctx
	// keeps providing cancellation/deadline so a fresh approval ctx
	// can cancel the resumed work without being shadowed by the
	// original Run's deadline. Cleared at end-of-turn.
	activeTraceCtx context.Context

	middlewares           []Middleware
	toolExecutionRecorder ToolExecutionRecorder

	mu              sync.RWMutex
	state           State
	conversation    conversationState
	cumulativeUsage protocol.Usage
	pendingTurnUser []protocol.Message
	activeToolRun   *activeToolRun
	pendingActions  []*PendingAction
	deferredActions []*PendingAction
	turns           []TurnAudit
	pendingProfile  *runtimeProfileRequest
	runtimeActivity int
	profileActivity int
	runActive       bool
	compacting      atomic.Bool
	runtimeIdle     chan struct{}
	profileApplyMu  sync.Mutex
}

type activeToolRun struct {
	cancel      context.CancelFunc
	interrupted bool
	completed   map[string]struct{}
}

type userInputBoundary uint8

const (
	userInputContinuesTurn userInputBoundary = iota
	userInputStartsTurn
)

// AgentOption configures an Agent during construction.
type AgentOption func(*Agent)

// WithSystemPromptBuilder sets the system prompt builder.
func WithSystemPromptBuilder(pb *prompt.SystemPromptBuilder) AgentOption {
	return func(a *Agent) { a.promptBuilder = pb }
}

// WithUserContextBuilder sets the builder used to persist contextual user
// messages before real user turns.
func WithUserContextBuilder(uc *prompt.UserContextBuilder) AgentOption {
	return func(a *Agent) { a.userContext = uc }
}

// WithApprovalPolicy sets the tool approval policy.
func WithApprovalPolicy(p ApprovalPolicy) AgentOption {
	return func(a *Agent) {
		a.requestedPolicy = p
		a.refreshApprovalPolicy()
	}
}

// WithTools registers one or more tools.
func WithTools(ts ...tools.Tool) AgentOption {
	return func(a *Agent) {
		for _, t := range ts {
			a.tools[t.Name()] = t
		}
	}
}

// AddSafeDirs registers additional directories as safe for auto-approved read access.
// Components should call this when they produce files the agent may need to read.
func (a *Agent) AddSafeDirs(dirs ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.extraSafeDirs = append(a.extraSafeDirs, dirs...)
}

// WithTranscriptDir sets the directory for saving compressed message transcripts.
// When set, the compressor saves the original messages to a JSONL file before
// discarding them, and injects the file path into the summary for later retrieval.
func WithTranscriptDir(dir string) AgentOption {
	return func(a *Agent) {
		a.extraSafeDirs = append(a.extraSafeDirs, dir)
		if a.compressor != nil {
			a.compressor.SetTranscriptDir(dir)
		}
	}
}

// WithReminder registers a ReminderProvider that injects text into tool results.
func WithReminder(provider prompt.ReminderProvider) AgentOption {
	return func(a *Agent) {
		if a.reminderBuilder == nil {
			a.reminderBuilder = prompt.NewReminderBuilder()
		}
		a.reminderBuilder.Providers = append(a.reminderBuilder.Providers, provider)
	}
}

// WithObserver wires an observability backend (typically Langfuse).
// Passing nil resets to Noop. The agent always holds a non-nil
// Observer so hot-path call sites can dispatch unconditionally.
func WithObserver(obs observability.Observer) AgentOption {
	return func(a *Agent) {
		if obs == nil {
			a.observer = observability.Noop{}
			return
		}
		a.observer = obs
	}
}

// WithProfileName records the configured profile name for runtime reporting and switching.
func WithProfileName(name string) AgentOption {
	return func(a *Agent) { a.profileName = name }
}

// WithReviewAllTools routes otherwise auto-executable Safe calls through the
// safety classifier. Tool-level blocks and mandatory approvals still win.
func WithReviewAllTools(enabled bool) AgentOption {
	return func(a *Agent) { a.reviewAllTools = enabled }
}

// New creates a new Agent with the given configuration and executor.
// The executor parameter provides the environment for tool execution (local, E2B, Docker, etc.)
func New(cfg config.ProfileConfig, exec executor.Executor, opts ...AgentOption) (*Agent, error) {
	p, err := providers.NewProvider(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create provider: %w", err)
	}

	// Profile config wins for the compressor's context window; the
	// default already falls back to config.DefaultContextWindow,
	// so omitting `context_window` doesn't desync this from the TUI
	// status display.
	compressorCfg := compaction.DefaultCompressorConfig()
	if cfg.ContextWindow > 0 {
		compressorCfg.ContextWindowSize = cfg.ContextWindow
	}

	runtimeIdle := make(chan struct{})
	close(runtimeIdle)
	a := &Agent{
		cfg:             cfg,
		provider:        p,
		executor:        exec,
		tools:           make(map[string]tools.Tool),
		policy:          ApprovalPolicyAuto,
		requestedPolicy: ApprovalPolicyAuto,
		state:           StateIdle,
		conversation:    newConversationState(),
		turns:           make([]TurnAudit, 0),
		loopDetector:    loopguard.NewLoopDetector(loopguard.DefaultLoopDetectorConfig()),
		compressor:      compaction.NewCompressor(compressorCfg),
		observer:        observability.Noop{},
		runtimeIdle:     runtimeIdle,
	}
	for _, opt := range opts {
		opt(a)
	}
	a.configureCompressorSummarizer()
	return a, nil
}

// Keep compaction on the active model, including after runtime profile changes.
func (a *Agent) configureCompressorSummarizer() {
	if a.compressor == nil {
		return
	}
	a.compressor.SetSummarizer(compaction.NewProviderSummarizer(a.provider, compaction.WithSummarizerObserver(a.observer, a.cfg.Model)))
}

// RegisterTool adds a tool to the agent. Safe to call after construction.
func (a *Agent) RegisterTool(t tools.Tool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tools[t.Name()] = t
}

// SetApprovalPolicy changes the tool approval policy at runtime.
func (a *Agent) SetApprovalPolicy(p ApprovalPolicy) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requestedPolicy = p
	a.refreshApprovalPolicy()
}

// RequestedApprovalPolicyForInput returns the policy selected by the host or
// user. The effective policy may be stricter when the active profile forces
// manual approval, but it can never be more permissive than this value.
// It does not wait for a remote compaction request: hosts must not block
// their stop/control loop.
func (a *Agent) RequestedApprovalPolicyForInput() (ApprovalPolicy, error) {
	if err := a.lockForInput(); err != nil {
		return "", err
	}
	defer a.mu.Unlock()
	return a.requestedPolicy, nil
}

// TryLock plus the atomic flag closes the check-then-lock race when compaction
// starts concurrently. Ordinary short critical sections retain waiting semantics.
func (a *Agent) lockForInput() error {
	for {
		if a.compacting.Load() {
			return ErrCompacting
		}
		if a.mu.TryLock() {
			return nil
		}
		time.Sleep(time.Millisecond)
	}
}

// SetProfileApprovalFallback forces manual approval without overwriting the
// policy selected by the host or user.
func (a *Agent) SetProfileApprovalFallback(forceManual bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.profileForcesManual = forceManual
	a.refreshApprovalPolicy()
}

func (a *Agent) refreshApprovalPolicy() {
	a.policy = a.requestedPolicy
	if a.profileForcesManual && a.requestedPolicy != ApprovalPolicyBypass {
		a.policy = ApprovalPolicyManual
	}
}

// Description is a user-facing snapshot of the resolved runtime
// configuration — useful for startup banners, /info commands, and
// bug-report context. Everything in it is non-secret (no API keys,
// no base URLs).
type Description struct {
	Profile             string
	Provider            string
	Model               string
	ThinkingLevel       string
	ContextWindow       int // tokens; 0 means "unknown / use registry fallback"
	ApprovalPolicy      ApprovalPolicy
	ClassifierEnabled   bool
	ClassifierAvailable bool // true when a classifier is wired + enabled
	ClassifierProvider  string
	ClassifierModel     string
	ReviewThreshold     string
}

// Describe returns a resolved snapshot of the agent's configuration at
// construction time. Safe to call at any point in the agent lifecycle.
func (a *Agent) Describe() Description {
	a.mu.RLock()
	defer a.mu.RUnlock()

	d := Description{
		Profile:           a.profileName,
		Model:             a.cfg.Model,
		ThinkingLevel:     a.cfg.ThinkingLevel,
		ContextWindow:     a.cfg.ContextWindow,
		ApprovalPolicy:    a.policy,
		ClassifierEnabled: a.cfg.Safety.Classifier.IsEnabled(),
		ReviewThreshold:   a.cfg.Safety.Classifier.ReviewThreshold,
	}
	if d.Profile == "" {
		d.Profile = a.cfg.Provider
	}
	if a.provider != nil {
		d.Provider = a.provider.Name()
	}
	if a.classifier != nil && d.ClassifierEnabled {
		d.ClassifierAvailable = true
		d.ClassifierProvider = a.classifierProfile.Provider
		d.ClassifierModel = a.classifierProfile.Model
	}
	return d
}

// Snapshot returns a deep copy of the agent's current state.
func (a *Agent) Snapshot() Snapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	hist, userContextState := a.conversation.snapshot()
	pending := make([]*PendingAction, len(a.pendingActions))
	copy(pending, a.pendingActions)
	deferred := make([]*PendingAction, len(a.deferredActions))
	copy(deferred, a.deferredActions)
	turns := make([]TurnAudit, len(a.turns))
	for index := range a.turns {
		turns[index] = cloneTurnAudit(a.turns[index])
	}
	return Snapshot{
		State:            a.state,
		History:          hist,
		CumulativeUsage:  a.cumulativeUsage.Normalized(),
		PendingActions:   pending,
		DeferredActions:  deferred,
		TurnSafety:       cloneTurnSafetyState(a.turnSafety),
		Turns:            turns,
		UserContextState: userContextState,
		CreatedAt:        time.Now(),
	}
}

// Restore replaces the agent's state with the given snapshot.
func (a *Agent) Restore(s Snapshot) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state = s.State
	a.conversation.restore(s.History, s.UserContextState)
	a.cumulativeUsage = s.CumulativeUsage.Normalized()
	if a.cumulativeUsage == (protocol.Usage{}) {
		a.cumulativeUsage = protocol.SessionUsage(s.History).Normalized()
	}
	a.pendingTurnUser = nil
	a.pendingActions = s.PendingActions
	a.deferredActions = s.DeferredActions
	a.turnSafety = cloneTurnSafetyState(s.TurnSafety)
	if s.Turns == nil {
		a.turns = make([]TurnAudit, 0)
	} else {
		a.turns = make([]TurnAudit, len(s.Turns))
		for index := range s.Turns {
			a.turns[index] = cloneTurnAudit(s.Turns[index])
		}
	}
}

func cloneTurnSafetyState(state TurnSafetyState) TurnSafetyState {
	state.RequestContext = state.RequestContext.Clone()
	if state.LastDecisionContext != nil {
		call := *state.LastDecisionContext
		state.LastDecisionContext = &call
	}
	return state
}

func cloneTurnAudit(turn TurnAudit) TurnAudit {
	turn.SafetyEvents = append([]AuditEvent(nil), turn.SafetyEvents...)
	for index := range turn.SafetyEvents {
		turn.SafetyEvents[index].ContextSummary.RecentActions = append([]string(nil), turn.SafetyEvents[index].ContextSummary.RecentActions...)
		turn.SafetyEvents[index].ContextSummary.RequestContext = turn.SafetyEvents[index].ContextSummary.RequestContext.Clone()
	}
	return turn
}
