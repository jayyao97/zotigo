package zotigod

import (
	"context"

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
	event.Session = &hooks.SessionPayload{Result: string(session.State), ErrorCode: session.ErrorCode}
	h.hooks.Dispatch(context.Background(), event)
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
