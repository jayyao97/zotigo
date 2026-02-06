package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/services"
	"github.com/jayyao97/zotigo/core/skills"
	"github.com/jayyao97/zotigo/core/tools"
)

type Agent struct {
	cfg       config.ProfileConfig
	provider  providers.Provider
	executor  executor.Executor
	tools     map[string]tools.Tool
	policy    ApprovalPolicy
	sysPrompt string

	// Services
	loopDetector *services.LoopDetector
	compressor   *services.Compressor
	skillManager *skills.SkillManager

	mu             sync.RWMutex
	state          State
	history        []protocol.Message
	pendingActions []*PendingAction
}

// New creates a new Agent with the given configuration, executor, and system prompt.
// The executor parameter provides the environment for tool execution (local, E2B, Docker, etc.)
func New(cfg config.ProfileConfig, exec executor.Executor, sysPrompt string) (*Agent, error) {
	p, err := providers.NewProvider(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create provider: %w", err)
	}

	return &Agent{
		cfg:          cfg,
		provider:     p,
		executor:     exec,
		tools:        make(map[string]tools.Tool),
		policy:       ApprovalPolicyManual,
		sysPrompt:    sysPrompt,
		state:        StateIdle,
		history:      make([]protocol.Message, 0),
		loopDetector: services.NewLoopDetector(services.DefaultLoopDetectorConfig()),
		compressor:   services.NewCompressor(services.DefaultCompressorConfig()),
	}, nil
}

// Executor returns the agent's executor
func (a *Agent) Executor() executor.Executor {
	return a.executor
}

func (a *Agent) RegisterTool(t tools.Tool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tools[t.Name()] = t
}

func (a *Agent) SetApprovalPolicy(p ApprovalPolicy) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.policy = p
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

// SetCompressorSummarizer sets the LLM summarizer for high-quality summaries.
func (a *Agent) SetCompressorSummarizer(s services.Summarizer) {
	if a.compressor != nil {
		a.compressor.SetSummarizer(s)
	}
}

// SetSkillManager sets the skill manager for skill-based conversations.
func (a *Agent) SetSkillManager(sm *skills.SkillManager) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.skillManager = sm
}

// SkillManager returns the agent's skill manager.
func (a *Agent) SkillManager() *skills.SkillManager {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.skillManager
}

// ... Snapshot/Restore ... (omitted for brevity, keep existing)
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

		// Process $skill mentions in user messages
		if msg.Role == protocol.RoleUser && a.skillManager != nil {
			for _, part := range msg.Content {
				if part.Type == protocol.ContentTypeText {
					a.skillManager.ProcessMentions(part.Text)
				}
			}
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
			toolsMap := a.tools // Copy reference
			a.mu.RUnlock()
			
			if state == StatePaused {
				outCh <- protocol.Event{Type: protocol.EventTypeFinish, FinishReason: "need_approval"}
				return
			}

			a.mu.RLock()
			msgs := a.buildContext()
			a.mu.RUnlock()

			// Prepare Tools List
			var toolList []tools.Tool
			for _, t := range toolsMap {
				toolList = append(toolList, t)
			}

			// Pass tools to StreamChat
			stream, err := a.provider.StreamChat(ctx, msgs, toolList)
			if err != nil {
				outCh <- protocol.NewErrorEvent(err)
				return
			}

			// 3. Process Stream
			var currentToolCalls []*protocol.ToolCall
			var currentContent string
			var providerFinishReason protocol.FinishReason
			
			for evt := range stream {
				// Intercept Finish Event
				if evt.Type == protocol.EventTypeFinish {
					providerFinishReason = evt.FinishReason
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
			
			a.mu.Lock()
			a.history = append(a.history, asstMsg)
			a.mu.Unlock()

						// 5. Handle Tool Calls (The Decision Point)
						if len(currentToolCalls) > 0 {
							if a.policy == ApprovalPolicyManual {
								a.mu.Lock()
								a.state = StatePaused
								for _, tc := range currentToolCalls {
									a.pendingActions = append(a.pendingActions, &PendingAction{
										ToolCallID: tc.ID,
										Name:       tc.Name,
										Arguments:  tc.Arguments,
										ToolCall:   tc,
									})
								}
								a.mu.Unlock()
								// Emit custom finish reason for UI to trigger approval
								// Wait, the loop will continue, check StatePaused, and emit "need_approval".
								// So we just continue here.
								continue 
							} else {
								// ... (Auto execution logic same as before) ...
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
								
								toolMsg := protocol.NewToolMessage(results)
								a.mu.Lock()
								a.history = append(a.history, toolMsg)
								a.mu.Unlock()
								continue 
							}
						}
						
						// No tool calls -> Turn Finished
						a.mu.Lock()
						a.state = StateIdle
						a.mu.Unlock()
						
						// Emit the actual finish event from provider (e.g. Stop)
						outCh <- protocol.NewFinishEvent(providerFinishReason)
						return
					}	}()

	return outCh, nil
}

func (a *Agent) ApproveAndExecutePendingActions(ctx context.Context) (<-chan protocol.Event, error) {
	results, err := a.executePendingActions(ctx)
	if err != nil {
		return nil, err
	}
	return a.SubmitToolOutputs(ctx, results)
}

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
				// Add warning to the result
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
			results = append(results, protocol.NewTextToolResult(action.ToolCallID, fmt.Sprintf("Error: Tool %s not found", action.Name), true))
			continue
		}
		// Pass executor to tool.Execute
		res, err := tool.Execute(ctx, exec, action.Arguments)
		if err != nil {
			results = append(results, protocol.NewTextToolResult(action.ToolCallID, fmt.Sprintf("Error: %v", err), true))
		} else {
			results = append(results, protocol.NewTextToolResult(action.ToolCallID, fmt.Sprintf("%v", res), false))
		}
	}
	a.mu.Lock()
	a.pendingActions = nil
	a.mu.Unlock()
	return results, nil
}

func (a *Agent) buildContext() []protocol.Message {
	var msgs []protocol.Message
	if a.sysPrompt != "" {
		msgs = append(msgs, protocol.NewSystemMessage(a.sysPrompt))
	}

	// Inject activated skills as system messages
	if a.skillManager != nil {
		injection := a.skillManager.BuildAllInjections()
		if injection != "" {
			msgs = append(msgs, protocol.NewSystemMessage(injection))
		}
	}

	history := a.history

	// Check if compression is needed
	if a.compressor != nil && a.compressor.NeedsCompression(history) {
		// Compress history using intelligent partitioning
		compressed, result, err := a.compressor.Compress(context.Background(), history)
		if err == nil && result.Compressed {
			history = compressed
			// Update the agent's history with compressed version
			// Note: This modifies history in place, which is safe since we hold the lock
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