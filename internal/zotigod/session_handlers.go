package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
	"github.com/jayyao97/zotigo/internal/channels"
	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

func (h *handler) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		diagnostics := h.codexSyncDiagnostics(r.Context(), r.URL.Query().Get("sync_codex") == "true")
		sessions, err := h.listSessions(r.Context())
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("list sessions: %v", err))
			return
		}
		writeAPIJSON(w, http.StatusOK, struct {
			Sessions    []Session               `json:"sessions"`
			Diagnostics []sessionSyncDiagnostic `json:"diagnostics,omitempty"`
		}{Sessions: sessions, Diagnostics: diagnostics})
	case http.MethodPost:
		var req createSessionRequest
		if err := readOptionalJSON(r, &req); err != nil {
			writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
			return
		}
		session := newSession("", "")
		workingDirectory := ""
		var assignedWorkspaceID string
		if req.WorkspaceID != "" {
			if !h.requireCatalog(w) {
				return
			}
			workspace, releaseWorkspace, workspaceErr := h.lockWorkspaceForUse(r.Context(), req.WorkspaceID)
			if workspaceErr != nil {
				h.writeCatalogError(w, workspaceErr)
				return
			}
			defer releaseWorkspace()
			if workspace.Status != zotigoworkspace.WorkspaceStatusReady {
				writeAPIError(w, http.StatusConflict, "workspace is not ready")
				return
			}
			workingDirectory = workspace.RootPath
			if strings.TrimSpace(req.WorkingDirectory) != "" {
				// Ownership and execution cwd are separate: a session started
				// from code/backend keeps that directory as its cwd.
				explicit, err := resolveWorkingDirectory(req.WorkingDirectory)
				if err != nil {
					writeAPIError(w, http.StatusBadRequest, err.Error())
					return
				}
				if !zotigoworkspace.PathWithin(workspace.RootPath, explicit) {
					writeAPIError(w, http.StatusBadRequest, "working_directory must be inside the selected workspace")
					return
				}
				workingDirectory = explicit
			}
			assignedWorkspaceID = workspace.ID
		} else if strings.TrimSpace(req.WorkingDirectory) == "" {
			root := h.sessionStoreRoot()
			if root == "" {
				writeAPIError(w, http.StatusInternalServerError, "scratch sessions require persistent session storage")
				return
			}
			workingDirectory = filepath.Join(root, "scratch", session.ID)
		} else {
			var err error
			workingDirectory, err = resolveWorkingDirectory(req.WorkingDirectory)
			if err != nil {
				writeAPIError(w, http.StatusBadRequest, err.Error())
				return
			}
			workspaceID, releaseWorkspace, ok := h.workspaceOwningDirectory(w, r.Context(), workingDirectory)
			if !ok {
				return
			}
			defer releaseWorkspace()
			assignedWorkspaceID = workspaceID
		}
		approvalPolicy, err := workerbase.NormalizeSessionApprovalPolicy(req.ApprovalPolicy, true)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		agentKind := zotigoruntime.AgentKind(strings.TrimSpace(req.Agent))
		if agentKind == "" {
			agentKind = zotigoruntime.AgentZotigo
		}
		var profileName string
		switch agentKind {
		case zotigoruntime.AgentZotigo:
			if req.Model != "" || req.ReasoningEffort != "" {
				writeAPIError(w, http.StatusBadRequest, "model and reasoning_effort require agent codex")
				return
			}
			appConfig, configErr := config.NewManager().LoadForDir(workingDirectory)
			if configErr != nil {
				writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load profiles: %v", configErr))
				return
			}
			profileName, _, err = appConfig.ResolveProfile(req.Profile)
			if err != nil {
				if strings.TrimSpace(req.Profile) != "" {
					writeAPIError(w, http.StatusBadRequest, err.Error())
				} else {
					writeAPIError(w, http.StatusInternalServerError, "default "+err.Error())
				}
				return
			}
		case zotigoruntime.AgentCodex:
			if strings.TrimSpace(req.Profile) != "" {
				writeAPIError(w, http.StatusBadRequest, "profile is only valid for agent zotigo")
				return
			}
			if strings.TrimSpace(req.Model) == "" || strings.TrimSpace(req.ReasoningEffort) == "" {
				writeAPIError(w, http.StatusBadRequest, "codex sessions require model and reasoning_effort")
				return
			}
			if _, adapterErr := h.runtimes.adapter(agentKind); adapterErr != nil {
				writeAPIError(w, http.StatusServiceUnavailable, adapterErr.Error())
				return
			}
			if h.store == nil || h.sessionStoreRoot() == "" {
				writeAPIError(w, http.StatusServiceUnavailable, "codex sessions require persistent session storage")
				return
			}
			if err := h.validateCodexSettings(r.Context(), strings.TrimSpace(req.Model), strings.TrimSpace(req.ReasoningEffort)); err != nil {
				writeAPIError(w, http.StatusBadRequest, err.Error())
				return
			}
			profileName = ""
		default:
			writeAPIError(w, http.StatusBadRequest, "unsupported agent")
			return
		}
		session.WorkingDirectory = workingDirectory
		session.ProfileName = profileName
		if assignedWorkspaceID == "" && strings.TrimSpace(req.WorkingDirectory) == "" {
			if err := os.MkdirAll(session.WorkingDirectory, 0700); err != nil {
				writeAPIError(w, http.StatusInternalServerError, "could not create scratch directory")
				return
			}
		}
		session.Agent = string(agentKind)
		session.Model = strings.TrimSpace(req.Model)
		session.ReasoningEffort = strings.TrimSpace(req.ReasoningEffort)
		session.ApprovalPolicy = approvalPolicy
		if err := h.persistSession(r.Context(), session); err != nil {
			if assignedWorkspaceID == "" && strings.TrimSpace(req.WorkingDirectory) == "" {
				_ = os.Remove(session.WorkingDirectory)
			}
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("persist session: %v", err))
			return
		}
		var workspacePosition *int64
		if assignedWorkspaceID != "" {
			organization, err := h.catalog.AssignSession(r.Context(), session.ID, assignedWorkspaceID)
			if err != nil {
				if h.store != nil {
					_ = h.store.Delete(r.Context(), session.ID)
				}
				h.writeCatalogError(w, err)
				return
			}
			workspacePosition = organization.WorkspacePosition
		}
		session = h.registry.Add(session)
		writeAPIJSON(w, http.StatusCreated, struct {
			Session
			WorkspacePosition *int64 `json:"workspace_position,omitempty"`
		}{session, workspacePosition})
	default:
		w.Header().Set("Allow", "GET, POST")
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func resolveWorkingDirectory(raw string) (string, error) {
	workDir := strings.TrimSpace(raw)
	if workDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve working_directory: %w", err)
		}
		workDir = cwd
	}
	if !filepath.IsAbs(workDir) {
		return "", fmt.Errorf("working_directory must be an absolute path")
	}
	workDir = filepath.Clean(workDir)
	info, err := os.Stat(workDir)
	if err != nil {
		return "", fmt.Errorf("working_directory must exist: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("working_directory must be a directory")
	}
	return workDir, nil
}

// workspaceOwningDirectory binds a directory session to the Workspace that
// contains its cwd. A directory outside every Workspace stays an ordinary
// directory session. A directory inside a Workspace that cannot accept new
// sessions, or inside several Workspaces, is rejected rather than silently
// left unowned. The returned release function holds the same Workspace
// locks as an explicit workspace_id request until the caller finishes.
func (h *handler) workspaceOwningDirectory(w http.ResponseWriter, ctx context.Context, workingDirectory string) (string, func(), bool) {
	noop := func() {}
	if h.catalog == nil {
		// Without a catalog there is no Workspace model to bind against;
		// this matches the daemon's existing degraded-catalog behavior.
		return "", noop, true
	}
	owner, err := h.catalog.WorkspaceContainingPath(ctx, workingDirectory)
	switch {
	case errors.Is(err, zotigoworkspace.ErrNotFound):
		return "", noop, true
	case errors.Is(err, zotigoworkspace.ErrConflict):
		writeAPIErrorCode(w, http.StatusConflict, "workspace_ambiguous", err.Error()+"; pass workspace_id explicitly")
		return "", noop, false
	case err != nil:
		h.writeCatalogError(w, err)
		return "", noop, false
	}
	workspace, release, err := h.lockWorkspaceForUse(ctx, owner.ID)
	if err != nil {
		if errors.Is(err, zotigoworkspace.ErrConflict) {
			writeAPIErrorCode(w, http.StatusConflict, "workspace_not_ready", fmt.Sprintf("working_directory belongs to workspace %s: %v", owner.ID, err))
			return "", noop, false
		}
		h.writeCatalogError(w, err)
		return "", noop, false
	}
	if workspace.Status != zotigoworkspace.WorkspaceStatusReady {
		release()
		writeAPIErrorCode(w, http.StatusConflict, "workspace_not_ready", fmt.Sprintf("working_directory belongs to workspace %s, which is %s", workspace.ID, workspace.Status))
		return "", noop, false
	}
	return workspace.ID, release, true
}

func (h *handler) persistSession(ctx context.Context, session Session) error {
	if h.store == nil {
		return nil
	}
	storedProfile := session.ProfileName
	if zotigoruntime.AgentKind(session.Agent) == zotigoruntime.AgentCodex {
		storedProfile = "__zotigo_backend__:codex"
	}
	return h.store.Put(ctx, &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:               session.ID,
			WorkingDirectory: session.WorkingDirectory,
			Agent:            session.Agent,
			ProfileName:      storedProfile,
			Model:            session.Model,
			ReasoningEffort:  session.ReasoningEffort,
			ApprovalPolicy:   session.ApprovalPolicy,
			CreatedAt:        session.CreatedAt,
			UpdatedAt:        session.CreatedAt,
			Capabilities:     zotigosession.Capabilities{ChannelToolsVersion: session.ChannelToolsVersion, SessionToolsVersion: session.SessionToolsVersion},
		},
		AgentSnapshot: agent.Snapshot{
			State:     agent.StateIdle,
			CreatedAt: session.CreatedAt,
		},
		Turns: make([]zotigosession.Turn, 0),
	})
}

func (h *handler) listSessions(ctx context.Context) ([]Session, error) {
	registrySessions := h.registry.List()
	seen := make(map[string]struct{}, len(registrySessions))
	registryIndex := make(map[string]int, len(registrySessions))
	for idx := range registrySessions {
		registrySessions[idx].Live = true
		seen[registrySessions[idx].ID] = struct{}{}
		registryIndex[registrySessions[idx].ID] = idx
	}
	if h.store == nil {
		return registrySessions, nil
	}
	metadata, err := h.store.List(ctx, zotigosession.ListFilter{OrderBy: zotigosession.OrderByUpdatedDesc})
	if err != nil {
		return nil, err
	}
	sessions := make([]Session, len(registrySessions))
	copy(sessions, registrySessions)
	for _, meta := range metadata {
		if _, ok := seen[meta.ID]; ok {
			idx := registryIndex[meta.ID]
			sessions[idx] = withSessionMetadata(sessions[idx], meta)
			continue
		}
		sessions = append(sessions, sessionFromMetadata(meta, SessionStateOffline, false))
	}
	if store, ok := h.store.(sessionReadStore); ok {
		states, err := store.ListSessionReadStates(ctx)
		if err != nil {
			return nil, err
		}
		for i := range sessions {
			state := states[sessions[i].ID]
			sessions[i].ReadState = &state
		}
	}
	return sessions, nil
}

func sessionFromMetadata(meta zotigosession.Metadata, state SessionState, live bool) Session {
	agentKind := meta.Agent
	if agentKind == "" {
		agentKind = string(zotigoruntime.AgentZotigo)
	}
	profileName := meta.ProfileName
	if agentKind == string(zotigoruntime.AgentCodex) {
		profileName = ""
	}
	approvalPolicy := meta.ApprovalPolicy
	if approvalPolicy == "" {
		approvalPolicy = agent.ApprovalPolicyAuto
	}
	return Session{
		ID:                   meta.ID,
		State:                state,
		Live:                 live,
		WorkingDirectory:     meta.WorkingDirectory,
		Agent:                agentKind,
		ProfileName:          profileName,
		Model:                meta.Model,
		ReasoningEffort:      meta.ReasoningEffort,
		ApprovalPolicy:       approvalPolicy,
		LastPrompt:           meta.LastPrompt,
		CreatedAt:            meta.CreatedAt,
		UpdatedAt:            meta.UpdatedAt,
		ContextUsage:         unavailableSessionContextUsage(),
		ChannelToolsVersion:  meta.Capabilities.ChannelToolsVersion,
		SessionToolsVersion:  meta.Capabilities.SessionToolsVersion,
		ChannelToolsEligible: channelToolsEligible(meta),
	}
}

func channelToolsEligible(meta zotigosession.Metadata) bool {
	return zotigoruntime.AgentKind(meta.Agent) != zotigoruntime.AgentCodex || meta.Capabilities.ChannelToolsVersion >= channels.RuntimeToolsVersion || strings.TrimSpace(meta.ConversationID) == ""
}

func (h *handler) storedSession(ctx context.Context, id string) (Session, bool, error) {
	if h.store == nil {
		return Session{}, false, nil
	}
	session, err := h.store.Get(ctx, id)
	if err != nil {
		return Session{}, false, err
	}
	if session == nil {
		return Session{}, false, nil
	}
	return sessionFromMetadata(session.Metadata, SessionStateOffline, false), true, nil
}

func (h *handler) loadSessionIntoRegistry(ctx context.Context, id string) (Session, bool, error) {
	if session, ok := h.registry.Get(id); ok {
		session.Live = true
		return session, true, nil
	}
	stored, ok, err := h.storedSession(ctx, id)
	if err != nil || !ok {
		return Session{}, ok, err
	}
	stored.State = SessionStateCreated
	stored.Live = true
	stored.activationSource = "resume"
	return h.registry.GetOrAdd(stored), true, nil
}

func (h *handler) handleSessionRouteNotFound(w http.ResponseWriter, _ *http.Request) {
	writeAPIError(w, http.StatusNotFound, "not found")
}

func (h *handler) handleSessionGet(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	session, ok := h.registry.Get(id)
	if !ok {
		stored, inStore, err := h.storedSession(r.Context(), id)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load session: %v", err))
			return
		}
		if !inStore {
			writeAPIError(w, http.StatusNotFound, "session not found")
			return
		}
		full, err := h.store.Get(r.Context(), id)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load session: %v", err))
			return
		}
		h.writeSessionWithReadState(w, r, h.decorateSession(r.Context(), stored, full))
		return
	}
	session.Live = true
	var full *zotigosession.Session
	if h.store != nil {
		full, _ = h.store.Get(r.Context(), id)
	}
	h.writeSessionWithReadState(w, r, h.decorateSession(r.Context(), session, full))
}

func (h *handler) handleSessionStart(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	session, err := h.ensureSessionRunning(r.Context(), id)
	if err != nil {
		h.writeEnsureRunningError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, session)
}

func (h *handler) handleSessionItems(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	query, err := parseDisplayItemQuery(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	_, inRegistry := h.registry.Get(id)
	items, inStore, err := h.items.LoadItems(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load display items: %v", err))
		return
	}
	if !inRegistry && !inStore {
		writeAPIError(w, http.StatusNotFound, "session not found")
		return
	}

	writeAPIJSON(w, http.StatusOK, buildItemsResponse(items, query, r.Header.Get("X-Zotigo-Runtime-Updates") == "true"))
}
