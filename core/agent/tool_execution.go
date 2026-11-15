package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jayyao97/zotigo/core/agent/prompt"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/tools"
)

func interruptedToolResult(callID string, toolName string) protocol.ToolResult {
	return protocol.ToolResult{
		ToolCallID: callID,
		ToolName:   toolName,
		Type:       protocol.ToolResultTypeExecutionDenied,
		Reason:     "interrupted_by_steering",
		IsError:    true,
	}
}

func interruptedToolResults(calls []*protocol.ToolCall) []protocol.ToolResult {
	results := make([]protocol.ToolResult, 0, len(calls))
	for _, call := range calls {
		if call == nil {
			continue
		}
		results = append(results, interruptedToolResult(call.ID, call.Name))
	}
	return results
}

func interruptedActionResults(actions []*PendingAction) []protocol.ToolResult {
	results := make([]protocol.ToolResult, 0, len(actions))
	for _, action := range actions {
		if action == nil {
			continue
		}
		results = append(results, interruptedToolResult(action.ToolCallID, action.Name))
	}
	return results
}

func interruptedActionResultsPreservingCompleted(actions []*PendingAction, results []protocol.ToolResult, completed map[string]struct{}) []protocol.ToolResult {
	interrupted := make([]protocol.ToolResult, 0, len(actions))
	for i, action := range actions {
		if action == nil {
			continue
		}
		if _, ok := completed[action.ToolCallID]; ok && i < len(results) {
			interrupted = append(interrupted, results[i])
			continue
		}
		interrupted = append(interrupted, interruptedToolResult(action.ToolCallID, action.Name))
	}
	return interrupted
}

func (a *Agent) beginToolRun(ctx context.Context) (context.Context, *activeToolRun, bool) {
	toolCtx, cancel := context.WithCancel(ctx)
	run := &activeToolRun{cancel: cancel, completed: make(map[string]struct{})}
	a.mu.Lock()
	if len(a.pendingTurnUser) > 0 {
		a.mu.Unlock()
		cancel()
		return toolCtx, run, false
	}
	a.activeToolRun = run
	a.mu.Unlock()
	return toolCtx, run, true
}

func (a *Agent) endToolRun(run *activeToolRun) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeToolRun == run {
		a.activeToolRun = nil
	}
	return run.interrupted
}

func (a *Agent) canStartToolAction(run *activeToolRun) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.activeToolRun == run && !run.interrupted
}

func (a *Agent) completeToolAction(run *activeToolRun, action *PendingAction, result protocol.ToolResult) {
	if action == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeToolRun == run && (!run.interrupted || !result.IsError) {
		run.completed[action.ToolCallID] = struct{}{}
	}
}

// ApproveAndExecutePendingActions executes all pending tool calls and continues.
func (a *Agent) ApproveAndExecutePendingActions(ctx context.Context) (<-chan protocol.Event, error) {
	// Overlay the saved turn's trace identity onto the caller's ctx
	// so middleware-emitted spans and the follow-up generation nest
	// under the original trace, while keeping the caller's
	// cancellation/deadline (the approval ctx, not the original Run ctx).
	a.mu.RLock()
	if a.state != StatePaused || len(a.pendingActions) == 0 {
		a.mu.RUnlock()
		return nil, fmt.Errorf("agent is not paused")
	}
	saved := a.activeTraceCtx
	actions := make([]*PendingAction, len(a.pendingActions))
	copy(actions, a.pendingActions)
	deferred := make([]*PendingAction, len(a.deferredActions))
	copy(deferred, a.deferredActions)
	a.mu.RUnlock()
	if saved != nil {
		ctx = a.observer.ResumeTrace(ctx, saved)
	}
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

	resolvedActions := orderedActions(append(deferred, actions...))
	a.mu.RLock()
	exec := a.executor
	a.mu.RUnlock()
	if err := a.ensureSnapshotForActions(ctx, exec, resolvedActions); err != nil {
		return nil, err
	}
	return a.executeResolvedPendingActions(ctx, resolvedActions, false)
}

func (a *Agent) submitToolOutputs(ctx context.Context, outputs []protocol.ToolResult, mergeDeferred bool, toolsExecuted bool) (<-chan protocol.Event, error) {
	a.mu.Lock()
	if a.state != StatePaused {
		a.mu.Unlock()
		return nil, fmt.Errorf("agent is not paused")
	}
	// Overlay saved trace identity onto the caller's ctx — the TUI
	// denial path calls us without going through ApproveAndExecute,
	// so it needs the same ctx threading.
	saved := a.activeTraceCtx
	deferred := make([]*PendingAction, len(a.deferredActions))
	copy(deferred, a.deferredActions)
	if mergeDeferred && len(deferred) > 0 {
		outputs = mergeDeferredSkippedResults(deferred, outputs, "another tool call in the batch was denied")
	}
	msg := protocol.NewToolMessage(outputs)
	if err := a.conversation.appendMessages(msg); err != nil {
		if toolsExecuted {
			a.markDurabilityFailedLocked()
		}
		a.mu.Unlock()
		return nil, fmt.Errorf("record tool outputs: %w", err)
	}
	a.pendingActions = nil
	a.deferredActions = nil
	a.state = StateRunning
	a.mu.Unlock()
	if saved != nil {
		ctx = a.observer.ResumeTrace(ctx, saved)
	}

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

func (a *Agent) ResolvePendingActions(ctx context.Context, decisions map[string]protocol.ToolResult) (<-chan protocol.Event, error) {
	a.mu.RLock()
	pending := append([]*PendingAction(nil), a.pendingActions...)
	deferred := append([]*PendingAction(nil), a.deferredActions...)
	a.mu.RUnlock()
	if len(pending) == 0 {
		return nil, fmt.Errorf("agent is not paused")
	}

	var approved []*PendingAction
	var denied []protocol.ToolResult
	deniedReasons := make([]string, 0)
	for _, action := range pending {
		result, ok := decisions[action.ToolCallID]
		if !ok || result.Type != protocol.ToolResultTypeExecutionDenied {
			approved = append(approved, action)
			continue
		}
		if result.ToolName == "" {
			result.ToolName = action.Name
		}
		if result.Reason == "" {
			result.Reason = "User denied permission"
		}
		result.IsError = true
		denied = append(denied, result)
		deniedReasons = append(deniedReasons, result.Reason)
	}

	if len(denied) > 0 {
		reason := commonDenialReason(deniedReasons)
		for _, action := range approved {
			denied = append(denied, skippedToolResult(action, reason))
		}
		outputs := mergeDeferredSkippedResults(deferred, denied, reason)
		outputs = sortToolResultsByActionOrder(outputs, append(deferred, pending...))
		innerCh, err := a.submitToolOutputs(ctx, outputs, false, false)
		if err != nil {
			return nil, err
		}
		return prependToolResultEvents(outputs, innerCh), nil
	}

	return a.executeResolvedPendingActions(ctx, orderedActions(append(deferred, approved...)), true)
}

func (a *Agent) executeActions(ctx context.Context, actions []*PendingAction, eventSink tools.EventSink, ensureSnapshot bool) (results []protocol.ToolResult, mayHaveExecuted bool, err error) {
	var executionStarted atomic.Bool
	toolCtx, toolRun, execute := a.beginToolRun(ctx)
	if !execute {
		return interruptedActionResults(actions), false, nil
	}
	defer func() {
		mayHaveExecuted = executionStarted.Load()
		interrupted := a.endToolRun(toolRun)
		toolRun.cancel()
		if interrupted {
			results = interruptedActionResultsPreservingCompleted(actions, results, toolRun.completed)
			err = nil
		}
	}()
	ctx = toolCtx

	a.mu.RLock()
	exec := a.executor
	a.mu.RUnlock()

	if ensureSnapshot {
		if err := a.ensureSnapshotForActions(ctx, exec, actions); err != nil {
			return nil, false, err
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	results = make([]protocol.ToolResult, len(actions))
	for start := 0; start < len(actions); {
		if !a.canStartToolAction(toolRun) {
			break
		}
		if !canRunActionConcurrently(actions[start]) {
			results[start] = a.executePendingAction(ctx, exec, actions[start], a.recordLoopWarning(actions[start]), eventSink, func() { executionStarted.Store(true) })
			a.completeToolAction(toolRun, actions[start], results[start])
			start++
			continue
		}

		end := start + 1
		for end < len(actions) && canRunActionConcurrently(actions[end]) {
			end++
		}
		warnings := make([]string, end-start)
		for i, action := range actions[start:end] {
			warnings[i] = a.recordLoopWarning(action)
		}
		a.executePendingActionGroup(ctx, exec, actions[start:end], warnings, results[start:end], eventSink, func() { executionStarted.Store(true) }, func(action *PendingAction, result protocol.ToolResult) {
			a.completeToolAction(toolRun, action, result)
		})
		start = end
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

	return results, executionStarted.Load(), nil
}

func (a *Agent) executeResolvedPendingActions(ctx context.Context, actions []*PendingAction, ensureSnapshot bool) (<-chan protocol.Event, error) {
	outCh := make(chan protocol.Event, len(actions)+100)
	a.mu.Lock()
	a.beginRuntimeActivityLocked()
	a.mu.Unlock()
	go func() {
		defer close(outCh)
		defer a.endRuntimeActivity()
		results, mayHaveExecuted, err := a.executeActions(ctx, actions, func(event protocol.Event) {
			outCh <- event
		}, ensureSnapshot)
		if err != nil {
			outCh <- protocol.NewErrorEvent(err)
			return
		}
		innerCh, err := a.submitToolOutputs(ctx, results, true, mayHaveExecuted)
		if err != nil {
			outCh <- protocol.NewErrorEvent(err)
			return
		}
		for i := range results {
			outCh <- protocol.Event{Type: protocol.EventTypeToolResultDone, ToolResult: &results[i]}
		}
		for event := range innerCh {
			outCh <- event
		}
	}()
	return outCh, nil
}

func prependToolResultEvents(results []protocol.ToolResult, innerCh <-chan protocol.Event) <-chan protocol.Event {
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
	return outCh
}

func canRunActionConcurrently(action *PendingAction) bool {
	if action == nil {
		return false
	}
	if action.Decision.RequiresSnapshot {
		return false
	}
	if action.Decision.Decision == ExecutionDecisionAutoExecute &&
		action.Decision.RiskLevel == tools.LevelSafe.String() &&
		!action.Decision.RequiresSnapshot {
		return true
	}
	return isApprovedReadOnlySubagent(action)
}

func isApprovedReadOnlySubagent(action *PendingAction) bool {
	if action.Decision.Decision != ExecutionDecisionRequireApproval || action.Name != "spawn" {
		return false
	}
	var args struct {
		AgentType string `json:"agent_type"`
	}
	if err := json.Unmarshal([]byte(action.Arguments), &args); err != nil {
		return false
	}
	return args.AgentType == "explore"
}

func orderedActions(actions []*PendingAction) []*PendingAction {
	ordered := append([]*PendingAction(nil), actions...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Order < ordered[j].Order
	})
	return ordered
}

// mergeDeferredSkippedResults gives every deferred action that has no result in
// outputs a skipped (or interrupted) one, and returns all results in call
// order. A deferred action that ran with the approved batch already has its
// result in outputs and must not get a second one: providers reject a tool
// call with two results.
func mergeDeferredSkippedResults(deferred []*PendingAction, outputs []protocol.ToolResult, reason string) []protocol.ToolResult {
	answered := make(map[string]struct{}, len(outputs))
	for _, output := range outputs {
		answered[output.ToolCallID] = struct{}{}
	}
	unanswered := make([]*PendingAction, 0, len(deferred))
	for _, action := range deferred {
		if _, ok := answered[action.ToolCallID]; !ok {
			unanswered = append(unanswered, action)
		}
	}

	results := make([]protocol.ToolResult, 0, len(unanswered)+len(outputs))
	if reason == "interrupted_by_steering" {
		results = append(results, interruptedActionResults(unanswered)...)
	} else {
		for _, action := range unanswered {
			results = append(results, skippedToolResult(action, reason))
		}
	}
	results = append(results, outputs...)

	orderByID := make(map[string]int, len(deferred)+len(outputs))
	for _, action := range deferred {
		orderByID[action.ToolCallID] = action.Order
	}
	for i, output := range outputs {
		if _, ok := orderByID[output.ToolCallID]; !ok {
			orderByID[output.ToolCallID] = len(deferred) + i
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		return orderByID[results[i].ToolCallID] < orderByID[results[j].ToolCallID]
	})
	return results
}

func skippedToolResult(action *PendingAction, reason string) protocol.ToolResult {
	if reason == "" {
		reason = "another tool call in the batch was denied"
	}
	return protocol.ToolResult{
		ToolCallID: action.ToolCallID,
		ToolName:   action.Name,
		Type:       protocol.ToolResultTypeExecutionDenied,
		Reason:     "Skipped because " + reason,
		IsError:    true,
	}
}

func sortToolResultsByActionOrder(results []protocol.ToolResult, actions []*PendingAction) []protocol.ToolResult {
	orderByID := make(map[string]int, len(actions))
	for _, action := range actions {
		orderByID[action.ToolCallID] = action.Order
	}
	ordered := append([]protocol.ToolResult(nil), results...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return orderByID[ordered[i].ToolCallID] < orderByID[ordered[j].ToolCallID]
	})
	return ordered
}

func commonDenialReason(reasons []string) string {
	for _, reason := range reasons {
		reason = strings.TrimSpace(reason)
		if reason != "" {
			return reason
		}
	}
	return "another tool call in the batch was denied"
}

func (a *Agent) executePendingActionGroup(ctx context.Context, exec executor.Executor, actions []*PendingAction, warnings []string, results []protocol.ToolResult, eventSink tools.EventSink, started func(), completed func(*PendingAction, protocol.ToolResult)) {
	var wg sync.WaitGroup
	for i, action := range actions {
		i, action := i, action
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = a.executePendingAction(ctx, exec, action, warnings[i], eventSink, started)
			completed(action, results[i])
		}()
	}
	wg.Wait()
}

func (a *Agent) recordLoopWarning(action *PendingAction) string {
	// A loop-detector warning is prefixed to the real tool result (rather than
	// appended as a separate ToolResult) so the tool call ID stays unique within
	// the tool-role message we emit.
	if a.loopDetector == nil {
		return ""
	}
	status := a.loopDetector.RecordCall(action.Name, action.Arguments)
	if !status.IsLooping {
		return ""
	}
	return fmt.Sprintf(
		"<system-reminder>\nLoop detected: %s\nSuggestion: %s\nProceeding with execution anyway.\n</system-reminder>\n\n",
		status.Pattern,
		status.Suggestion,
	)
}

func (a *Agent) executePendingAction(ctx context.Context, exec executor.Executor, action *PendingAction, loopWarning string, eventSink tools.EventSink, started func()) protocol.ToolResult {
	prefixed := func(text string) string {
		if loopWarning == "" {
			return text
		}
		return loopWarning + text
	}

	if action.Decision.Decision == ExecutionDecisionBlock {
		return protocol.ToolResult{
			ToolCallID: action.ToolCallID,
			ToolName:   action.Name,
			Type:       protocol.ToolResultTypeExecutionDenied,
			Reason:     action.Decision.Reason,
			IsError:    true,
		}
	}

	tool, ok := a.tools[action.Name]
	if !ok {
		tr := protocol.NewTextToolResult(action.ToolCallID, prefixed(fmt.Sprintf("Error: Tool %s not found", action.Name)), true)
		tr.ToolName = action.Name
		return tr
	}
	call := &ToolCall{
		ToolCallID: action.ToolCallID,
		Tool:       tool,
		Name:       action.Name,
		Arguments:  action.Arguments,
		Executor:   exec,
	}
	if a.toolExecutionRecorder != nil {
		if err := a.toolExecutionRecorder.RecordToolExecutionStarted(ctx, call); err != nil {
			tr := protocol.NewTextToolResult(action.ToolCallID, prefixed(fmt.Sprintf("Error: record durable tool execution start: %v", err)), true)
			tr.ToolName = action.Name
			return tr
		}
	}
	started()
	ctx = tools.WithEventSink(ctx, eventSink)
	ctx = tools.WithCallInfo(ctx, tools.CallInfo{ID: call.ToolCallID, Name: call.Name})
	invoke := buildMiddlewareChain(a.middlewares, func(ctx context.Context, c *ToolCall) (any, error) {
		return c.Tool.Execute(ctx, c.Executor, c.Arguments)
	})
	res, err := invoke(ctx, call)
	if err != nil {
		if reason, denied := toolExecutionDenialReason(err); denied {
			return protocol.ToolResult{
				ToolCallID: action.ToolCallID,
				ToolName:   action.Name,
				Type:       protocol.ToolResultTypeExecutionDenied,
				Reason:     reason,
				IsError:    true,
			}
		}
		tr := protocol.NewTextToolResult(action.ToolCallID, prefixed(fmt.Sprintf("Error: %v", err)), true)
		tr.ToolName = action.Name
		return tr
	}
	if tr, ok := res.(protocol.ToolResult); ok {
		// The dispatcher owns call identity, even when a tool returns rich content.
		tr.ToolCallID = action.ToolCallID
		tr.ToolName = action.Name
		if loopWarning != "" {
			if tr.Type == protocol.ToolResultTypeContent {
				tr.Content = append([]protocol.ToolResultContentPart{{Type: protocol.ContentTypeText, Text: loopWarning}}, tr.Content...)
			} else {
				tr.Text = prefixed(tr.Text)
			}
		}
		return tr
	}
	tr := protocol.NewTextToolResult(action.ToolCallID, prefixed(formatToolOutput(res)), false)
	tr.ToolName = action.Name
	tr.Metadata = toolResultMetadata(res)
	return tr
}

func (a *Agent) markDurabilityFailedLocked() {
	a.pendingActions = nil
	a.deferredActions = nil
	a.state = StateDurabilityFailed
}

// formatToolOutput converts a tool's return value to a readable string.
// Slices are joined with newlines instead of Go's default bracket format.
func formatToolOutput(v any) string {
	switch val := v.(type) {
	case ToolOutputWithMetadata:
		return val.ToolOutputText()
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

func toolResultMetadata(v any) map[string]any {
	withMetadata, ok := v.(ToolOutputWithMetadata)
	if !ok {
		return nil
	}
	metadata := withMetadata.ToolResultMetadata()
	if len(metadata) == 0 {
		return nil
	}
	return metadata
}

func usageFromLastToolMessage(history []protocol.Message) protocol.Usage {
	if len(history) == 0 || history[len(history)-1].Role != protocol.RoleTool {
		return protocol.Usage{}
	}
	var total protocol.Usage
	for _, part := range history[len(history)-1].Content {
		if part.Type != protocol.ContentTypeToolResult || part.ToolResult == nil {
			continue
		}
		total = total.Add(usageFromToolResultMetadata(part.ToolResult.Metadata))
	}
	return total.Normalized()
}

func usageFromToolResultMetadata(metadata map[string]any) protocol.Usage {
	if len(metadata) == 0 {
		return protocol.Usage{}
	}
	switch usage := metadata["usage"].(type) {
	case protocol.Usage:
		return usage.Normalized()
	case *protocol.Usage:
		if usage == nil {
			return protocol.Usage{}
		}
		return usage.Normalized()
	case map[string]any:
		return usageFromMap(usage).Normalized()
	case map[string]int:
		return protocol.Usage{
			InputTokens:              usage["input_tokens"],
			OutputTokens:             usage["output_tokens"],
			TotalTokens:              usage["total_tokens"],
			CacheCreationInputTokens: usage["cache_creation_input_tokens"],
			CacheReadInputTokens:     usage["cache_read_input_tokens"],
		}.Normalized()
	default:
		return protocol.Usage{}
	}
}

func usageFromMap(values map[string]any) protocol.Usage {
	return protocol.Usage{
		InputTokens:              intFromAny(values["input_tokens"]),
		OutputTokens:             intFromAny(values["output_tokens"]),
		TotalTokens:              intFromAny(values["total_tokens"]),
		CacheCreationInputTokens: intFromAny(values["cache_creation_input_tokens"]),
		CacheReadInputTokens:     intFromAny(values["cache_read_input_tokens"]),
	}
}

func intFromAny(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		if value, err := n.Int64(); err == nil {
			return int(value)
		}
		if value, err := n.Float64(); err == nil {
			return int(value)
		}
	default:
		return 0
	}
	return 0
}

func isZeroUsage(usage protocol.Usage) bool {
	return usage.InputTokens == 0 &&
		usage.OutputTokens == 0 &&
		usage.TotalTokens == 0 &&
		usage.CacheCreationInputTokens == 0 &&
		usage.CacheReadInputTokens == 0
}
