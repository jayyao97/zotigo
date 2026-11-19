package zotigod

import (
	"context"
	"sync"

	"github.com/jayyao97/zotigo/core/compaction"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
)

func (h *handler) decorateSession(ctx context.Context, session Session, stored *zotigosession.Session) Session {
	if stored != nil {
		session.ForkedFrom = stored.ForkedFrom
		session.Agent = stored.Agent
		if stored.Agent == string(zotigoruntime.AgentCodex) {
			session.ProfileName = ""
		} else {
			session.ProfileName = stored.ProfileName
		}
		session.Model = stored.Model
		session.ReasoningEffort = stored.ReasoningEffort
		session.ApprovalPolicy = stored.ApprovalPolicy
		session.ChannelToolsVersion = stored.Capabilities.ChannelToolsVersion
		session.SessionToolsVersion = stored.Capabilities.SessionToolsVersion
		session.ChannelToolsEligible = channelToolsEligible(stored.Metadata)
	}
	session.ContextUsage = unavailableSessionContextUsage()
	items, _, err := h.items.LoadItems(ctx, session.ID)
	if err == nil {
		var latest uint64
		for idx := len(items) - 1; idx >= 0; idx-- {
			if isPublicDisplayItem(items[idx], true) {
				latest = items[idx].Sequence
				break
			}
		}
		session.LatestItemSequence = &latest
		if session.State == SessionStateStarting || session.State == SessionStateRunning || session.State == SessionStatePausing || session.State == SessionStatePaused {
			session.Working = session.State == SessionStatePausing || sessionlog.LastOpenTurnID(items) != ""
			session.ActiveTool = activeToolName(items)
		} else {
			session.Working = false
			session.ActiveTool = ""
		}
		if zotigoruntime.AgentKind(session.Agent) == zotigoruntime.AgentCodex {
			for idx := len(items) - 1; idx >= 0; idx-- {
				if usage := items[idx].ContextUsage; usage != nil && usage.Tokens > 0 && usage.Window > 0 {
					status := usage.Status
					if status == "" {
						status = "available"
					}
					source := usage.Source
					if source == "" {
						source = "provider"
					}
					updatedAt := usage.UpdatedAt
					if updatedAt.IsZero() {
						updatedAt = items[idx].CreatedAt
					}
					session.ContextUsage = &SessionContextUsage{Tokens: usage.Tokens, Window: usage.Window, Status: status, Source: source, UpdatedAt: updatedAt}
					break
				}
			}
		}
	}
	if stored == nil || zotigoruntime.AgentKind(session.Agent) == zotigoruntime.AgentCodex || session.ContextUsage.Status == "available" {
		return session
	}
	appConfig, err := config.NewManager().LoadForDir(session.WorkingDirectory)
	if err != nil {
		return session
	}
	_, profile, err := appConfig.ResolveProfile(session.ProfileName)
	if err != nil {
		return session
	}
	contextWindow := profile.ContextWindow
	if contextWindow <= 0 {
		contextWindow = config.DefaultContextWindow
	}
	updatedAt := session.UpdatedAt
	prompt := protocol.ContextInput{ProfileKey: compaction.ContextProfileKey(profile)}
	// A running worker persists small progress records after each model call.
	// The full snapshot is only checkpointed at turn/pause boundaries.
	if session.Working {
		for idx := len(items) - 1; idx >= 0; idx-- {
			if usage := items[idx].ContextUsage; usage != nil && usage.ProfileKey == prompt.ProfileKey && usage.Window == contextWindow {
				session.ContextUsage = &SessionContextUsage{Tokens: usage.Tokens, Window: usage.Window, Status: "available", Source: usage.Source, UpdatedAt: usage.UpdatedAt}
				return session
			}
			if items[idx].Type == zotigosession.DisplayItemTurnStarted || items[idx].Type == zotigosession.DisplayItemProfileChanged || items[idx].Type == zotigosession.DisplayItemContextCompacted {
				break
			}
		}
	}
	if len(stored.AgentSnapshot.History) == 0 {
		return session
	}
	for idx := len(stored.AgentSnapshot.History) - 1; idx >= 0; idx-- {
		message := stored.AgentSnapshot.History[idx]
		if message.Role == protocol.RoleAssistant && message.Metadata != nil && message.Metadata.Usage != nil {
			if anchor := message.Metadata.ContextInput; anchor != nil {
				// Dormant sessions use the last worker's static prompt estimate.
				// The worker revalidates system/tool definitions before dispatch.
				prompt.PrefixTokens = anchor.PrefixTokens
				if anchor.ProfileKey == prompt.ProfileKey {
					prompt.PromptKey = anchor.PromptKey
				}
			}
			updatedAt = message.CreatedAt
			if message.FinishedAt != nil {
				updatedAt = *message.FinishedAt
			}
			break
		}
	}
	inputTokens, calibrated := h.nativeContextTokens(session.ID, stored.AgentSnapshot.History, prompt)
	source := "estimate"
	if calibrated {
		source = "provider_estimate"
	}
	session.ContextUsage = &SessionContextUsage{Tokens: inputTokens, Window: contextWindow, Status: "available", Source: source, UpdatedAt: updatedAt}
	return session
}

func unavailableSessionContextUsage() *SessionContextUsage {
	return &SessionContextUsage{Status: "unavailable"}
}

// Read-only counting is shared across sessions; constructing a tokenizer for
// every session on every sidebar refresh rebuilds the BPE tables.
var nativeContextEstimator = sync.OnceValue(func() *compaction.Compressor {
	return compaction.NewCompressor(compaction.CompressorConfig{})
})

func activeToolName(items []zotigosession.DisplayItem) string {
	active := make(map[string]string)
	order := make([]string, 0)
	for _, item := range items {
		if item.Type == zotigosession.DisplayItemToolExecutionStarted && item.ToolExecution != nil {
			id := item.ToolExecution.ToolCallID
			active[id] = item.ToolExecution.ToolName
			order = append(order, id)
		}
		for _, part := range item.Content {
			if part.ToolResult != nil {
				delete(active, part.ToolResult.ToolCallID)
			}
		}
		if item.Type == zotigosession.DisplayItemTurnCompleted || item.Type == zotigosession.DisplayItemTurnFailed || item.Type == zotigosession.DisplayItemTurnInterrupted {
			clear(active)
		}
	}
	for idx := len(order) - 1; idx >= 0; idx-- {
		if name, ok := active[order[idx]]; ok {
			return name
		}
	}
	return ""
}

// Runtime liveness comes from the registry; user-editable configuration comes
// from durable metadata, including while a worker is still applying a change.
func withSessionMetadata(runtime Session, meta zotigosession.Metadata) Session {
	runtime.Agent = meta.Agent
	if meta.Agent == string(zotigoruntime.AgentCodex) {
		runtime.ProfileName = ""
	} else {
		runtime.ProfileName = meta.ProfileName
	}
	runtime.Model = meta.Model
	runtime.ReasoningEffort = meta.ReasoningEffort
	runtime.ApprovalPolicy = meta.ApprovalPolicy
	runtime.UpdatedAt = meta.UpdatedAt
	runtime.LastPrompt = meta.LastPrompt
	runtime.ChannelToolsVersion = meta.Capabilities.ChannelToolsVersion
	runtime.SessionToolsVersion = meta.Capabilities.SessionToolsVersion
	runtime.ChannelToolsEligible = channelToolsEligible(meta)
	return runtime
}
