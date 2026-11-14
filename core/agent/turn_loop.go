package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jayyao97/zotigo/core/debug"
	"github.com/jayyao97/zotigo/core/observability"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
)

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
	if a.runActive {
		a.mu.Unlock()
		return nil, errors.New("agent is already running")
	}
	if a.state == StateDurabilityFailed {
		a.mu.Unlock()
		return nil, errors.New("agent cannot continue after a durability failure; restore from durable state")
	}

	if msg.Role != "" {
		if a.state == StatePaused {
			a.mu.Unlock()
			return nil, fmt.Errorf("cannot add user input while agent is paused")
		}

		if msg.Role == protocol.RoleUser {
			if err := a.appendUserInputLocked(msg, userInputStartsTurn); err != nil {
				a.mu.Unlock()
				return nil, err
			}
			a.startNewTurn(msg.DisplayString(), messageRequestContext(msg))
		} else {
			if err := a.conversation.appendMessages(msg); err != nil {
				a.mu.Unlock()
				return nil, fmt.Errorf("record history: %w", err)
			}
		}
	}

	if a.state == StatePaused && len(a.pendingActions) > 0 {
		a.mu.Unlock()
		return nil, fmt.Errorf("agent is paused waiting for tool outputs")
	}

	a.state = StateRunning
	a.runActive = true
	a.beginRuntimeActivityLocked()
	a.mu.Unlock()

	outCh := make(chan protocol.Event, 100)

	go func() {
		defer close(outCh)
		defer func() {
			a.mu.Lock()
			a.runActive = false
			a.mu.Unlock()
		}()
		defer a.endRuntimeActivity()

		// Trace lifecycle: fresh user input opens a trace; a tool-result
		// continuation overlays the saved trace identity onto the
		// caller's ctx so spans still nest under the original turn.
		// EndTurn is deferred but skipped while we're paused — the
		// turn resumes when the user approves.
		var turnErr error
		// A run can contain many model calls. Progress and terminal events
		// carry cumulative run snapshots, including calls before a failure.
		var runUsage protocol.Usage
		withUsage := func(event protocol.Event) protocol.Event {
			if !isZeroUsage(runUsage) {
				usage := runUsage.Normalized()
				event.Usage = &usage
			}
			return event
		}
		isFreshTurn := msg.Role == protocol.RoleUser
		if isFreshTurn {
			turnMeta := map[string]any{
				"working_directory": a.executor.WorkDir(),
				"platform":          a.executor.Platform(),
				"provider":          a.provider.Name(),
				"model":             a.cfg.Model,
				"approval_policy":   string(a.policy),
			}
			if a.cfg.ThinkingLevel != "" {
				turnMeta["thinking_level"] = a.cfg.ThinkingLevel
			}
			ctx = a.observer.StartTurn(ctx, msg, turnMeta)
		} else {
			a.mu.RLock()
			saved := a.activeTraceCtx
			a.mu.RUnlock()
			if saved != nil {
				ctx = a.observer.ResumeTrace(ctx, saved)
			}
		}
		defer func() {
			a.mu.RLock()
			paused := a.state == StatePaused
			a.mu.RUnlock()
			if paused {
				// Save a values-only snapshot. WithoutCancel detaches
				// from the original Run's cancellation chain so the
				// resumed work isn't pre-canceled by an aborted Run,
				// and so we don't pin the parent ctx tree until resume.
				a.mu.Lock()
				a.activeTraceCtx = context.WithoutCancel(ctx)
				a.mu.Unlock()
				return
			}
			a.observer.EndTurn(ctx, turnErr)
			a.mu.Lock()
			a.activeTraceCtx = nil
			a.mu.Unlock()
		}()

		emptyRecoveryAttempts := 0
		const maxEmptyRecoveryAttempts = 1

		// Reactive compaction: when a provider rejects a request as
		// over-context, force-compress and retry once. Cap at one retry
		// so a buggy compactor or persistent overflow can't loop.
		reactiveRetries := 0
		const maxReactiveRetries = 1
		recordInterruptedToolCalls := func(calls []*protocol.ToolCall) error {
			results := interruptedToolResults(calls)
			a.mu.Lock()
			err := a.conversation.appendMessages(protocol.NewToolMessage(results))
			a.mu.Unlock()
			if err != nil {
				return err
			}
			for i := range results {
				outCh <- protocol.Event{Type: protocol.EventTypeToolResultDone, ToolResult: &results[i]}
			}
			return nil
		}

		for {
			a.mu.RLock()
			state := a.state
			toolsMap := a.tools
			a.mu.RUnlock()

			if state == StatePaused {
				outCh <- withUsage(protocol.Event{Type: protocol.EventTypeFinish, FinishReason: "need_approval"})
				return
			}

			a.applyPendingRuntimeProfile(true)
			_, err := a.applyPendingTurnUserInput(ctx, outCh)
			if err != nil {
				turnErr = err
				return
			}
			a.mu.Lock()
			msgs, contextInput, compacted, err := a.buildContext(ctx)
			provider := a.provider
			model := a.cfg.Model
			profileName := a.profileName
			contextUsage := a.contextUsageLocked(contextInput)
			a.mu.Unlock()
			if err != nil {
				turnErr = fmt.Errorf("build context: %w", err)
				outCh <- withUsage(protocol.NewErrorEvent(turnErr))
				return
			}
			if compacted != nil {
				outCh <- protocol.Event{Type: protocol.EventTypeContextCompacted, ContextCompaction: compacted}
			}
			outCh <- protocol.Event{Type: protocol.EventTypeUsageUpdated, ContextUsage: contextUsage}
			// Prepare tools list (sorted by name for deterministic ordering,
			// which is required for Anthropic prompt caching to work)
			toolList := sortedToolList(toolsMap)

			estimatedTokens := 0
			if a.compressor != nil {
				estimatedTokens = a.compressor.CountTokens(msgs)
			}
			modelTurnStart := time.Now()
			debug.Logf(
				"agent turn start provider=%s model=%s messages=%d tools=%d est_tokens=%d",
				provider.Name(),
				model,
				len(msgs),
				len(toolList),
				estimatedTokens,
			)

			// One generation per StreamChat invocation. genCtx flows
			// into the provider so any future provider-side child spans
			// would attach correctly. End is called exactly once per
			// iteration via endGen, which guards against double-firing
			// when the iteration has multiple exit paths (success,
			// reactive recovery, terminal error).
			generationMeta := map[string]any{"profile": profileName}
			genCtx := a.observer.StartGeneration(ctx, observability.GenerationMain, model, msgs, toolList, generationMeta)
			genEnded := false
			endGen := func(out observability.GenerationOutput, usage *protocol.Usage, gerr error) {
				if genEnded {
					return
				}
				genEnded = true
				a.observer.EndGeneration(genCtx, out, usage, gerr)
			}

			stream, err := provider.StreamChat(genCtx, msgs, toolList)
			if err != nil {
				endGen(observability.GenerationOutput{}, nil, err)
				if a.tryReactiveRecover(ctx, err, &reactiveRetries, maxReactiveRetries, outCh) {
					continue
				}
				turnErr = err
				outCh <- withUsage(protocol.NewErrorEvent(err))
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
			var reactiveErr error
			var failedRetry *protocol.RequestRetry

			for evt := range stream {
				// Resolve a failed retry together with its following error: the
				// agent may hand a context-length failure to compaction instead.
				if evt.Type == protocol.EventTypeRequestRetry && evt.Retry != nil && evt.Retry.Status == "failed" {
					failedRetry = evt.Retry
					continue
				}
				if evt.Type == protocol.EventTypeFinish {
					providerFinishReason = evt.FinishReason
					providerUsage = evt.Usage
					continue
				}

				// Errors are terminal for the turn. Context-length errors
				// get intercepted (caller below force-compacts and retries
				// once); anything else is forwarded and the turn ends —
				// without this exit, an empty currentContent would trip
				// the empty-response recovery path and re-call the model.
				if evt.Type == protocol.EventTypeError && evt.Error != nil {
					if evt.Usage != nil {
						// Failed requests have no assistant history entry, but their
						// reported usage still belongs in turn events and hook snapshots.
						runUsage = runUsage.Add(*evt.Usage).Normalized()
						a.mu.Lock()
						a.cumulativeUsage = a.cumulativeUsage.Add(*evt.Usage).Normalized()
						a.mu.Unlock()
					}
					if errors.Is(evt.Error, providers.ErrContextLengthExceeded) &&
						reactiveRetries < maxReactiveRetries {
						reactiveErr = evt.Error
					} else {
						turnErr = evt.Error
					}
					if failedRetry != nil {
						retry := *failedRetry
						if reactiveErr != nil {
							retry.Status = "context_recovery"
						}
						outCh <- protocol.Event{Type: protocol.EventTypeRequestRetry, Retry: &retry}
					}
					if reactiveErr == nil {
						outCh <- withUsage(evt)
					}
					for range stream {
					}
					if reactiveErr == nil {
						return
					}
					break
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

			// Reactive recovery: a context-length error was caught
			// mid-stream. Force a compaction and restart the outer
			// loop so the same user turn gets retried once.
			if reactiveErr != nil {
				endGen(observability.GenerationOutput{}, nil, reactiveErr)
				if a.tryReactiveRecover(ctx, reactiveErr, &reactiveRetries, maxReactiveRetries, outCh) {
					continue
				}
				turnErr = reactiveErr
				outCh <- withUsage(protocol.NewErrorEvent(reactiveErr))
				return
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
			a.mu.RLock()
			toolUsage := usageFromLastToolMessage(a.conversation.history)
			a.mu.RUnlock()
			if providerUsage != nil {
				// Normalize before persisting so consumers (cost cmd, TUI
				// status line) can rely on TotalTokens being populated even
				// for providers that only emit per-component counts (e.g.
				// Anthropic, which doesn't ship a total field on the wire).
				normalized := providerUsage.Normalized()
				if asstMsg.Metadata == nil {
					asstMsg.Metadata = &protocol.MessageMetadata{}
				}
				asstMsg.Metadata.Usage = &normalized
				asstMsg.Metadata.ContextInput = &contextInput
				asstMsg.Metadata.Provider = provider.Name()
				asstMsg.Metadata.Model = model
			}
			if !isZeroUsage(toolUsage) {
				normalized := toolUsage.Normalized()
				if asstMsg.Metadata == nil {
					asstMsg.Metadata = &protocol.MessageMetadata{}
				}
				asstMsg.Metadata.ToolUsage = &normalized
			}

			// Generation completed normally — close it with the assembled
			// output. Reasoning text is flattened from all blocks (we already
			// have them in reasoningBlocks); tool calls are dereferenced into
			// the flat shape Langfuse expects.
			{
				var reasoningText string
				for _, rb := range reasoningBlocks {
					reasoningText += rb.Text
				}
				calls := make([]protocol.ToolCall, 0, len(currentToolCalls))
				for _, tc := range currentToolCalls {
					calls = append(calls, *tc)
				}
				endGen(observability.GenerationOutput{
					Text:         currentContent,
					Reasoning:    reasoningText,
					ToolCalls:    calls,
					FinishReason: providerFinishReason,
				}, providerUsage, nil)
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
			err = a.conversation.appendMessages(asstMsg)
			if err == nil {
				usage := protocol.SessionUsage([]protocol.Message{asstMsg})
				a.cumulativeUsage = a.cumulativeUsage.Add(usage).Normalized()
				runUsage = runUsage.Add(usage).Normalized()
			}
			steeringPending := len(a.pendingTurnUser) > 0
			contextUsage = a.contextUsageLocked(contextInput)
			a.mu.Unlock()
			if err != nil {
				turnErr = fmt.Errorf("record assistant history: %w", err)
				outCh <- withUsage(protocol.NewErrorEvent(turnErr))
				return
			}

			outCh <- withUsage(protocol.Event{Type: protocol.EventTypeUsageUpdated, ContextUsage: contextUsage})

			// Reset the empty-recovery budget on any legitimate output.
			// The cap is meant to stop consecutive empty responses, not to
			// limit total recoveries within a long multi-step turn.
			if currentContent != "" || len(currentToolCalls) > 0 {
				emptyRecoveryAttempts = 0
			}

			if steeringPending {
				if len(currentToolCalls) > 0 {
					if err := recordInterruptedToolCalls(currentToolCalls); err != nil {
						turnErr = fmt.Errorf("record interrupted tool results: %w", err)
						outCh <- withUsage(protocol.NewErrorEvent(turnErr))
						return
					}
				}
				continue
			}

			// Handle tool calls
			if len(currentToolCalls) > 0 {
				var autoCalls []*PendingAction
				var approvalCalls []*PendingAction
				var blockedCalls []*PendingAction

				for i, tc := range currentToolCalls {
					decision := a.classifyToolCall(ctx, tc)
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
						Order:      i,
						ToolCall:   tc,
					}
					switch decision.Decision {
					case ExecutionDecisionAutoExecute:
						autoCalls = append(autoCalls, action)
					case ExecutionDecisionRequireApproval:
						approvalCalls = append(approvalCalls, action)
					case ExecutionDecisionBlock:
						blockedCalls = append(blockedCalls, action)
					}
				}

				if len(approvalCalls) > 0 {
					a.mu.Lock()
					if len(a.pendingTurnUser) > 0 {
						a.mu.Unlock()
						if err := recordInterruptedToolCalls(currentToolCalls); err != nil {
							turnErr = fmt.Errorf("record interrupted tool results: %w", err)
							outCh <- withUsage(protocol.NewErrorEvent(turnErr))
							return
						}
						continue
					}
					a.state = StatePaused
					a.pendingActions = append(a.pendingActions, approvalCalls...)
					a.deferredActions = append(a.deferredActions, autoCalls...)
					a.deferredActions = append(a.deferredActions, blockedCalls...)
					a.mu.Unlock()
					continue
				}

				executableCalls := orderedActions(append(autoCalls, blockedCalls...))
				if len(executableCalls) > 0 {
					results, mayHaveExecuted, err := a.executeActions(ctx, executableCalls, func(event protocol.Event) {
						outCh <- event
					}, true)
					if err != nil {
						turnErr = err
						outCh <- protocol.NewErrorEvent(err)
						return
					}
					toolMsg := protocol.NewToolMessage(results)
					a.mu.Lock()
					err = a.conversation.appendMessages(toolMsg)
					if err != nil && mayHaveExecuted {
						a.markDurabilityFailedLocked()
					}
					a.mu.Unlock()
					if err != nil {
						turnErr = fmt.Errorf("record tool results: %w", err)
						outCh <- withUsage(protocol.NewErrorEvent(turnErr))
						return
					}
					for i := range results {
						outCh <- protocol.Event{Type: protocol.EventTypeToolResultDone, ToolResult: &results[i]}
					}
				}

				continue
			}

			drained, err := a.applyPendingTurnUserInput(ctx, outCh)
			if err != nil {
				turnErr = err
				return
			}
			if drained {
				continue
			}

			// Recover from empty responses: the model emitted no text and no
			// tool calls (all output_tokens went into reasoning). Without this
			// guard the turn would end silently mid-task. Nudge the model once
			// with a system-reminder and retry; if that also comes back empty,
			// fall through to the normal end-of-turn.
			if currentContent == "" && len(currentToolCalls) == 0 &&
				emptyRecoveryAttempts < maxEmptyRecoveryAttempts {
				a.mu.Lock()
				if len(a.pendingTurnUser) > 0 {
					a.mu.Unlock()
					continue
				}
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
				err = a.conversation.appendMessages(nudge)
				a.mu.Unlock()
				if err != nil {
					turnErr = fmt.Errorf("record recovery nudge: %w", err)
					outCh <- withUsage(protocol.NewErrorEvent(turnErr))
					return
				}
				continue
			}

			// No tool calls -> turn finished
			drained, err = a.applyPendingTurnUserInput(ctx, outCh)
			if err != nil {
				turnErr = err
				return
			}
			if drained {
				continue
			}
			a.mu.Lock()
			a.pendingTurnUser = nil
			a.state = StateIdle
			a.mu.Unlock()

			outCh <- withUsage(protocol.NewFinishEvent(providerFinishReason))
			return
		}
	}()

	return outCh, nil
}

// tryReactiveRecover handles a provider context-length error by
// force-compacting history and signalling the caller to retry. Returns
// true if recovery was attempted (caller should `continue` the turn
// loop), false if the error isn't ours to handle or recovery itself
// failed (caller should bubble it up).
//
// Increments *retries on success. Emits a transcript-visible notice so
// users see why their turn paused, but the error itself is swallowed —
// the retry presents as a normal turn.
func (a *Agent) tryReactiveRecover(ctx context.Context, err error, retries *int, max int, outCh chan<- protocol.Event) bool {
	if a.compressor == nil || *retries >= max {
		return false
	}
	if !errors.Is(err, providers.ErrContextLengthExceeded) {
		return false
	}

	a.mu.Lock()
	system := a.systemPromptMessages()
	result, cerr := a.compressHistoryWithPromptLocked(ctx, true, true, a.contextPromptFor(system), system)
	a.mu.Unlock()

	if cerr != nil || !result.Compressed {
		debug.Logf("agent reactive recover failed: err=%v compressed=%v", cerr, result.Compressed)
		return false
	}

	*retries++
	debug.Logf("agent reactive recover ok: %d→%d tokens", result.OriginalTokens, result.CompressedTokens)
	outCh <- protocol.Event{
		Type: protocol.EventTypeContextCompacted,
		ContextCompaction: &protocol.ContextCompaction{
			OriginalTokens:   result.OriginalTokens,
			CompressedTokens: result.CompressedTokens,
			MessagesBefore:   result.MessagesBefore,
			MessagesAfter:    result.MessagesAfter,
		},
	}
	outCh <- protocol.Event{
		Type: protocol.EventTypeContentDelta,
		ContentPartDelta: &protocol.ContentPartDelta{
			Type: protocol.ContentTypeReasoning,
			Text: fmt.Sprintf("[context exceeded — auto-compacted %d→%d tokens, retrying]\n",
				result.OriginalTokens, result.CompressedTokens),
		},
	}
	return true
}
