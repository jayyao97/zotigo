package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	zotigosession "github.com/jayyao97/zotigo/core/session"
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
		value, err := h.channels.PutConversationValidated(r.Context(), id, input, func(ctx context.Context, conversation channels.Conversation, prompt channels.SessionPromptConfig) error {
			if conversation.RootID == "" {
				return h.validateChannelWorkspaceBinding(ctx, conversation.WorkspaceID)
			}
			if strings.TrimSpace(conversation.SessionID) == "" {
				return nil
			}
			unlock := h.sessionOps.lock(conversation.SessionID)
			defer unlock()
			if err := h.validateChannelSessionBinding(ctx, conversation.SessionID); err != nil {
				return err
			}
			session, err := h.store.Get(ctx, conversation.SessionID)
			if err != nil || session == nil {
				return errors.New("bound session is unavailable")
			}
			session.PromptConfig = zotigosession.PromptConfig{AgentInstructions: prompt.AgentInstructions, ApprovalInstructions: prompt.ApprovalInstructions, ReviewAllTools: prompt.ReviewAllTools, Revision: 1}
			session.UpdatedAt = time.Now().UTC()
			return h.store.Put(ctx, session)
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

func (h *handler) validateChannelWorkspaceBinding(ctx context.Context, workspaceID string) error {
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
	appConfig, err := config.NewManager().LoadForDir(workspace.RootPath)
	if err != nil {
		return fmt.Errorf("load channel workspace profiles: %w", err)
	}
	if _, _, err := appConfig.ResolveProfile(""); err != nil {
		return fmt.Errorf("resolve channel workspace default profile: %w", err)
	}
	return nil
}

func (h *handler) validateChannelSessionBinding(ctx context.Context, sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return nil
	}
	if h.store == nil {
		return errors.New("session store is unavailable")
	}
	stored, err := h.store.Get(ctx, strings.TrimSpace(sessionID))
	if err != nil {
		return fmt.Errorf("load bound session: %w", err)
	}
	if stored == nil {
		return errors.New("bound session not found")
	}
	if zotigoruntime.AgentKind(stored.Agent) != zotigoruntime.AgentZotigo {
		return errors.New("channels currently require a Zotigo session")
	}
	if stored.ApprovalPolicy != agent.ApprovalPolicyAuto {
		return errors.New("channel sessions must use auto approval policy")
	}
	if live, ok := h.registry.Get(stored.ID); ok && live.State != SessionStateCreated && live.State != SessionStateOffline {
		return errors.New("a channel can only bind a new, idle session")
	}
	items, exists, err := h.items.LoadItems(ctx, stored.ID)
	if err != nil {
		return fmt.Errorf("load bound session history: %w", err)
	}
	if !exists {
		return errors.New("bound session history not found")
	}
	if len(items) != 0 {
		return errors.New("a channel can only bind a session with no existing history")
	}
	return nil
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
