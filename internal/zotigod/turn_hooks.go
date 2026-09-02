package zotigod

import (
	"context"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/internal/hooks"
)

func dispatchTurnStartHook(dispatcher hookEventDispatcher, sessionID string, agentName string, cwd string, turnID string, model string) {
	if dispatcher == nil {
		return
	}
	event := hooks.NewEvent(hooks.TurnStart, sessionID, agentName, cwd)
	event.TurnID = turnID
	event.Turn = &hooks.TurnPayload{Status: "running", Model: model}
	dispatcher.Dispatch(context.Background(), event)
}

func dispatchUserPromptSubmitHook(dispatcher hookEventDispatcher, sessionID string, agentName string, cwd string, turnID string, text string) {
	if dispatcher == nil {
		return
	}
	event := hooks.NewEvent(hooks.UserPromptSubmit, sessionID, agentName, cwd)
	event.TurnID = turnID
	event.Prompt = &hooks.PromptPayload{Text: text}
	dispatcher.Dispatch(context.Background(), event)
}

func dispatchTurnEndHook(dispatcher hookEventDispatcher, sessionID string, agentName string, cwd string, turnID string, status string, model string, usage protocol.Usage) {
	if dispatcher == nil {
		return
	}
	event := hooks.NewEvent(hooks.TurnEnd, sessionID, agentName, cwd)
	event.TurnID = turnID
	event.Turn = &hooks.TurnPayload{Status: status, Model: model, Usage: usagePayload(usage)}
	dispatcher.Dispatch(context.Background(), event)
}

func turnUsageDelta(before protocol.Usage, after protocol.Usage) protocol.Usage {
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
