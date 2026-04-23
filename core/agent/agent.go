package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jayyao97/zotigo/core/agent/prompt"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/debug"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/services"
	"github.com/jayyao97/zotigo/core/skills"
	"github.com/jayyao97/zotigo/core/tools"
)

// Agent orchestrates provider, tools, and conversation history.
type Agent struct {
	cfg           config.ProfileConfig
	provider      providers.Provider
	executor      executor.Executor
	tools         map[string]tools.Tool
	policy        ApprovalPolicy
	promptBuilder *prompt.SystemPromptBuilder
	userWrapper   *prompt.UserPromptWrapper

	reminderBuilder *prompt.ReminderBuilder

	// Services
	loopDetector                *services.LoopDetector
	compressor                  *services.Compressor
	skillManager                *skills.SkillManager
	classifier                  SafetyClassifier
	classifierProfileName       string
	classifierProfile           config.ProfileConfig
	classifierUnavailableReason string
	extraSafeDirs               []string
	turnSafety                  TurnSafetyState

	middlewares []Middleware

	mu             sync.RWMutex
	state          State
	history        []protocol.Message
	pendingActions []*PendingAction
	turns          []TurnAudit
}

// AgentOption configures an Agent during construction.
type AgentOption func(*Agent)

// WithSystemPromptBuilder sets the system prompt builder.
func WithSystemPromptBuilder(pb *prompt.SystemPromptBuilder) AgentOption {
	return func(a *Agent) { a.promptBuilder = pb }
}

// WithUserPromptWrapper sets the wrapper used to add context to user messages.
func WithUserPromptWrapper(uw *prompt.UserPromptWrapper) AgentOption {
	return func(a *Agent) { a.userWrapper = uw }
}

// WithApprovalPolicy sets the tool approval policy.
func WithApprovalPolicy(p ApprovalPolicy) AgentOption {
	return func(a *Agent) { a.policy = p }
}

// WithCompressorSummarizer sets the LLM summarizer for high-quality summaries.
func WithCompressorSummarizer(s services.Summarizer) AgentOption {
	return func(a *Agent) {
		if a.compressor != nil {
			a.compressor.SetSummarizer(s)
		}
	}
}

// WithSkillManager sets the skill manager.
func WithSkillManager(sm *skills.SkillManager) AgentOption {
	return func(a *Agent) { a.skillManager = sm }
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

// New creates a new Agent with the given configuration and executor.
// The executor parameter provides the environment for tool execution (local, E2B, Docker, etc.)
func New(cfg config.ProfileConfig, exec executor.Executor, opts ...AgentOption) (*Agent, error) {
	p, err := providers.NewProvider(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create provider: %w", err)
	}

	a := &Agent{
		cfg:          cfg,
		provider:     p,
		executor:     exec,
		tools:        make(map[string]tools.Tool),
		policy:       ApprovalPolicyAuto,
		state:        StateIdle,
		history:      make([]protocol.Message, 0),
		turns:        make([]TurnAudit, 0),
		loopDetector: services.NewLoopDetector(services.DefaultLoopDetectorConfig()),
		compressor:   services.NewCompressor(services.DefaultCompressorConfig()),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a, nil
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
	a.policy = p
}

// Executor returns the agent's executor.
func (a *Agent) Executor() executor.Executor {
	return a.executor
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
		Profile:           a.cfg.Provider,
		Model:             a.cfg.Model,
		ThinkingLevel:     a.cfg.ThinkingLevel,
		ApprovalPolicy:    a.policy,
		ClassifierEnabled: a.cfg.Safety.Classifier.IsEnabled(),
		ReviewThreshold:   a.cfg.Safety.Classifier.ReviewThreshold,
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

// SetSkillManager sets the skill manager at runtime.
func (a *Agent) SetSkillManager(sm *skills.SkillManager) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.skillManager = sm
	a.extraSafeDirs = append(a.extraSafeDirs, sm.Dirs()...)
}

// SkillManager returns the agent's skill manager.
func (a *Agent) SkillManager() *skills.SkillManager {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.skillManager
}

// AuditTurns returns a copy of persisted turn audit records.
func (a *Agent) AuditTurns() []TurnAudit {
	a.mu.RLock()
	defer a.mu.RUnlock()

	turns := make([]TurnAudit, len(a.turns))
	copy(turns, a.turns)
	return turns
}

// ResetLoopDetector clears the loop detection history.
// Call this when starting a new conversation or topic.
func (a *Agent) ResetLoopDetector() {
	if a.loopDetector != nil {
		a.loopDetector.Reset()
	}
}

// GetContextStats returns statistics about the current context.
func (a *Agent) GetContextStats() map[string]int {
	a.mu.RLock()
	defer a.mu.RUnlock()

	stats := make(map[string]int)
	stats["message_count"] = len(a.history)
	if a.compressor != nil {
		stats["estimated_tokens"] = a.compressor.CountTokens(a.history)
	}
	if a.loopDetector != nil {
		for k, v := range a.loopDetector.Stats() {
			stats["loop_"+k] = v
		}
	}
	return stats
}

// ForceCompress manually triggers context compression.
// Returns the compression result with statistics.
func (a *Agent) ForceCompress(ctx context.Context) (*services.CompressionResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.compressor == nil {
		return nil, fmt.Errorf("compressor not initialized")
	}

	compressed, result, err := a.compressor.Compress(ctx, a.history)
	if err != nil {
		return nil, err
	}

	if result.Compressed {
		a.history = compressed
	}

	return &result, nil
}

// Snapshot returns a deep copy of the agent's current state.
func (a *Agent) Snapshot() Snapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	hist := make([]protocol.Message, len(a.history))
	copy(hist, a.history)
	pending := make([]*PendingAction, len(a.pendingActions))
	copy(pending, a.pendingActions)
	turns := make([]TurnAudit, len(a.turns))
	copy(turns, a.turns)
	return Snapshot{
		State:          a.state,
		History:        hist,
		PendingActions: pending,
		TurnSafety:     a.turnSafety,
		Turns:          turns,
		CreatedAt:      time.Now(),
	}
}

// Restore replaces the agent's state with the given snapshot.
func (a *Agent) Restore(s Snapshot) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state = s.State
	a.history = s.History
	a.pendingActions = s.PendingActions
	a.turnSafety = s.TurnSafety
	if s.Turns == nil {
		a.turns = make([]TurnAudit, 0)
	} else {
		a.turns = s.Turns
	}
}

// Run continues the execution loop.
func (a *Agent) Run(ctx context.Context, input string) (<-chan protocol.Event, error) {
	if input == "" {
		return a.RunMessage(ctx, protocol.Message{})
	}
	return a.RunMessage(ctx, protocol.NewUserMessage(input))
}

// RunMessage continues the execution loop with a structured message.
func (a *Agent) RunMessage(ctx context.Context, msg protocol.Message) (<-chan protocol.Event, error) {
	a.mu.Lock()

	if msg.Role != "" {
		if a.state == StatePaused {
			a.mu.Unlock()
			return nil, fmt.Errorf("cannot add user input while agent is paused")
		}

		// Wrap user messages with context if wrapper is set
		if msg.Role == protocol.RoleUser && a.userWrapper != nil {
			pctx := prompt.PromptContext{
				WorkDir:  a.executor.WorkDir(),
				Platform: a.executor.Platform(),
				Model:    a.cfg.Model,
			}
			msg = a.wrapUserMessage(msg, pctx)
		}

		a.history = append(a.history, msg)
		if msg.Role == protocol.RoleUser {
			a.startNewTurn(msg.String())
		}
	}

	if a.state == StatePaused && len(a.pendingActions) > 0 {
		a.mu.Unlock()
		return nil, fmt.Errorf("agent is paused waiting for tool outputs")
	}

	a.state = StateRunning
	a.mu.Unlock()

	outCh := make(chan protocol.Event, 100)

	go func() {
		defer close(outCh)

		emptyRecoveryAttempts := 0
		const maxEmptyRecoveryAttempts = 1

		for {
			a.mu.RLock()
			state := a.state
			toolsMap := a.tools
			a.mu.RUnlock()

			if state == StatePaused {
				outCh <- protocol.Event{Type: protocol.EventTypeFinish, FinishReason: "need_approval"}
				return
			}

			a.mu.RLock()
			msgs := a.buildContext()
			a.mu.RUnlock()

			// Prepare tools list (sorted by name for deterministic ordering,
			// which is required for Anthropic prompt caching to work)
			var toolList []tools.Tool
			for _, t := range toolsMap {
				toolList = append(toolList, t)
			}
			sort.Slice(toolList, func(i, j int) bool {
				return toolList[i].Name() < toolList[j].Name()
			})

			estimatedTokens := 0
			if a.compressor != nil {
				estimatedTokens = a.compressor.CountTokens(msgs)
			}
			modelTurnStart := time.Now()
			debug.Logf(
				"agent turn start provider=%s model=%s messages=%d tools=%d est_tokens=%d",
				a.provider.Name(),
				a.cfg.Model,
				len(msgs),
				len(toolList),
				estimatedTokens,
			)

			stream, err := a.provider.StreamChat(ctx, msgs, toolList)
			if err != nil {
				outCh <- protocol.NewErrorEvent(err)
				return
			}

			// Process stream
			var currentToolCalls []*protocol.ToolCall
			var currentContent string
			var currentReasoning string
			var reasoningSignature string
			var reasoningBlocks []protocol.ContentPart // one per ContentEnd for reasoning
			var providerFinishReason protocol.FinishReason
			var providerUsage *protocol.Usage

			for evt := range stream {
				if evt.Type == protocol.EventTypeFinish {
					providerFinishReason = evt.FinishReason
					providerUsage = evt.Usage
					continue
				}

				outCh <- evt

				if evt.Type == protocol.EventTypeToolCallEnd && evt.ToolCall != nil {
					currentToolCalls = append(currentToolCalls, evt.ToolCall)
				}
				if evt.Type == protocol.EventTypeContentDelta && evt.ContentPartDelta != nil {
					if evt.ContentPartDelta.Type == protocol.ContentTypeReasoning {
						currentReasoning += evt.ContentPartDelta.Text
					} else {
						currentContent += evt.ContentPartDelta.Text
					}
				}
				// Each ContentEnd for a reasoning block is snapshotted as its
				// own ContentPart, not merged — Responses API emits multiple
				// reasoning items per turn (one per think → tool → think
				// loop) and each has its own ID + encrypted_content that
				// must be echoed back verbatim next turn to continue the
				// chain of thought. Anthropic thinking fits the same shape
				// (one block per turn, signature preserved).
				if evt.Type == protocol.EventTypeContentEnd && evt.ContentPart != nil &&
					evt.ContentPart.Type == protocol.ContentTypeReasoning {
					text := evt.ContentPart.Text
					if text == "" {
						text = currentReasoning
					}
					reasoningBlocks = append(reasoningBlocks, protocol.ContentPart{
						Type:             protocol.ContentTypeReasoning,
						Text:             text,
						Signature:        evt.ContentPart.Signature,
						ReasoningID:      evt.ContentPart.ReasoningID,
						EncryptedContent: evt.ContentPart.EncryptedContent,
					})
					currentReasoning = ""
					reasoningSignature = evt.ContentPart.Signature
				}
			}

			// Residual reasoning with no matching ContentEnd (provider that
			// streams deltas but never emits end, or a truncated stream).
			if currentReasoning != "" {
				reasoningBlocks = append(reasoningBlocks, protocol.ContentPart{
					Type:      protocol.ContentTypeReasoning,
					Text:      currentReasoning,
					Signature: reasoningSignature,
				})
			}

			asstMsg := protocol.NewAssistantMessage(currentContent)
			// Prepend reasoning blocks in emit order so the next turn
			// sees them with their original inter-block position.
			if len(reasoningBlocks) > 0 {
				asstMsg.Content = append(reasoningBlocks, asstMsg.Content...)
			}
			for _, tc := range currentToolCalls {
				asstMsg.AddToolCall(*tc)
			}
			if providerUsage != nil {
				if asstMsg.Metadata == nil {
					asstMsg.Metadata = &protocol.MessageMetadata{}
				}
				asstMsg.Metadata.Usage = providerUsage
			}

			debug.Logf(
				"agent turn end provider=%s model=%s duration=%s finish=%s content_chars=%d reasoning_chars=%d tool_calls=%d usage_in=%d usage_out=%d",
				a.provider.Name(),
				a.cfg.Model,
				time.Since(modelTurnStart).Round(time.Millisecond),
				providerFinishReason,
				len(currentContent),
				func() int {
					// Sum over all captured reasoning blocks; currentReasoning
					// only holds residual text from any block that never
					// received a ContentEnd.
					n := len(currentReasoning)
					for _, rb := range reasoningBlocks {
						n += len(rb.Text)
					}
					return n
				}(),
				len(currentToolCalls),
				func() int {
					if providerUsage == nil {
						return 0
					}
					return providerUsage.InputTokens
				}(),
				func() int {
					if providerUsage == nil {
						return 0
					}
					return providerUsage.OutputTokens
				}(),
			)

			a.mu.Lock()
			a.history = append(a.history, asstMsg)
			a.mu.Unlock()

			// Reset the empty-recovery budget on any legitimate output.
			// The cap is meant to stop consecutive empty responses, not to
			// limit total recoveries within a long multi-step turn.
			if currentContent != "" || len(currentToolCalls) > 0 {
				emptyRecoveryAttempts = 0
			}

			// Handle tool calls
			if len(currentToolCalls) > 0 {
				var autoCalls []*PendingAction
				var approvalCalls []*PendingAction
				var immediateResults []protocol.ToolResult

				for _, tc := range currentToolCalls {
					decision := a.classifyToolCall(tc)
					a.appendSafetyEvent(AuditEvent{
						ToolCallID:         tc.ID,
						ToolName:           tc.Name,
						ToolArgsSummary:    summarizeText(tc.Arguments, 240),
						DecisionSource:     decision.Source,
						Decision:           mapExecutionDecision(decision.Decision),
						Reason:             decision.Reason,
						RiskLevel:          decision.RiskLevel,
						SnapshotStatus:     SnapshotStatusNotNeeded,
						ClassifierProvider: a.classifierProfile.Provider,
						ClassifierModel:    a.classifierProfile.Model,
						ContextSummary: AuditContextSummary{
							UserPrompt: a.turnSafety.CurrentUserPrompt,
							Trigger:    decision.Reason,
						},
						RawContext: decision.RawContext,
					})

					action := &PendingAction{
						ToolCallID: tc.ID,
						Name:       tc.Name,
						Arguments:  tc.Arguments,
						Decision:   decision,
						ToolCall:   tc,
					}
					switch decision.Decision {
					case ExecutionDecisionAutoExecute:
						autoCalls = append(autoCalls, action)
					case ExecutionDecisionRequireApproval:
						approvalCalls = append(approvalCalls, action)
					case ExecutionDecisionBlock:
						immediateResults = append(immediateResults, protocol.ToolResult{
							ToolCallID: tc.ID,
							ToolName:   tc.Name,
							Type:       protocol.ToolResultTypeExecutionDenied,
							Reason:     decision.Reason,
							IsError:    true,
						})
					}
				}

				if len(autoCalls) > 0 {
					a.mu.Lock()
					a.pendingActions = append(a.pendingActions, autoCalls...)
					a.mu.Unlock()

					results, err := a.executePendingActions(ctx)
					if err != nil {
						outCh <- protocol.NewErrorEvent(err)
						return
					}
					immediateResults = append(immediateResults, results...)
				}

				if len(immediateResults) > 0 {
					for i := range immediateResults {
						outCh <- protocol.Event{Type: protocol.EventTypeToolResultDone, ToolResult: &immediateResults[i]}
					}
					toolMsg := protocol.NewToolMessage(immediateResults)
					a.mu.Lock()
					a.history = append(a.history, toolMsg)
					a.mu.Unlock()
				}

				if len(approvalCalls) > 0 {
					a.mu.Lock()
					a.state = StatePaused
					a.pendingActions = append(a.pendingActions, approvalCalls...)
					a.mu.Unlock()
				}

				continue
			}

			// Recover from empty responses: the model emitted no text and no
			// tool calls (all output_tokens went into reasoning). Without this
			// guard the turn would end silently mid-task. Nudge the model once
			// with a system-reminder and retry; if that also comes back empty,
			// fall through to the normal end-of-turn.
			if currentContent == "" && len(currentToolCalls) == 0 &&
				emptyRecoveryAttempts < maxEmptyRecoveryAttempts {
				emptyRecoveryAttempts++
				debug.Logf(
					"agent empty response recovery attempt=%d reasoning_chars=%d",
					emptyRecoveryAttempts, len(currentReasoning),
				)
				nudge := protocol.NewUserMessage(
					"<system-reminder>\nYour previous response contained no " +
						"visible text and no tool call. If the task is not " +
						"yet complete, continue by emitting the next tool " +
						"call. If the task is complete, reply with a short " +
						"summary of what was done.\n</system-reminder>",
				)
				a.mu.Lock()
				a.history = append(a.history, nudge)
				a.mu.Unlock()
				continue
			}

			// No tool calls -> turn finished
			a.mu.Lock()
			a.state = StateIdle
			a.mu.Unlock()

			outCh <- protocol.NewFinishEvent(providerFinishReason)
			return
		}
	}()

	return outCh, nil
}

// ApproveAndExecutePendingActions executes all pending tool calls and continues.
func (a *Agent) ApproveAndExecutePendingActions(ctx context.Context) (<-chan protocol.Event, error) {
	a.mu.RLock()
	actions := make([]*PendingAction, len(a.pendingActions))
	copy(actions, a.pendingActions)
	a.mu.RUnlock()
	for _, action := range actions {
		a.appendSafetyEvent(AuditEvent{
			ToolCallID:      action.ToolCallID,
			ToolName:        action.Name,
			ToolArgsSummary: summarizeText(action.Arguments, 240),
			DecisionSource:  SafetyDecisionSourceUserApproval,
			Decision:        SafetyClassifierDecisionAllow,
			Reason:          "user approved action",
			RiskLevel:       action.Decision.RiskLevel,
			SnapshotStatus:  SnapshotStatusNotNeeded,
			ContextSummary: AuditContextSummary{
				UserPrompt: a.turnSafety.CurrentUserPrompt,
				Trigger:    "user approved pending action",
			},
		})
	}

	results, err := a.executePendingActions(ctx)
	if err != nil {
		return nil, err
	}
	innerCh, err := a.SubmitToolOutputs(ctx, results)
	if err != nil {
		return nil, err
	}

	// Prepend tool result events before the inner stream.
	outCh := make(chan protocol.Event, len(results)+100)
	for i := range results {
		outCh <- protocol.Event{Type: protocol.EventTypeToolResultDone, ToolResult: &results[i]}
	}
	go func() {
		defer close(outCh)
		for evt := range innerCh {
			outCh <- evt
		}
	}()
	return outCh, nil
}

// SubmitToolOutputs submits tool results and resumes the agent loop.
func (a *Agent) SubmitToolOutputs(ctx context.Context, outputs []protocol.ToolResult) (<-chan protocol.Event, error) {
	a.mu.Lock()
	if a.state != StatePaused {
		a.mu.Unlock()
		return nil, fmt.Errorf("agent is not paused")
	}
	a.pendingActions = nil
	a.state = StateRunning
	msg := protocol.NewToolMessage(outputs)
	a.history = append(a.history, msg)
	a.mu.Unlock()

	for _, output := range outputs {
		if output.Type != protocol.ToolResultTypeExecutionDenied {
			continue
		}
		a.appendSafetyEvent(AuditEvent{
			ToolCallID:     output.ToolCallID,
			ToolName:       output.ToolName,
			DecisionSource: SafetyDecisionSourceUserApproval,
			Decision:       SafetyClassifierDecisionDeny,
			Reason:         output.Reason,
			SnapshotStatus: SnapshotStatusNotNeeded,
			ContextSummary: AuditContextSummary{
				UserPrompt: a.turnSafety.CurrentUserPrompt,
				Trigger:    "pending action denied",
			},
		})
	}
	return a.Run(ctx, "")
}

func (a *Agent) executePendingActions(ctx context.Context) ([]protocol.ToolResult, error) {
	a.mu.RLock()
	actions := a.pendingActions
	exec := a.executor
	a.mu.RUnlock()

	if err := a.ensureSnapshotForActions(ctx, exec, actions); err != nil {
		return nil, err
	}

	var results []protocol.ToolResult
	for _, action := range actions {
		// A loop-detector warning is prefixed to the real tool result
		// (rather than appended as a separate ToolResult) so the tool
		// call ID stays unique within the tool-role message we emit —
		// OpenAI Chat Completions rejects a request whose messages
		// contain two tool-role entries with the same tool_call_id.
		var loopWarning string
		if a.loopDetector != nil {
			status := a.loopDetector.RecordCall(action.Name, action.Arguments)
			if status.IsLooping {
				loopWarning = fmt.Sprintf(
					"<system-reminder>\nLoop detected: %s\nSuggestion: %s\nProceeding with execution anyway.\n</system-reminder>\n\n",
					status.Pattern, status.Suggestion,
				)
			}
		}

		prefixed := func(text string) string {
			if loopWarning == "" {
				return text
			}
			return loopWarning + text
		}

		tool, ok := a.tools[action.Name]
		if !ok {
			tr := protocol.NewTextToolResult(action.ToolCallID, prefixed(fmt.Sprintf("Error: Tool %s not found", action.Name)), true)
			tr.ToolName = action.Name
			results = append(results, tr)
			continue
		}
		call := &ToolCall{
			Tool:      tool,
			Name:      action.Name,
			Arguments: action.Arguments,
			Executor:  exec,
		}
		invoke := buildMiddlewareChain(a.middlewares, func(ctx context.Context, c *ToolCall) (any, error) {
			return c.Tool.Execute(ctx, c.Executor, c.Arguments)
		})
		res, err := invoke(ctx, call)
		if err != nil {
			tr := protocol.NewTextToolResult(action.ToolCallID, prefixed(fmt.Sprintf("Error: %v", err)), true)
			tr.ToolName = action.Name
			results = append(results, tr)
		} else {
			tr := protocol.NewTextToolResult(action.ToolCallID, prefixed(formatToolOutput(res)), false)
			tr.ToolName = action.Name
			results = append(results, tr)
		}
	}
	// Inject reminders into the last tool result
	if len(results) > 0 && a.reminderBuilder != nil {
		pctx := prompt.PromptContext{
			WorkDir:  a.executor.WorkDir(),
			Platform: a.executor.Platform(),
			Model:    a.cfg.Model,
		}
		tcResults := make([]prompt.ToolCallResult, len(results))
		for i, r := range results {
			tcResults[i] = prompt.ToolCallResult{
				Name:    r.ToolName,
				Result:  r.Text,
				IsError: r.IsError,
			}
		}
		if s := a.reminderBuilder.Build(pctx, tcResults); s != "" {
			results[len(results)-1].Text += s
		}
	}

	a.mu.Lock()
	a.pendingActions = nil
	a.mu.Unlock()
	return results, nil
}

func (a *Agent) startNewTurn(userPrompt string) {
	turnID := fmt.Sprintf("turn_%d", time.Now().UnixNano())
	now := time.Now()
	a.turnSafety = TurnSafetyState{
		TurnID:            turnID,
		CurrentUserPrompt: userPrompt,
	}
	a.turns = append(a.turns, TurnAudit{
		ID:                turnID,
		CreatedAt:         now,
		UpdatedAt:         now,
		UserPromptSummary: summarizeText(userPrompt, 240),
		SnapshotStatus:    SnapshotStatusNotNeeded,
		SafetyEvents:      make([]AuditEvent, 0),
	})
}

func (a *Agent) currentTurnIndex() int {
	if a.turnSafety.TurnID == "" {
		return -1
	}
	for i := len(a.turns) - 1; i >= 0; i-- {
		if a.turns[i].ID == a.turnSafety.TurnID {
			return i
		}
	}
	return -1
}

func (a *Agent) appendSafetyEvent(event AuditEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	if event.TurnID == "" {
		event.TurnID = a.turnSafety.TurnID
	}

	idx := a.currentTurnIndex()
	if idx == -1 {
		now := time.Now()
		turnID := a.turnSafety.TurnID
		if turnID == "" {
			turnID = fmt.Sprintf("turn_%d", now.UnixNano())
			a.turnSafety.TurnID = turnID
		}
		a.turns = append(a.turns, TurnAudit{
			ID:             turnID,
			CreatedAt:      now,
			UpdatedAt:      now,
			SnapshotStatus: SnapshotStatusNotNeeded,
			SafetyEvents:   make([]AuditEvent, 0),
		})
		idx = len(a.turns) - 1
	}

	a.turns[idx].UpdatedAt = time.Now()
	a.turns[idx].SafetyEvents = append(a.turns[idx].SafetyEvents, event)
}

func (a *Agent) setTurnSnapshot(status SnapshotStatus, snapshotID string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	idx := a.currentTurnIndex()
	if idx == -1 {
		return
	}
	a.turns[idx].UpdatedAt = time.Now()
	a.turns[idx].SnapshotStatus = status
	a.turns[idx].SnapshotID = snapshotID
	a.turnSafety.SnapshotID = snapshotID
	a.turnSafety.SnapshotCreated = status == SnapshotStatusCreated
	a.turnSafety.SnapshotFailed = status == SnapshotStatusFailed
	a.turnSafety.SnapshotAttempted = status == SnapshotStatusCreated || status == SnapshotStatusFailed
}

// classifyToolCall decides how a tool call should be handled.
//
// Each tool assigns an ordinal SafetyLevel to the call. The agent
// compares that against a single configurable threshold:
//
//	Level         | Manual mode       | Auto mode
//	LevelSafe     | auto_execute      | auto_execute
//	LevelLow      | require_approval  | auto_execute (< threshold) or
//	              |                   | classifier (>= threshold)
//	LevelMedium   | require_approval  | auto_execute (< threshold) or
//	              |                   | classifier (>= threshold)
//	LevelHigh     | require_approval  | classifier (default threshold),
//	              |                   | or approval when classifier is off
//	LevelBlocked  | block             | block
//
// Auto threshold is config.Safety.Classifier.ReviewThreshold; default
// LevelMedium. Manual threshold is fixed at LevelLow — any mutation
// gets a prompt. When the threshold is set to "off", the classifier is
// never called but LevelHigh calls still require user approval ("off"
// disables classifier calls, not safety).
func (a *Agent) classifyToolCall(tc *protocol.ToolCall) ActionDecision {
	tool, ok := a.tools[tc.Name]
	if !ok {
		return ActionDecision{
			Decision:  ExecutionDecisionBlock,
			Source:    SafetyDecisionSourceHardRule,
			Reason:    "tool not found",
			RiskLevel: tools.LevelBlocked.String(),
		}
	}

	decision := tool.Classify(tools.SafetyCall{
		Arguments: tc.Arguments,
		WorkDir:   a.executor.WorkDir(),
		SafeDirs:  a.safeDirs(),
		Executor:  a.executor,
	})

	level := decision.Level
	riskLabel := level.String()

	if level >= tools.LevelBlocked {
		return ActionDecision{
			Decision:  ExecutionDecisionBlock,
			Source:    SafetyDecisionSourceHardRule,
			Reason:    decision.Reason,
			RiskLevel: riskLabel,
		}
	}

	if level <= tools.LevelSafe {
		return ActionDecision{
			Decision:         ExecutionDecisionAutoExecute,
			Source:           SafetyDecisionSourceHardRule,
			Reason:           decision.Reason,
			RiskLevel:        riskLabel,
			RequiresSnapshot: decision.RequiresSnapshot,
		}
	}

	if a.policy != ApprovalPolicyAuto {
		// Manual mode: any mutation (level > Safe) prompts the user.
		return ActionDecision{
			Decision:         ExecutionDecisionRequireApproval,
			Source:           SafetyDecisionSourceHardRule,
			Reason:           decision.Reason,
			RiskLevel:        riskLabel,
			RequiresSnapshot: decision.RequiresSnapshot,
		}
	}

	threshold := tools.ParseSafetyLevel(a.cfg.Safety.Classifier.ReviewThreshold)
	// "off" / "none" parses to a level beyond LevelHigh. Interpret this as
	// "never call the classifier" but still require approval for anything
	// LevelHigh and above — the setting disables the classifier, not
	// safety. Clamp the threshold so High stays above it.
	classifierDisabled := threshold > tools.LevelHigh
	if classifierDisabled {
		threshold = tools.LevelHigh
	}

	if level < threshold {
		// Below the configured review bar: trust the tool's call.
		return ActionDecision{
			Decision:         ExecutionDecisionAutoExecute,
			Source:           SafetyDecisionSourceHardRule,
			Reason:           decision.Reason,
			RiskLevel:        riskLabel,
			RequiresSnapshot: decision.RequiresSnapshot,
		}
	}

	// At or above the threshold: consult the classifier (when enabled).
	if !classifierDisabled {
		if resp, ok := a.classifyWithSafetyClassifier(tc, riskLabel); ok {
			return resp
		}
	}
	// Classifier disabled or unavailable — fall back to user approval.
	return ActionDecision{
		Decision:         ExecutionDecisionRequireApproval,
		Source:           SafetyDecisionSourceHardRule,
		Reason:           decision.Reason,
		RiskLevel:        riskLabel,
		RequiresSnapshot: decision.RequiresSnapshot,
	}
}

func (a *Agent) classifyWithSafetyClassifier(tc *protocol.ToolCall, riskLevel string) (ActionDecision, bool) {
	if !a.cfg.Safety.Classifier.IsEnabled() {
		return ActionDecision{}, false
	}
	if a.classifier == nil {
		reason := a.classifierUnavailableReason
		if reason == "" {
			reason = "classifier enabled but no classifier implementation configured; falling back to user approval"
		}
		return ActionDecision{
			Decision:  ExecutionDecisionRequireApproval,
			Source:    SafetyDecisionSourceClassifier,
			Reason:    reason,
			RiskLevel: riskLevel,
		}, true
	}

	req := SafetyClassifierRequest{
		UserPrompt:    a.turnSafety.CurrentUserPrompt,
		ToolName:      tc.Name,
		ToolArguments: tc.Arguments,
		RiskLevel:     riskLevel,
		IsGitRepo:     a.isGitRepository(context.Background(), a.executor),
		HasSnapshot:   a.turnSafety.SnapshotCreated,
		RecentActions: a.recentActionsForClassifier(),
	}

	// Capture a bounded dump of the classifier request for audit when the
	// operator has explicitly opted in. Never attached otherwise.
	var rawCtx string
	if a.cfg.Safety.Classifier.CaptureRawAuditContext {
		limit := a.cfg.Safety.Classifier.MaxAuditContextChars
		if limit <= 0 {
			limit = 1200
		}
		rawCtx = summarizeClassifierRequest(req, limit)
	}

	resp, err := a.classifier.Classify(req)
	if err != nil {
		return ActionDecision{
			Decision:   ExecutionDecisionRequireApproval,
			Source:     SafetyDecisionSourceClassifier,
			Reason:     fmt.Sprintf("classifier error: %v; falling back to user approval", err),
			RiskLevel:  riskLevel,
			RawContext: rawCtx,
		}, true
	}

	switch resp.Decision {
	case SafetyClassifierDecisionAllow:
		// Classifier is only called in Auto mode (see classifyToolCall /
		// classifyShellToolCall), so allow always means auto-execute. Manual
		// mode never consults the classifier — users review every mutating
		// action themselves.
		return ActionDecision{
			Decision:         ExecutionDecisionAutoExecute,
			Source:           SafetyDecisionSourceClassifier,
			Reason:           resp.Reason,
			RiskLevel:        riskLevel,
			RequiresSnapshot: resp.RequiresSnapshot,
			RawContext:       rawCtx,
		}, true
	case SafetyClassifierDecisionDeny:
		return ActionDecision{
			Decision:   ExecutionDecisionBlock,
			Source:     SafetyDecisionSourceClassifier,
			Reason:     resp.Reason,
			RiskLevel:  riskLevel,
			RawContext: rawCtx,
		}, true
	case SafetyClassifierDecisionAskUser:
		return ActionDecision{
			Decision:         ExecutionDecisionRequireApproval,
			Source:           SafetyDecisionSourceClassifier,
			Reason:           resp.Reason,
			RiskLevel:        riskLevel,
			RequiresSnapshot: resp.RequiresSnapshot,
			RawContext:       rawCtx,
		}, true
	default:
		return ActionDecision{}, false
	}
}

// summarizeClassifierRequest renders a bounded textual dump of a classifier
// request for audit persistence. It is only used when the operator has
// enabled CaptureRawAuditContext.
func summarizeClassifierRequest(req SafetyClassifierRequest, limit int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "user_prompt: %s\n", summarizeText(req.UserPrompt, 400))
	fmt.Fprintf(&sb, "tool: %s\n", req.ToolName)
	fmt.Fprintf(&sb, "args: %s\n", summarizeText(req.ToolArguments, 400))
	fmt.Fprintf(&sb, "risk_level: %s\n", req.RiskLevel)
	fmt.Fprintf(&sb, "is_git_repo: %t\n", req.IsGitRepo)
	fmt.Fprintf(&sb, "has_snapshot: %t\n", req.HasSnapshot)
	if len(req.RecentActions) > 0 {
		sb.WriteString("recent_actions:\n")
		for _, a := range req.RecentActions {
			status := "ok"
			if a.IsError {
				status = "error"
			}
			fmt.Fprintf(&sb, "- [%s] %s: %s\n", status, a.ToolName, summarizeText(a.Result, 120))
		}
	}
	return summarizeText(sb.String(), limit)
}

func (a *Agent) recentActionsForClassifier() []RecentAction {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var actions []RecentAction
	for i := len(a.history) - 1; i >= 0 && len(actions) < 6; i-- {
		msg := a.history[i]
		for _, cp := range msg.Content {
			if cp.Type == protocol.ContentTypeToolResult && cp.ToolResult != nil {
				actions = append(actions, RecentAction{
					ToolName: cp.ToolResult.ToolName,
					Result:   summarizeText(cp.ToolResult.Text, 160),
					IsError:  cp.ToolResult.IsError,
				})
			}
		}
	}
	return actions
}

func (a *Agent) ensureSnapshotForActions(ctx context.Context, exec executor.Executor, actions []*PendingAction) error {
	var needsSnapshot bool
	for _, action := range actions {
		if action.Decision.RequiresSnapshot {
			needsSnapshot = true
			break
		}
	}
	if !needsSnapshot || a.turnSafety.SnapshotCreated {
		return nil
	}

	if !a.isGitRepository(ctx, exec) {
		a.setTurnSnapshot(SnapshotStatusMissingGitRepo, "")
		return nil
	}

	a.turnSafety.SnapshotAttempted = true
	result, err := exec.Exec(ctx, `snap-commit store -m "zotigo pre-action snapshot"`, executor.ExecOptions{})

	// snap-commit is an optional external binary. If it isn't installed,
	// degrade gracefully: mark the turn as unsnapshotted but do not abort
	// the user's action. The same pattern is used in
	// cli/commands/builtin/snapshot.go for explicit /snapshot invocations.
	//
	// LocalExecutor swallows *exec.ExitError (so a missing binary surfaces
	// as err=nil, exit=127, stderr contains "command not found") while other
	// executors may surface it as a non-nil error. Check both paths.
	if isCommandNotFound(err, result) {
		a.setTurnSnapshot(SnapshotStatusNotInstalled, "")
		return nil
	}
	if err != nil {
		a.setTurnSnapshot(SnapshotStatusFailed, "")
		return fmt.Errorf("failed to create pre-action snapshot: %w", err)
	}
	if !result.Success() {
		a.setTurnSnapshot(SnapshotStatusFailed, "")
		stderr := strings.TrimSpace(string(result.Stderr))
		if stderr == "" {
			stderr = fmt.Sprintf("exit code %d", result.ExitCode)
		}
		return fmt.Errorf("failed to create pre-action snapshot: %s", stderr)
	}

	snapshotID := summarizeText(strings.TrimSpace(string(result.Stdout)), 120)
	a.setTurnSnapshot(SnapshotStatusCreated, snapshotID)
	return nil
}

// isCommandNotFound reports whether an exec error / result pair indicates the
// snap-commit binary is missing from PATH, as opposed to snap-commit itself
// failing with a non-zero exit.
//
// We deliberately use narrow, anchored matches. A bare "not found" substring
// is too broad: snap-commit's own failure messages can legitimately contain
// "ref not found" or "object not found", and mis-classifying those as
// "binary missing" would silently skip the snapshot for a protected action.
func isCommandNotFound(err error, result *executor.ExecResult) bool {
	if err != nil {
		// Go's os/exec surfaces a missing binary with this canonical string
		// (see exec.LookPath / *exec.Error).
		if strings.Contains(strings.ToLower(err.Error()), "executable file not found") {
			return true
		}
	}
	if result != nil {
		// POSIX shells return 127 when the command does not exist. This is the
		// most reliable signal across platforms.
		if result.ExitCode == 127 {
			return true
		}
		// Some non-POSIX execution paths may not set exit 127 but still print
		// the canonical "command not found" phrase to stderr. Match only the
		// anchored phrase to avoid swallowing unrelated snap-commit errors.
		if strings.Contains(strings.ToLower(string(result.Stderr)), "command not found") {
			return true
		}
	}
	return false
}

func (a *Agent) isGitRepository(ctx context.Context, exec executor.Executor) bool {
	result, err := exec.Exec(ctx, "git rev-parse --is-inside-work-tree", executor.ExecOptions{Timeout: 2 * time.Second})
	if err != nil || !result.Success() {
		return false
	}
	return strings.TrimSpace(string(result.Stdout)) == "true"
}

func summarizeText(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}

func mapExecutionDecision(decision ExecutionDecision) SafetyClassifierDecision {
	switch decision {
	case ExecutionDecisionAutoExecute:
		return SafetyClassifierDecisionAllow
	case ExecutionDecisionBlock:
		return SafetyClassifierDecisionDeny
	default:
		return SafetyClassifierDecisionAskUser
	}
}

// formatToolOutput converts a tool's return value to a readable string.
// Slices are joined with newlines instead of Go's default bracket format.
func formatToolOutput(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case []string:
		return strings.Join(val, "\n")
	case []any:
		var lines []string
		for _, item := range val {
			lines = append(lines, fmt.Sprintf("%v", item))
		}
		return strings.Join(lines, "\n")
	default:
		return fmt.Sprintf("%v", v)
	}
}

func (a *Agent) buildContext() []protocol.Message {
	var msgs []protocol.Message

	pctx := prompt.PromptContext{
		WorkDir:  a.executor.WorkDir(),
		Platform: a.executor.Platform(),
		Model:    a.cfg.Model,
	}

	// System prompt messages (static + dynamic, each as separate message).
	// System array stays ≤ 2 messages so Anthropic can keep ephemeral cache
	// on block[0] and small models can rely on first-system attention.
	// Dynamic content that would otherwise land in a third system message
	// (skill index, per-turn snapshots) rides the latest user message via
	// userTurnReminders instead.
	if a.promptBuilder != nil {
		for _, text := range a.promptBuilder.BuildMessages(pctx) {
			msgs = append(msgs, protocol.NewSystemMessage(text))
		}
	}

	history := a.history

	// Check if compression is needed
	if a.compressor != nil && a.compressor.NeedsCompression(history) {
		compressed, result, err := a.compressor.Compress(context.Background(), history)
		if err == nil && result.Compressed {
			history = compressed
			a.history = history
		}
	}

	// Truncate long tool results
	if a.compressor != nil {
		history = a.compressor.TruncateToolResults(history, 2000)
	}

	msgs = append(msgs, history...)
	return msgs
}

// wrapUserMessage applies the UserPromptWrapper to a user message.
// It extracts text content, wraps it, and preserves non-text parts (images, etc.).
func (a *Agent) wrapUserMessage(msg protocol.Message, pctx prompt.PromptContext) protocol.Message {
	var rawTexts []string
	var nonTextParts []protocol.ContentPart
	for _, p := range msg.Content {
		if p.Type == protocol.ContentTypeText {
			rawTexts = append(rawTexts, p.Text)
		} else {
			nonTextParts = append(nonTextParts, p)
		}
	}

	if len(rawTexts) == 0 {
		return msg
	}

	wrapped := a.userWrapper.Wrap(strings.Join(rawTexts, "\n"), pctx)

	var newContent []protocol.ContentPart
	newContent = append(newContent, protocol.ContentPart{
		Type: protocol.ContentTypeText,
		Text: wrapped,
	})
	newContent = append(newContent, nonTextParts...)

	return protocol.Message{
		ID:        msg.ID,
		Role:      msg.Role,
		Content:   newContent,
		Metadata:  msg.Metadata,
		CreatedAt: msg.CreatedAt,
	}
}

// safeDirs returns all directories that are safe for auto-approved read access.
func (a *Agent) safeDirs() []string {
	dirs := make([]string, 0, 1+len(a.extraSafeDirs))
	dirs = append(dirs, a.executor.WorkDir())
	dirs = append(dirs, a.extraSafeDirs...)
	return dirs
}
