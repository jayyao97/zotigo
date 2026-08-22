package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

type changeApprovalPolicyRequest struct {
	ApprovalPolicy agent.ApprovalPolicy `json:"approval_policy"`
}

type changeApprovalPolicyResponse struct {
	ApprovalPolicy agent.ApprovalPolicy `json:"approval_policy"`
	Status         string               `json:"status"`
	CommandID      string               `json:"command_id,omitempty"`
}

type sessionApprovalPolicyUpdater interface {
	UpdateApprovalPolicy(ctx context.Context, id string, policy agent.ApprovalPolicy, updatedAt time.Time) error
}

func persistSessionApprovalPolicy(ctx context.Context, store zotigosession.Store, sess *zotigosession.Session) error {
	if updater, ok := store.(sessionApprovalPolicyUpdater); ok {
		return updater.UpdateApprovalPolicy(ctx, sess.ID, sess.ApprovalPolicy, sess.UpdatedAt)
	}
	return store.Put(ctx, sess)
}

func normalizeSessionApprovalPolicy(policy agent.ApprovalPolicy, allowEmpty bool) (agent.ApprovalPolicy, error) {
	if policy == "" && allowEmpty {
		return agent.ApprovalPolicyAuto, nil
	}
	switch policy {
	case agent.ApprovalPolicyAuto, agent.ApprovalPolicyBypass:
		return policy, nil
	default:
		return "", fmt.Errorf("approval_policy must be %q or %q", agent.ApprovalPolicyAuto, agent.ApprovalPolicyBypass)
	}
}

func (h *handler) handleSessionApprovalPolicy(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req changeApprovalPolicyRequest
	if err := readRequiredJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	target, err := normalizeSessionApprovalPolicy(req.ApprovalPolicy, false)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	for {
		session, live := h.registry.Get(id)
		if !live || session.State != SessionStateStarting || h.workers.Has(id) {
			break
		}
		if !h.waitForWorker(r.Context(), id) {
			writeAPIError(w, http.StatusServiceUnavailable, "approval policy change requires an online worker")
			return
		}
	}

	unlockOperation := h.sessionOps.lock(id)
	operationLocked := true
	defer func() {
		if operationLocked {
			unlockOperation()
		}
	}()

	session, live := h.registry.Get(id)
	if !live {
		var ok bool
		session, ok, err = h.storedSession(r.Context(), id)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load session: %v", err))
			return
		}
		if !ok {
			writeAPIError(w, http.StatusNotFound, "session not found")
			return
		}
	} else if h.store != nil {
		stored, loadErr := h.store.Get(r.Context(), id)
		if loadErr != nil {
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load session: %v", loadErr))
			return
		}
		if stored != nil {
			session.ApprovalPolicy = stored.ApprovalPolicy
		}
	}
	if session.Agent == string(zotigoruntime.AgentCodex) {
		writeAPIError(w, http.StatusConflict, "codex sessions do not support approval policy changes")
		return
	}
	if session.State == SessionStateEnded || session.State == SessionStateFailed {
		writeAPIError(w, http.StatusConflict, "approval policy change requires a resumable session")
		return
	}
	items, _, err := h.items.LoadItems(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load display items: %v", err))
		return
	}
	if lastOpenTurnID(items) != "" || hasPendingMessageCommand(items) {
		writeAPIError(w, http.StatusConflict, "approval policy change requires an idle session")
		return
	}
	applyWithoutWorker := !live || session.State == SessionStateCreated ||
		(session.State == SessionStateRunning && !h.workers.Has(id))
	pendingPolicy, pendingCommandID, hasPendingPolicy := pendingApprovalPolicyIntent(items)
	if hasPendingPolicy && pendingPolicy == target {
		writeAPIJSON(w, http.StatusAccepted, changeApprovalPolicyResponse{
			ApprovalPolicy: target,
			Status:         "pending",
			CommandID:      pendingCommandID,
		})
		return
	} else if !hasPendingPolicy && session.ApprovalPolicy == target && !applyWithoutWorker {
		writeAPIJSON(w, http.StatusOK, changeApprovalPolicyResponse{ApprovalPolicy: target, Status: "applied"})
		return
	}

	if applyWithoutWorker {
		if hasPendingPolicy {
			writeAPIError(w, http.StatusConflict, "approval policy change is already pending; start the session before changing it again")
			return
		}
		if err := h.applyStoredApprovalPolicy(r.Context(), id, target); err != nil {
			if errors.Is(err, zotigosession.ErrSessionLocked) {
				writeAPIErrorCode(w, http.StatusConflict, "session_in_use", "session is active in another process")
				return
			}
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("change approval policy: %v", err))
			return
		}
		if live {
			_, _ = h.registry.UpdateApprovalPolicy(id, target)
		}
		writeAPIJSON(w, http.StatusOK, changeApprovalPolicyResponse{ApprovalPolicy: target, Status: "applied"})
		return
	}

	workerOnline := h.workers.Has(id)
	if !workerOnline {
		writeAPIError(w, http.StatusServiceUnavailable, "approval policy change requires an online worker")
		return
	}
	item, err := h.items.AppendItemIf(r.Context(), id, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemSessionCommand,
		Command: &zotigosession.DisplayCommand{
			Type:           sessionCommandApprovalPolicy,
			ApprovalPolicy: string(target),
		},
	}, requireIdleSession)
	if err != nil {
		if errors.Is(err, errSessionBusy) {
			writeAPIError(w, http.StatusConflict, "approval policy change requires an idle session")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("append approval policy command: %v", err))
		return
	}
	command := approvalPolicyCommandFromItem(item)
	unlockOperation()
	operationLocked = false
	h.sendCommand(r.Context(), id, command)
	writeAPIJSON(w, http.StatusAccepted, changeApprovalPolicyResponse{
		ApprovalPolicy: target,
		Status:         "pending",
		CommandID:      item.ID,
	})
}

func pendingApprovalPolicyIntent(items []zotigosession.DisplayItem) (agent.ApprovalPolicy, string, bool) {
	completed := make(map[string]bool)
	for _, item := range items {
		if item.Type == zotigosession.DisplayItemApprovalPolicyChanged && item.ApprovalPolicy != nil {
			completed[item.ApprovalPolicy.CommandID] = true
		}
	}
	var policy agent.ApprovalPolicy
	var commandID string
	var sequence uint64
	for _, item := range items {
		if item.Command == nil || item.Command.Type != sessionCommandApprovalPolicy || completed[item.ID] || item.Sequence < sequence {
			continue
		}
		policy = agent.ApprovalPolicy(item.Command.ApprovalPolicy)
		commandID = item.ID
		sequence = item.Sequence
	}
	return policy, commandID, commandID != ""
}

func (h *handler) applyStoredApprovalPolicy(ctx context.Context, id string, policy agent.ApprovalPolicy) error {
	if h.store == nil {
		return fmt.Errorf("session store is not configured")
	}
	if err := h.store.Lock(ctx, id); err != nil {
		return err
	}
	sess, err := h.store.Get(ctx, id)
	if err != nil {
		return errors.Join(err, h.store.Unlock(context.Background(), id))
	}
	if sess == nil {
		return errors.Join(fmt.Errorf("session not found"), h.store.Unlock(context.Background(), id))
	}
	sess.ApprovalPolicy = policy
	sess.UpdatedAt = time.Now().UTC()
	return errors.Join(persistSessionApprovalPolicy(ctx, h.store, sess), h.store.Unlock(context.Background(), id))
}
