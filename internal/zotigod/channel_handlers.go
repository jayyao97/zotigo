package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
	"github.com/jayyao97/zotigo/internal/channels"
	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

const maxChannelRequestBytes = 256 << 10

func (h *handler) requireChannels(w http.ResponseWriter) bool {
	if h.channels != nil {
		return true
	}
	writeAPIError(w, http.StatusServiceUnavailable, "channels are unavailable")
	return false
}

func (h *handler) handleChannelConnections(w http.ResponseWriter, r *http.Request) {
	if !h.requireChannels(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		values, err := h.channels.ListConnections(r.Context())
		if err != nil {
			writeAPIError(w, 500, err.Error())
			return
		}
		writeAPIJSON(w, 200, map[string]any{"connections": values})
	case http.MethodPost:
		var input channels.ConnectionInput
		if err := readRequiredLimitedJSON(r, &input, maxChannelRequestBytes); err != nil {
			writeAPIError(w, 400, fmt.Sprintf("decode request: %v", err))
			return
		}
		value, err := h.channels.PutConnection(r.Context(), newZotigodID("channel"), input)
		if err != nil {
			writeAPIError(w, 400, err.Error())
			return
		}
		writeAPIJSON(w, 201, value)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeAPIError(w, 405, "method not allowed")
	}
}

func (h *handler) handleChannelConnection(w http.ResponseWriter, r *http.Request, id string) {
	if !h.requireChannels(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		value, err := h.channels.GetConnection(r.Context(), id)
		if err != nil {
			h.writeChannelError(w, err)
			return
		}
		writeAPIJSON(w, 200, value)
	case http.MethodPut:
		var input channels.ConnectionInput
		if err := readRequiredLimitedJSON(r, &input, maxChannelRequestBytes); err != nil {
			writeAPIError(w, 400, fmt.Sprintf("decode request: %v", err))
			return
		}
		value, err := h.channels.PutConnection(r.Context(), id, input)
		if err != nil {
			writeAPIError(w, 400, err.Error())
			return
		}
		writeAPIJSON(w, 200, value)
	case http.MethodDelete:
		if err := h.channels.DeleteConnection(r.Context(), id); err != nil {
			h.writeChannelError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeAPIError(w, 405, "method not allowed")
	}
}

func (h *handler) handleChannelGroups(w http.ResponseWriter, r *http.Request, id string) {
	if !h.requireChannels(w) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	groups, err := h.channels.ListGroups(r.Context(), id)
	if err != nil {
		h.writeChannelError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

func (h *handler) handleChannelGroupMembers(w http.ResponseWriter, r *http.Request, id string) {
	if !h.requireChannels(w) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	members, err := h.channels.ListGroupMembers(r.Context(), id, strings.TrimSpace(r.PathValue("chat_id")))
	if err != nil {
		h.writeChannelError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{"members": members})
}

func (h *handler) handleChannelConversations(w http.ResponseWriter, r *http.Request) {
	if !h.requireChannels(w) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeAPIError(w, 405, "method not allowed")
		return
	}
	values, err := h.channels.ListConversations(r.Context(), strings.TrimSpace(r.URL.Query().Get("connection_id")))
	if err != nil {
		writeAPIError(w, 500, err.Error())
		return
	}
	writeAPIJSON(w, 200, map[string]any{"conversations": values})
}

func (h *handler) handleChannelConversation(w http.ResponseWriter, r *http.Request, id string) {
	if !h.requireChannels(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		value, err := h.channels.GetConversation(r.Context(), id)
		if err != nil {
			h.writeChannelError(w, err)
			return
		}
		writeAPIJSON(w, 200, value)
	case http.MethodPut:
		var input channels.ConversationInput
		if err := readRequiredLimitedJSON(r, &input, maxChannelRequestBytes); err != nil {
			writeAPIError(w, 400, fmt.Sprintf("decode request: %v", err))
			return
		}
		value, err := h.channels.PutConversationValidated(r.Context(), id, input, func(ctx context.Context, conversation channels.Conversation, prompt channels.SessionPromptConfig, runtime channels.SessionRuntimeConfig) (channels.SessionPromptConfig, channels.SessionRuntimeConfig, error) {
			if conversation.RootID == "" && strings.TrimSpace(conversation.SessionID) == "" {
				return prompt, runtime, h.validateChannelWorkspaceBinding(ctx, conversation.WorkspaceID, runtime)
			}
			if strings.TrimSpace(conversation.SessionID) == "" {
				return prompt, runtime, nil
			}
			unlock := h.sessionOps.lock(conversation.SessionID)
			defer unlock()
			canonicalRuntime, err := h.validateChannelSessionBinding(ctx, conversation.SessionID, runtime)
			if err != nil {
				return prompt, runtime, err
			}
			session, err := h.store.Get(ctx, conversation.SessionID)
			if err != nil || session == nil {
				return prompt, runtime, errors.New("bound session is unavailable")
			}
			if conversation.RootID == "" {
				if h.catalog == nil {
					return prompt, runtime, errors.New("workspace catalog is unavailable")
				}
				organization, organizationErr := h.catalog.GetSessionOrganization(ctx, conversation.SessionID)
				if organizationErr != nil || organization.WorkspaceID == nil || *organization.WorkspaceID != conversation.WorkspaceID {
					return prompt, runtime, errors.New("shared session must belong to the selected workspace")
				}
			}
			canonicalPrompt := channels.SessionPromptConfig{
				AgentInstructions:    session.PromptConfig.AgentInstructions,
				ApprovalInstructions: session.PromptConfig.ApprovalInstructions,
				ReviewAllTools:       session.PromptConfig.ReviewAllTools,
			}
			return canonicalPrompt, canonicalRuntime, nil
		})
		if err != nil {
			if errors.Is(err, channels.ErrNotFound) {
				h.writeChannelError(w, err)
			} else {
				writeAPIError(w, 400, err.Error())
			}
			return
		}
		writeAPIJSON(w, 200, value)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeAPIError(w, 405, "method not allowed")
	}
}

func (h *handler) validateChannelWorkspaceBinding(ctx context.Context, workspaceID string, runtime channels.SessionRuntimeConfig) error {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return nil
	}
	if h.catalog == nil {
		return errors.New("workspace catalog is unavailable")
	}
	workspace, err := h.catalog.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("load channel workspace: %w", err)
	}
	if workspace.Status != zotigoworkspace.WorkspaceStatusReady {
		return errors.New("channel workspace is not ready")
	}
	switch zotigoruntime.AgentKind(runtime.Agent) {
	case zotigoruntime.AgentZotigo:
		appConfig, err := config.NewManager().LoadForDir(workspace.RootPath)
		if err != nil {
			return fmt.Errorf("load channel workspace profiles: %w", err)
		}
		if _, _, err := appConfig.ResolveProfile(runtime.ProfileName); err != nil {
			if runtime.ProfileName == "" {
				return fmt.Errorf("resolve channel workspace default profile: %w", err)
			}
			return fmt.Errorf("resolve channel workspace profile: %w", err)
		}
	case zotigoruntime.AgentCodex:
		if _, err := h.runtimes.adapter(zotigoruntime.AgentCodex); err != nil {
			return err
		}
		if h.store == nil || h.sessionStoreRoot() == "" {
			return errors.New("codex sessions require persistent session storage")
		}
		if err := h.validateCodexSettings(ctx, runtime.Model, runtime.ReasoningEffort); err != nil {
			return err
		}
	default:
		return errors.New("unsupported channel session agent")
	}
	return nil
}

func (h *handler) validateChannelSessionBinding(ctx context.Context, sessionID string, runtime channels.SessionRuntimeConfig) (channels.SessionRuntimeConfig, error) {
	if strings.TrimSpace(sessionID) == "" {
		return runtime, nil
	}
	if h.store == nil {
		return runtime, errors.New("session store is unavailable")
	}
	stored, err := h.store.Get(ctx, strings.TrimSpace(sessionID))
	if err != nil {
		return runtime, fmt.Errorf("load bound session: %w", err)
	}
	if stored == nil {
		return runtime, errors.New("bound session not found")
	}
	canonicalRuntime := channels.SessionRuntimeConfig{Agent: stored.Agent}
	switch zotigoruntime.AgentKind(stored.Agent) {
	case zotigoruntime.AgentZotigo:
		canonicalRuntime.ProfileName = stored.ProfileName
	case zotigoruntime.AgentCodex:
		canonicalRuntime.Model = stored.Model
		canonicalRuntime.ReasoningEffort = stored.ReasoningEffort
	}
	if zotigoruntime.AgentKind(stored.Agent) != zotigoruntime.AgentKind(runtime.Agent) {
		return runtime, errors.New("bound session agent does not match the channel runtime")
	}
	if zotigoruntime.AgentKind(stored.Agent) == zotigoruntime.AgentZotigo && runtime.ProfileName != "" && stored.ProfileName != runtime.ProfileName {
		return runtime, errors.New("bound session profile does not match the channel runtime")
	}
	if zotigoruntime.AgentKind(stored.Agent) == zotigoruntime.AgentCodex && (stored.Model != runtime.Model || stored.ReasoningEffort != runtime.ReasoningEffort) {
		return runtime, errors.New("bound session Codex settings do not match the channel runtime")
	}
	if stored.ApprovalPolicy != agent.ApprovalPolicyAuto {
		return runtime, errors.New("channel sessions must use auto approval policy")
	}
	if live, ok := h.registry.Get(stored.ID); ok {
		if live.Working || (live.State != SessionStateCreated && live.State != SessionStateOffline && live.State != SessionStateRunning) {
			return runtime, errors.New("a channel can only bind an idle session")
		}
	}
	items, exists, err := h.items.LoadItems(ctx, stored.ID)
	if err != nil {
		return runtime, fmt.Errorf("load bound session history: %w", err)
	}
	if !exists {
		return runtime, errors.New("bound session history not found")
	}
	if err := requireIdleSession(items); err != nil {
		return runtime, errors.New("a channel can only bind an idle session")
	}
	return canonicalRuntime, nil
}

func (h *handler) handleChannelMessages(w http.ResponseWriter, r *http.Request, id string) {
	if !h.requireChannels(w) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeAPIError(w, 405, "method not allowed")
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	if _, err := h.channels.GetConversation(r.Context(), id); err != nil {
		h.writeChannelError(w, err)
		return
	}
	values, err := h.channels.ListMessages(r.Context(), id, limit)
	if err != nil {
		writeAPIError(w, 500, err.Error())
		return
	}
	writeAPIJSON(w, 200, map[string]any{"messages": values})
}

func (h *handler) writeChannelError(w http.ResponseWriter, err error) {
	if errors.Is(err, channels.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "channel resource not found")
		return
	}
	writeAPIError(w, http.StatusInternalServerError, err.Error())
}
