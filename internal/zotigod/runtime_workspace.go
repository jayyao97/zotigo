package zotigod

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

func (h *handler) prepareRuntimeWorkspace(ctx context.Context, workspace zotigoworkspace.Workspace, agent zotigoruntime.AgentKind) (zotigoruntime.WorkspaceBinding, error) {
	adapter, err := h.runtimes.adapter(agent)
	if err != nil {
		return zotigoruntime.WorkspaceBinding{}, err
	}
	workspaceAdapter, ok := adapter.(zotigoruntime.WorkspaceAdapter)
	if !ok {
		return zotigoruntime.WorkspaceBinding{}, fmt.Errorf("runtime %q does not support workspace binding", agent)
	}
	root := filepath.Join(workspace.RootPath, "code")
	spec := zotigoruntime.WorkspaceSpec{WorkspaceID: workspace.ID, Name: workspace.Title, RootPath: root}

	binding, err := h.catalog.GetRuntimeWorkspaceBinding(ctx, workspace.ID, string(agent))
	if err == nil {
		return h.resolveRuntimeWorkspaceBinding(ctx, workspaceAdapter, spec, binding)
	}
	if !errors.Is(err, zotigoworkspace.ErrNotFound) {
		return zotigoruntime.WorkspaceBinding{}, err
	}

	existing, err := workspaceAdapter.FindWorkspace(ctx, spec)
	if err != nil {
		return zotigoruntime.WorkspaceBinding{}, err
	}
	if existing != nil {
		binding, err = h.catalog.ReuseRuntimeWorkspace(ctx, workspace.ID, string(agent), existing.ID, "")
		if err != nil {
			return zotigoruntime.WorkspaceBinding{}, err
		}
		return runtimeWorkspaceBinding(binding), nil
	}

	createKey := uuid.NewString()
	binding, _, err = h.catalog.BeginRuntimeWorkspaceBinding(ctx, workspace.ID, string(agent), createKey, workspace.Title, root)
	if err != nil {
		return zotigoruntime.WorkspaceBinding{}, err
	}
	return h.resolveRuntimeWorkspaceBinding(ctx, workspaceAdapter, spec, binding)
}

func (h *handler) resolveRuntimeWorkspaceBinding(ctx context.Context, adapter zotigoruntime.WorkspaceAdapter, spec zotigoruntime.WorkspaceSpec, binding zotigoworkspace.RuntimeWorkspaceBinding) (zotigoruntime.WorkspaceBinding, error) {
	switch binding.State {
	case zotigoworkspace.RuntimeWorkspaceBindingBound:
		external, err := adapter.ReadWorkspace(ctx, binding.ExternalID)
		if err != nil {
			if !errors.Is(err, zotigoruntime.ErrWorkspaceNotFound) {
				return zotigoruntime.WorkspaceBinding{}, err
			}
			return h.recoverRuntimeWorkspaceBinding(ctx, adapter, spec, binding)
		}
		matches, err := sameCanonicalPath(external.RootPath, spec.RootPath)
		if err != nil {
			return zotigoruntime.WorkspaceBinding{}, err
		}
		if !matches {
			return zotigoruntime.WorkspaceBinding{}, fmt.Errorf("codex project binding root mismatch")
		}
		return runtimeWorkspaceBinding(binding), nil
	case zotigoworkspace.RuntimeWorkspaceBindingCreating:
		external, err := adapter.CreateWorkspace(ctx, zotigoruntime.WorkspaceCreateIntent{
			WorkspaceSpec: zotigoruntime.WorkspaceSpec{
				WorkspaceID: binding.WorkspaceID,
				Name:        binding.CreateName,
				RootPath:    binding.CreateRoot,
			},
			IdempotencyKey: "zotigod:" + binding.WorkspaceID + ":" + binding.CreateKey,
		})
		if err != nil {
			if errors.Is(err, zotigoruntime.ErrWorkspaceCreateTombstone) {
				return h.recoverRuntimeWorkspaceCreateTombstone(ctx, adapter, spec, binding)
			}
			return zotigoruntime.WorkspaceBinding{}, err
		}
		bound, err := h.catalog.CompleteRuntimeWorkspaceBinding(ctx, binding, external.ID, "")
		if err != nil {
			return zotigoruntime.WorkspaceBinding{}, err
		}
		return runtimeWorkspaceBinding(bound), nil
	default:
		return zotigoruntime.WorkspaceBinding{}, fmt.Errorf("unsupported runtime workspace binding state %q", binding.State)
	}
}

func (h *handler) recoverRuntimeWorkspaceCreateTombstone(ctx context.Context, adapter zotigoruntime.WorkspaceAdapter, spec zotigoruntime.WorkspaceSpec, binding zotigoworkspace.RuntimeWorkspaceBinding) (zotigoruntime.WorkspaceBinding, error) {
	existing, err := adapter.FindWorkspace(ctx, spec)
	if err != nil {
		return zotigoruntime.WorkspaceBinding{}, err
	}
	if existing != nil {
		bound, err := h.catalog.CompleteRuntimeWorkspaceBinding(ctx, binding, existing.ID, "")
		if err != nil {
			return zotigoruntime.WorkspaceBinding{}, err
		}
		return runtimeWorkspaceBinding(bound), nil
	}
	next, err := h.catalog.RotateRuntimeWorkspaceCreateKey(ctx, binding, uuid.NewString())
	if errors.Is(err, zotigoworkspace.ErrConflict) {
		next, err = h.catalog.GetRuntimeWorkspaceBinding(ctx, binding.WorkspaceID, binding.Agent)
	}
	if err != nil {
		return zotigoruntime.WorkspaceBinding{}, err
	}
	return h.resolveRuntimeWorkspaceBinding(ctx, adapter, spec, next)
}

func (h *handler) recoverRuntimeWorkspaceBinding(ctx context.Context, adapter zotigoruntime.WorkspaceAdapter, spec zotigoruntime.WorkspaceSpec, binding zotigoworkspace.RuntimeWorkspaceBinding) (zotigoruntime.WorkspaceBinding, error) {
	existing, err := adapter.FindWorkspace(ctx, spec)
	if err != nil {
		return zotigoruntime.WorkspaceBinding{}, err
	}
	var next zotigoworkspace.RuntimeWorkspaceBinding
	if existing != nil {
		next, err = h.catalog.ReplaceRuntimeWorkspaceBinding(ctx, binding, existing.ID, "")
	} else {
		next, err = h.catalog.RebuildRuntimeWorkspaceBinding(ctx, binding, uuid.NewString(), spec.Name, spec.RootPath)
	}
	if errors.Is(err, zotigoworkspace.ErrConflict) {
		next, err = h.catalog.GetRuntimeWorkspaceBinding(ctx, binding.WorkspaceID, binding.Agent)
	}
	if err != nil {
		return zotigoruntime.WorkspaceBinding{}, err
	}
	return h.resolveRuntimeWorkspaceBinding(ctx, adapter, spec, next)
}

func sameCanonicalPath(left string, right string) (bool, error) {
	canonical := func(path string) (string, error) {
		if strings.TrimSpace(path) == "" {
			return "", fmt.Errorf("runtime workspace root is empty")
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return "", err
		}
		return filepath.Clean(resolved), nil
	}
	canonicalLeft, err := canonical(left)
	if err != nil {
		return false, fmt.Errorf("resolve bound runtime workspace root: %w", err)
	}
	canonicalRight, err := canonical(right)
	if err != nil {
		return false, fmt.Errorf("resolve expected runtime workspace root: %w", err)
	}
	return canonicalLeft == canonicalRight, nil
}

func runtimeWorkspaceBinding(binding zotigoworkspace.RuntimeWorkspaceBinding) zotigoruntime.WorkspaceBinding {
	return zotigoruntime.WorkspaceBinding{
		WorkspaceID: binding.WorkspaceID, Agent: zotigoruntime.AgentKind(binding.Agent),
		ExternalID: binding.ExternalID, Revision: binding.Revision, BackendVersion: binding.BackendVersion,
	}
}

func (h *handler) fenceRuntimeWorkspaceWorkers(ctx context.Context, workspaceID string, revision uint64) error {
	if h.catalog == nil || h.store == nil {
		return nil
	}
	organizations, err := h.catalog.ListSessionOrganizations(ctx)
	if err != nil {
		return fmt.Errorf("list workspace sessions for runtime fencing: %w", err)
	}
	for _, organization := range organizations {
		if organization.WorkspaceID == nil || *organization.WorkspaceID != workspaceID {
			continue
		}
		if !h.workers.Has(organization.SessionID) {
			continue
		}
		stored, err := h.store.Get(ctx, organization.SessionID)
		if err != nil {
			return fmt.Errorf("load workspace session for runtime fencing: %w", err)
		}
		if stored != nil && zotigoruntime.AgentKind(stored.Agent) == zotigoruntime.AgentCodex {
			h.workers.CloseIfWorkspaceRevisionMismatch(organization.SessionID, revision)
		}
	}
	return nil
}

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
	binding, err := h.catalog.GetRuntimeWorkspaceBinding(ctx, *organization.WorkspaceID, string(agentKind))
	if err != nil {
		return zotigoruntime.WorkerLaunchSpec{}, fmt.Errorf("load codex project binding: %w", err)
	}
	if binding.State != zotigoworkspace.RuntimeWorkspaceBindingBound {
		return zotigoruntime.WorkerLaunchSpec{}, fmt.Errorf("codex project binding is not ready")
	}
	spec.WorkingDirectory = filepath.Join(h.sessionWorkspaceRoot(ctx, organization), "code")
	runtimeBinding := runtimeWorkspaceBinding(binding)
	spec.WorkspaceBinding = &runtimeBinding
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
	workerRevision, ok := h.workers.WorkspaceRevision(sessionID, generation)
	if !ok {
		result.ErrorCode = "stale_worker"
		result.Error = "worker connection is stale"
		h.workers.SendConversationBoundResult(sessionID, generation, result)
		return
	}
	currentRevision, err := h.expectedWorkspaceBindingRevision(context.Background(), sessionID)
	if err != nil || currentRevision != workerRevision {
		result.ErrorCode = "stale_workspace_binding"
		result.Error = "workspace binding changed"
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
