package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jayyao97/zotigo/core/agent/prompt"
	"github.com/jayyao97/zotigo/core/config"
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
	loopDetector  *services.LoopDetector
	compressor    *services.Compressor
	skillManager  *skills.SkillManager
	extraSafeDirs []string

	mu             sync.RWMutex
	state          State
	history        []protocol.Message
	pendingActions []*PendingAction
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
		policy:       ApprovalPolicyManual,
		state:        StateIdle,
		history:      make([]protocol.Message, 0),
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
	return Snapshot{
		State:          a.state,
		History:        hist,
		PendingActions: pending,
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

			stream, err := a.provider.StreamChat(ctx, msgs, toolList)
			if err != nil {
				outCh <- protocol.NewErrorEvent(err)
				return
			}

			// Process stream
			var currentToolCalls []*protocol.ToolCall
			var currentContent string
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
					currentContent += evt.ContentPartDelta.Text
				}
			}

			asstMsg := protocol.NewAssistantMessage(currentContent)
			for _, tc := range currentToolCalls {
				asstMsg.AddToolCall(*tc)
			}
			if providerUsage != nil {
				if asstMsg.Metadata == nil {
					asstMsg.Metadata = &protocol.MessageMetadata{}
				}
				asstMsg.Metadata.Usage = providerUsage
			}

			a.mu.Lock()
			a.history = append(a.history, asstMsg)
			a.mu.Unlock()

			// Handle tool calls
			if len(currentToolCalls) > 0 {
				if a.policy == ApprovalPolicyManual {
					// Split into safe (read-only in working dir) and unsafe tool calls
					var safeCalls, unsafeCalls []*protocol.ToolCall
					for _, tc := range currentToolCalls {
						if a.isToolCallSafe(tc) {
							safeCalls = append(safeCalls, tc)
						} else {
							unsafeCalls = append(unsafeCalls, tc)
						}
					}

					// Auto-execute safe tool calls
					if len(safeCalls) > 0 {
						a.mu.Lock()
						for _, tc := range safeCalls {
							a.pendingActions = append(a.pendingActions, &PendingAction{
								ToolCallID: tc.ID,
								Name:       tc.Name,
								Arguments:  tc.Arguments,
								ToolCall:   tc,
							})
						}
						a.mu.Unlock()

						results, err := a.executePendingActions(ctx)
						if err != nil {
							outCh <- protocol.NewErrorEvent(err)
							return
						}
						for i := range results {
							outCh <- protocol.Event{Type: protocol.EventTypeToolResultDone, ToolResult: &results[i]}
						}
						toolMsg := protocol.NewToolMessage(results)
						a.mu.Lock()
						a.history = append(a.history, toolMsg)
						a.mu.Unlock()
					}

					// Pause for unsafe tool calls
					if len(unsafeCalls) > 0 {
						a.mu.Lock()
						a.state = StatePaused
						for _, tc := range unsafeCalls {
							a.pendingActions = append(a.pendingActions, &PendingAction{
								ToolCallID: tc.ID,
								Name:       tc.Name,
								Arguments:  tc.Arguments,
								ToolCall:   tc,
							})
						}
						a.mu.Unlock()
					}

					continue
				}

				// Auto execution
				a.mu.Lock()
				for _, tc := range currentToolCalls {
					a.pendingActions = append(a.pendingActions, &PendingAction{
						ToolCallID: tc.ID,
						Name:       tc.Name,
						Arguments:  tc.Arguments,
						ToolCall:   tc,
					})
				}
				a.mu.Unlock()

				results, err := a.executePendingActions(ctx)
				if err != nil {
					outCh <- protocol.NewErrorEvent(err)
					return
				}
				for i := range results {
					outCh <- protocol.Event{Type: protocol.EventTypeToolResultDone, ToolResult: &results[i]}
				}

				toolMsg := protocol.NewToolMessage(results)
				a.mu.Lock()
				a.history = append(a.history, toolMsg)
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
	return a.Run(ctx, "")
}

func (a *Agent) executePendingActions(ctx context.Context) ([]protocol.ToolResult, error) {
	a.mu.RLock()
	actions := a.pendingActions
	exec := a.executor
	a.mu.RUnlock()

	var results []protocol.ToolResult
	for _, action := range actions {
		// Check for loops before executing
		if a.loopDetector != nil {
			status := a.loopDetector.RecordCall(action.Name, action.Arguments)
			if status.IsLooping {
				results = append(results, protocol.NewTextToolResult(
					action.ToolCallID,
					fmt.Sprintf("Warning: Loop detected - %s\nSuggestion: %s\n\nProceeding with execution anyway...",
						status.Pattern, status.Suggestion),
					false,
				))
			}
		}

		tool, ok := a.tools[action.Name]
		if !ok {
			tr := protocol.NewTextToolResult(action.ToolCallID, fmt.Sprintf("Error: Tool %s not found", action.Name), true)
			tr.ToolName = action.Name
			results = append(results, tr)
			continue
		}
		res, err := tool.Execute(ctx, exec, action.Arguments)
		if err != nil {
			tr := protocol.NewTextToolResult(action.ToolCallID, fmt.Sprintf("Error: %v", err), true)
			tr.ToolName = action.Name
			results = append(results, tr)
		} else {
			tr := protocol.NewTextToolResult(action.ToolCallID, formatToolOutput(res), false)
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

	// System prompt messages (static + dynamic, each as separate message)
	if a.promptBuilder != nil {
		for _, text := range a.promptBuilder.BuildMessages(pctx) {
			msgs = append(msgs, protocol.NewSystemMessage(text))
		}
	}

	// Dynamically reload and inject skill index (progressive disclosure)
	if a.skillManager != nil {
		a.skillManager.Load()
		if index := a.skillManager.BuildSkillIndex(); index != "" {
			msgs = append(msgs, protocol.NewSystemMessage(index))
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

// isToolCallSafe checks if a tool call can be auto-approved based on the tool's Safety() declaration.
func (a *Agent) isToolCallSafe(tc *protocol.ToolCall) bool {
	tool, ok := a.tools[tc.Name]
	if !ok {
		return false
	}

	safety := tool.Safety()

	// Non-read-only tools always require approval
	if !safety.ReadOnly {
		return false
	}

	// Read-only with no path args → always safe
	if len(safety.PathArgs) == 0 {
		return true
	}

	// Read-only with path args → check all paths are within safe directories
	return a.allPathsInSafeDirs(tc.Arguments, safety.PathArgs)
}

// safeDirs returns all directories that are safe for auto-approved read access.
func (a *Agent) safeDirs() []string {
	dirs := make([]string, 0, 1+len(a.extraSafeDirs))
	dirs = append(dirs, a.executor.WorkDir())
	dirs = append(dirs, a.extraSafeDirs...)
	return dirs
}

// allPathsInSafeDirs checks that all path arguments are within any safe directory.
func (a *Agent) allPathsInSafeDirs(argsJSON string, pathArgs []string) bool {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return false
	}

	safeDirs := a.safeDirs()
	for _, key := range pathArgs {
		val, ok := args[key]
		if !ok || val == nil {
			continue
		}
		pathStr, ok := val.(string)
		if !ok || pathStr == "" {
			continue
		}
		absPath := pathStr
		if !filepath.IsAbs(pathStr) {
			absPath = filepath.Join(a.executor.WorkDir(), pathStr)
		}
		absPath = filepath.Clean(absPath)

		safe := false
		for _, dir := range safeDirs {
			if strings.HasPrefix(absPath, dir) {
				safe = true
				break
			}
		}
		if !safe {
			return false
		}
	}
	return true
}
