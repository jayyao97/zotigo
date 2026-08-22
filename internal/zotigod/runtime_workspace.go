package zotigod

import (
	"context"
	"fmt"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

func (h *handler) runtimeLaunchSpec(ctx context.Context, sessionID string) (zotigoruntime.WorkerLaunchSpec, error) {
	if h.store == nil {
		session, ok := h.registry.Get(sessionID)
		if !ok {
			return zotigoruntime.WorkerLaunchSpec{}, errSessionNotFound
		}
		agentKind := zotigoruntime.AgentKind(session.Agent)
		if agentKind == "" {
			agentKind = zotigoruntime.AgentZotigo
		}
		if agentKind != zotigoruntime.AgentZotigo {
			return zotigoruntime.WorkerLaunchSpec{}, fmt.Errorf("runtime %q requires persistent session storage", agentKind)
		}
		return zotigoruntime.WorkerLaunchSpec{
			SessionID: sessionID, Agent: agentKind, WorkingDirectory: session.WorkingDirectory,
		}, nil
	}
	stored, err := h.store.Get(ctx, sessionID)
	if err != nil {
		return zotigoruntime.WorkerLaunchSpec{}, fmt.Errorf("load session runtime metadata: %w", err)
	}
	if stored == nil {
		return zotigoruntime.WorkerLaunchSpec{}, errSessionNotFound
	}
	agentKind := zotigoruntime.AgentKind(stored.Agent)
	if agentKind == "" {
		agentKind = zotigoruntime.AgentZotigo
	}
	spec := zotigoruntime.WorkerLaunchSpec{
		SessionID: sessionID, SessionStoreRoot: h.sessionStoreRoot(), Agent: agentKind, WorkingDirectory: stored.WorkingDirectory,
		Settings: zotigoruntime.Settings{Model: stored.Model, ReasoningEffort: stored.ReasoningEffort},
	}
	if stored.ConversationID != "" {
		spec.SessionBinding = &zotigoruntime.BackendBinding{
			Agent: agentKind, ConversationID: stored.ConversationID, BackendVersion: stored.BackendVersion,
		}
	}
	if agentKind != zotigoruntime.AgentCodex {
		return spec, nil
	}
	if h.catalog == nil {
		return zotigoruntime.WorkerLaunchSpec{}, fmt.Errorf("codex runtime requires workspace catalog")
	}
	organization, err := h.catalog.GetSessionOrganization(ctx, sessionID)
	if err != nil || organization.WorkspaceID == nil {
		return zotigoruntime.WorkerLaunchSpec{}, fmt.Errorf("codex runtime requires assigned workspace")
	}
	spec.WorkingDirectory = h.sessionWorkspaceRoot(ctx, organization)
	return spec, nil
}

func (h *handler) sessionWorkspaceRoot(ctx context.Context, organization zotigoworkspace.SessionOrganization) string {
	if organization.WorkspaceID != nil {
		if workspace, err := h.catalog.GetWorkspace(ctx, *organization.WorkspaceID); err == nil {
			return workspace.RootPath
		}
	}
	return ""
}

type backendBindingStore interface {
	UpdateBackendBinding(context.Context, string, string, string, string, string) error
}

var _ backendBindingStore = (*zotigosession.FileStore)(nil)

func (h *handler) handleConversationBound(sessionID string, generation string, request *workerConversationBound) {
	if request == nil || request.ConversationID == "" {
		return
	}
	result := workerConversationBoundResult{ConversationID: request.ConversationID}
	unlock := h.sessionOps.lock(sessionID)
	defer unlock()
	if !h.workers.Matches(sessionID, generation) {
		result.ErrorCode = "stale_worker"
		result.Error = "worker connection is stale"
		h.workers.SendConversationBoundResult(sessionID, generation, result)
		return
	}
	stored, err := h.store.Get(context.Background(), sessionID)
	if err != nil || stored == nil {
		result.ErrorCode = "backend_binding_failed"
		result.Error = "load session binding"
		h.workers.SendConversationBoundResult(sessionID, generation, result)
		return
	}
	store, ok := h.store.(backendBindingStore)
	if !ok {
		result.ErrorCode = "backend_binding_failed"
		result.Error = "session store does not support backend binding"
		h.workers.SendConversationBoundResult(sessionID, generation, result)
		return
	}
	if err := store.UpdateBackendBinding(context.Background(), sessionID, stored.Agent, stored.ConversationID, request.ConversationID, ""); err != nil {
		result.ErrorCode = "backend_binding_conflict"
		result.Error = err.Error()
		h.workers.SendConversationBoundResult(sessionID, generation, result)
		return
	}
	h.workers.SendConversationBoundResult(sessionID, generation, result)
}
