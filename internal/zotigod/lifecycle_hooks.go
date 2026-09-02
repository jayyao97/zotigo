package zotigod

import (
	"context"
	"fmt"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/internal/hooks"
	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

type hookEventDispatcher interface {
	Dispatch(context.Context, hooks.Event) hooks.DispatchResult
}

func (h *handler) dispatchSessionStart(session Session) {
	if h == nil || h.hooks == nil {
		return
	}
	event := h.newSessionHookEvent(hooks.SessionStart, session)
	source := session.activationSource
	if source == "" {
		source = "start"
	}
	event.Session = &hooks.SessionPayload{Source: source}
	h.hooks.Dispatch(context.Background(), event)
}

func (h *handler) dispatchSessionEnd(session Session) {
	if h == nil || h.hooks == nil {
		return
	}
	event := h.newSessionHookEvent(hooks.SessionEnd, session)
	payload := &hooks.SessionPayload{Result: string(session.State), ErrorCode: session.ErrorCode, Model: session.Model}
	model, usage, err := h.sessionEndTelemetry(session)
	payload.Model = model
	payload.Usage = usage
	if err != nil {
		if h.logger != nil {
			h.logger.Printf("SessionEnd hook telemetry unavailable for session %s: %v", session.ID, err)
		}
	}
	event.Session = payload
	h.hooks.Dispatch(context.Background(), event)
}

func (h *handler) sessionEndTelemetry(session Session) (string, *hooks.UsagePayload, error) {
	if h.store == nil {
		return session.Model, nil, nil
	}
	stored, err := h.store.Get(context.Background(), session.ID)
	if err != nil {
		return session.Model, nil, fmt.Errorf("load persisted session: %w", err)
	}
	if stored == nil {
		return session.Model, nil, nil
	}

	usage := stored.AgentSnapshot.CumulativeUsage.Normalized()
	if usage == (protocol.Usage{}) {
		usage = protocol.SessionUsage(stored.AgentSnapshot.History).Normalized()
	}
	var payload *hooks.UsagePayload
	if usage.TotalTokens != 0 {
		payload = usagePayload(usage)
	}

	model := session.Model
	if zotigoruntime.AgentKind(session.Agent) == zotigoruntime.AgentZotigo {
		appConfig, err := config.NewManager().LoadForDir(stored.WorkingDirectory)
		if err != nil {
			return model, payload, fmt.Errorf("load profile configuration: %w", err)
		}
		_, profile, err := appConfig.ResolveProfile(stored.ProfileName)
		if err != nil {
			return model, payload, fmt.Errorf("resolve session profile: %w", err)
		}
		model = profile.Model
	}
	return model, payload, nil
}

func usagePayload(usage protocol.Usage) *hooks.UsagePayload {
	usage = usage.Normalized()
	return &hooks.UsagePayload{
		InputTokens:              usage.InputTokens,
		OutputTokens:             usage.OutputTokens,
		TotalTokens:              usage.TotalTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		CacheReadInputTokens:     usage.CacheReadInputTokens,
	}
}

func (h *handler) newSessionHookEvent(eventName hooks.EventName, session Session) hooks.Event {
	agentName := session.Agent
	if agentName == "" {
		agentName = string(zotigoruntime.AgentZotigo)
	}
	workDir := session.WorkingDirectory
	if workDir == "" {
		workDir = h.sessionWorkingDirectory(context.Background(), session.ID)
	}
	return hooks.NewEvent(eventName, session.ID, agentName, workDir)
}
