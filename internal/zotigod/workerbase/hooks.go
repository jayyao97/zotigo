package workerbase

import (
	"context"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/internal/hooks"
)

func DispatchTurnStartHook(dispatcher HookDispatcher, sessionID string, agentName string, cwd string, turnID string, model string) {
	if dispatcher == nil {
		return
	}
	event := hooks.NewEvent(hooks.TurnStart, sessionID, agentName, cwd)
	event.TurnID = turnID
	event.Turn = &hooks.TurnPayload{Status: "running", Model: model}
	dispatcher.Dispatch(context.Background(), event)
}

func DispatchUserPromptSubmitHook(dispatcher HookDispatcher, sessionID string, agentName string, cwd string, turnID string, text string) {
	if dispatcher == nil {
		return
	}
	event := hooks.NewEvent(hooks.UserPromptSubmit, sessionID, agentName, cwd)
	event.TurnID = turnID
	event.Prompt = &hooks.PromptPayload{Text: text}
	dispatcher.Dispatch(context.Background(), event)
}

func DispatchTurnEndHook(dispatcher HookDispatcher, sessionID string, agentName string, cwd string, turnID string, status string, model string, usage protocol.Usage) {
	if dispatcher == nil {
		return
	}
	event := hooks.NewEvent(hooks.TurnEnd, sessionID, agentName, cwd)
	event.TurnID = turnID
	event.Turn = &hooks.TurnPayload{Status: status, Model: model, Usage: UsagePayload(usage)}
	dispatcher.Dispatch(context.Background(), event)
}

func TurnUsageDelta(before protocol.Usage, after protocol.Usage) protocol.Usage {
	before = before.Normalized()
	after = after.Normalized()
	return protocol.Usage{
		InputTokens:              nonNegativeUsageDelta(before.InputTokens, after.InputTokens),
		OutputTokens:             nonNegativeUsageDelta(before.OutputTokens, after.OutputTokens),
		TotalTokens:              nonNegativeUsageDelta(before.TotalTokens, after.TotalTokens),
		CacheCreationInputTokens: nonNegativeUsageDelta(before.CacheCreationInputTokens, after.CacheCreationInputTokens),
		CacheReadInputTokens:     nonNegativeUsageDelta(before.CacheReadInputTokens, after.CacheReadInputTokens),
	}
}

func nonNegativeUsageDelta(before int, after int) int {
	if after <= before {
		return 0
	}
	return after - before
}

type HookDispatcher interface {
	Dispatch(context.Context, hooks.Event) hooks.DispatchResult
}

func UsagePayload(usage protocol.Usage) *hooks.UsagePayload {
	usage = usage.Normalized()
	return &hooks.UsagePayload{
		InputTokens:              usage.InputTokens,
		OutputTokens:             usage.OutputTokens,
		TotalTokens:              usage.TotalTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		CacheReadInputTokens:     usage.CacheReadInputTokens,
	}
}

func CloseHookDispatcher(dispatcher *hooks.Dispatcher) {
	if dispatcher == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = dispatcher.Close(ctx)
}
