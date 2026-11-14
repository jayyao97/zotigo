package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jayyao97/zotigo/core/agent/prompt"
	"github.com/jayyao97/zotigo/core/compaction"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

// GetContextStats returns statistics about the current context.
func (a *Agent) GetContextStats() map[string]int {
	a.mu.RLock()
	defer a.mu.RUnlock()

	stats := make(map[string]int)
	history := a.conversation.history
	stats["message_count"] = len(history)
	if a.compressor != nil {
		stats["estimated_tokens"] = a.contextTokensLocked()
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
func (a *Agent) ForceCompress(ctx context.Context) (*compaction.CompressionResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.compressor == nil {
		return nil, fmt.Errorf("compressor not initialized")
	}

	if a.runActive || a.state == StatePaused {
		return nil, fmt.Errorf("cannot compact while the agent is working or awaiting approval")
	}
	result, err := a.compressHistoryLocked(ctx, true)
	if err != nil {
		return nil, err
	}

	return &result, nil
}

func (a *Agent) buildContext(ctx context.Context) ([]protocol.Message, protocol.ContextInput, *protocol.ContextCompaction, error) {
	msgs := a.systemPromptMessages()
	contextInput := a.contextPromptFor(msgs)
	var compacted *protocol.ContextCompaction
	history := a.projectPromptHistory(a.conversation.history)

	// The compression path owns the single calibrated threshold check; an outer
	// local-only gate would suppress valid API-driven compaction.
	if a.compressor != nil {
		result, err := a.compressHistoryWithPromptLocked(ctx, false, false, contextInput, msgs)
		if err != nil {
			return nil, contextInput, nil, fmt.Errorf("compact context: %w", err)
		}
		if err == nil && result.Compressed {
			history = a.projectPromptHistory(a.conversation.history)
			compacted = &protocol.ContextCompaction{
				OriginalTokens:   result.OriginalTokens,
				CompressedTokens: result.CompressedTokens,
				MessagesBefore:   result.MessagesBefore,
				MessagesAfter:    result.MessagesAfter,
			}
		}
	}

	msgs = append(msgs, history...)
	contextInput.HistoryDigest = compaction.ContextDigest(history)
	return msgs, contextInput, compacted, nil
}

func (a *Agent) systemPromptMessages() []protocol.Message {
	var messages []protocol.Message
	if a.promptBuilder == nil {
		return messages
	}
	for _, text := range a.promptBuilder.BuildMessages(a.promptContext()) {
		messages = append(messages, protocol.NewSystemMessage(text))
	}
	return messages
}

func (a *Agent) projectPromptHistory(history []protocol.Message) []protocol.Message {
	if a.compressor == nil {
		return history
	}
	return a.compressor.ProjectPrompt(history, 2000)
}

func (a *Agent) compressHistoryLocked(
	ctx context.Context,
	force bool,
) (compaction.CompressionResult, error) {
	system := a.systemPromptMessages()
	return a.compressHistoryWithPromptLocked(ctx, force, false, a.contextPromptFor(system), system)
}

func sortedToolList(toolMap map[string]tools.Tool) []tools.Tool {
	var toolList []tools.Tool
	for _, tool := range toolMap {
		toolList = append(toolList, tool)
	}
	sort.Slice(toolList, func(i, j int) bool { return toolList[i].Name() < toolList[j].Name() })
	return toolList
}

func (a *Agent) compressHistoryWithPromptLocked(
	ctx context.Context,
	force bool,
	reactive bool,
	prompt protocol.ContextInput,
	system []protocol.Message,
) (compaction.CompressionResult, error) {
	// Match the UI usage path: ContextTokens owns prompt projection.
	tokens, _ := a.compressor.ContextTokens(a.conversation.history, prompt)
	history := a.projectPromptHistory(a.conversation.history)
	if !force && !a.compressor.NeedsCompressionTokens(tokens) {
		return compaction.CompressionResult{
			OriginalTokens: tokens, CompressedTokens: tokens,
			MessagesBefore: len(history), MessagesAfter: len(history),
		}, nil
	}

	var (
		compressed []protocol.Message
		result     compaction.CompressionResult
		err        error
	)
	a.compacting.Store(true)
	defer a.compacting.Store(false)
	if !reactive && a.provider != nil && a.compressorSummarizer == nil {
		messages := append(append([]protocol.Message(nil), system...), history...)
		toolList := sortedToolList(a.tools)
		summarizer := compaction.NewProviderSummarizer(a.provider, compaction.WithSummarizerObserver(a.observer, a.cfg.Model))
		compressed, result, err = a.compressor.ForceCompressWithSummary(ctx, history, func(ctx context.Context) (string, error) {
			return summarizer.SummarizePrompt(ctx, messages, toolList)
		})
		// An appended summary instruction can cross the provider's limit even
		// before the next main request does. Only length errors use the smaller
		// flattened request; cancellation and incomplete summaries must fail.
		if errors.Is(err, providers.ErrContextLengthExceeded) {
			compressed, result, err = a.compressor.ForceCompress(ctx, history)
		}
	} else {
		compressed, result, err = a.compressor.ForceCompress(ctx, history)
	}
	if err != nil || !result.Compressed {
		return result, err
	}
	result.OriginalTokens = tokens

	rebased, state, err := a.rebaseUserContext(compressed)
	if err != nil {
		return compaction.CompressionResult{}, err
	}
	result.MessagesAfter = len(rebased)
	result.CompressedTokens, _ = a.compressor.ContextTokens(rebased, prompt)
	// Compare local-to-local to verify that rewriting actually reduced input;
	// a provider-calibrated "before" and local "after" are not comparable.
	if a.compressor.CountTokens(a.projectPromptHistory(rebased)) >= a.compressor.CountTokens(history) {
		result.Compressed = false
		result.CompressedTokens = result.OriginalTokens
		result.MessagesAfter = result.MessagesBefore
		return result, nil
	}
	if err := a.conversation.replace(rebased, state); err != nil {
		return compaction.CompressionResult{}, fmt.Errorf("record compressed history: %w", err)
	}
	return result, nil
}

func (a *Agent) rebaseUserContext(
	history []protocol.Message,
) ([]protocol.Message, *prompt.UserContextState, error) {
	var contextText string
	state := a.conversation.userContextState.Clone()
	if a.userContext != nil {
		var err error
		var builtState prompt.UserContextState
		contextText, builtState, err = a.userContext.BuildFull(a.promptContext())
		if err != nil {
			return nil, nil, fmt.Errorf("build user context after compaction: %w", err)
		}
		state = builtState.Clone()
	}

	withoutContext := make([]protocol.Message, 0, len(history)+1)
	insertAt := 0
	foundSummary := false
	for _, msg := range history {
		if msg.IsContextualUser() {
			continue
		}
		withoutContext = append(withoutContext, msg)
		// Successful compression always rebuilds history as leading system
		// messages, one summary, then the preserved tail. Locate the summary by
		// that structural position so user-authored marker text cannot move the
		// rebased context later in history.
		if !foundSummary && msg.Role != protocol.RoleSystem {
			insertAt = len(withoutContext)
			foundSummary = true
		}
	}
	contextMessages := make([]protocol.Message, 0, 2)
	if contextText != "" {
		contextMessages = append(contextMessages, protocol.NewContextualUserMessage(contextText))
	}
	if requestContext := a.turnSafety.RequestContext; requestContext != nil {
		contextMessages = append(contextMessages, protocol.NewContextualUserMessage(requestContext.PromptText()))
	}
	if len(contextMessages) > 0 {
		withoutContext = append(withoutContext, make([]protocol.Message, len(contextMessages))...)
		copy(withoutContext[insertAt+len(contextMessages):], withoutContext[insertAt:len(withoutContext)-len(contextMessages)])
		copy(withoutContext[insertAt:], contextMessages)
	}
	return withoutContext, state, nil
}
