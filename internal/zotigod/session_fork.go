package zotigod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	session "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/workspace"
	"github.com/jayyao97/zotigo/internal/sessionadapter"
)

var errForkOutcomeUnknown = errors.New("fork outcome_unknown: creation may have started; do not retry with a new request ID")

type forkSessionRequest struct {
	RequestID     string `json:"request_id"`
	ThroughTurnID string `json:"through_turn_id,omitempty"`
	Title         string `json:"title,omitempty"`
}

type forkJournal struct {
	SourceID        string `json:"source_id"`
	RequestedTurnID string `json:"requested_turn_id"`
	ThroughTurnID   string `json:"through_turn_id"`
	Title           string `json:"title"`
	Complete        bool   `json:"complete"`
}

func (h *handler) handleSessionFork(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !h.requireCatalog(w) {
		return
	}
	var request forkSessionRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || strings.TrimSpace(request.RequestID) == "" || len(request.RequestID) > 200 || len(request.Title) > 200 {
		writeAPIError(w, http.StatusBadRequest, "request_id is required (at most 200 bytes); title at most 200 bytes")
		return
	}
	targetID := "sess_fork_" + toolDigest(id, request.RequestID)
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	created, err := h.forkSession(ctx, targetID, id, request.ThroughTurnID, request.Title)
	if err != nil {
		writeAPIError(w, http.StatusConflict, err.Error())
		return
	}
	result := sessionFromMetadata(created.Metadata, SessionStateOffline, false)
	result.ForkedFrom = created.ForkedFrom
	writeAPIJSON(w, http.StatusCreated, result)
}

// forkSession creates an idle branch. Both desktop API and tools use this same
// operation; tools must separately authorize source history and creation.
// The durable marker precedes any runtime side effect, so a lost RPC response
// cannot silently create a second remote thread on retry.
func (h *handler) forkSession(ctx context.Context, targetID, sourceID, throughTurnID, title string) (*session.Session, error) {
	if h.catalog == nil || h.store == nil || h.sessionStoreRoot() == "" {
		return nil, errors.New("fork requires durable workspace storage")
	}
	org, err := h.catalog.GetSessionOrganization(ctx, sourceID)
	if err != nil || org.WorkspaceID == nil || org.EffectiveArchived() {
		return nil, errors.New("fork source has no active workspace")
	}
	bound, unlockWorkspace, err := h.lockWorkspaceForUse(ctx, *org.WorkspaceID)
	if err != nil {
		return nil, err
	}
	defer unlockWorkspace()
	if bound.Status != workspace.WorkspaceStatusReady {
		return nil, errors.New("fork requires a ready workspace")
	}
	return h.forkSessionInWorkspace(ctx, targetID, sourceID, throughTurnID, title, *org.WorkspaceID)
}

// Caller holds the workspace-use fence, just as normal session creation does.
func (h *handler) forkSessionInWorkspace(ctx context.Context, targetID, sourceID, throughTurnID, title, workspaceID string) (*session.Session, error) {
	if sourceID == targetID || sourceID == "" {
		return nil, errors.New("invalid fork source")
	}
	unlock := h.sessionOps.lock("fork:" + targetID)
	defer unlock()
	unlockSource := h.sessionOps.lock(sourceID)
	defer unlockSource()
	org, err := h.catalog.GetSessionOrganization(ctx, sourceID)
	if err != nil || org.WorkspaceID == nil || *org.WorkspaceID != workspaceID || org.EffectiveArchived() {
		return nil, errors.New("fork source workspace changed")
	}
	dir := filepath.Join(h.sessionStoreRoot(), "fork-operations")
	path := filepath.Join(dir, toolDigest(targetID)+".json")
	if data, readErr := os.ReadFile(path); readErr == nil {
		var prior forkJournal
		if err := json.Unmarshal(data, &prior); err != nil {
			return nil, err
		}
		if prior.SourceID != sourceID || prior.RequestedTurnID != throughTurnID || prior.Title != title {
			return nil, errCommandIDConflict
		}
		if !prior.Complete {
			return nil, errForkOutcomeUnknown
		}
		stored, err := h.store.Get(ctx, targetID)
		if err != nil {
			return nil, err
		}
		if stored == nil {
			return nil, errors.New("fork target is no longer available")
		}
		return stored, nil
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return nil, readErr
	}
	source, err := h.store.Get(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, errSessionNotFound
	}
	items, _, err := h.items.LoadItems(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	requestedTurnID := throughTurnID
	boundary := -1
	for index, item := range items {
		if item.Type == session.DisplayItemTurnCompleted && item.Turn != nil && (requestedTurnID == "" || item.Turn.ID == requestedTurnID) {
			if boundary < 0 || !item.CreatedAt.Before(items[boundary].CreatedAt) {
				boundary, throughTurnID = index, item.Turn.ID
			}
		}
	}
	if boundary < 0 {
		return nil, errors.New("fork requires an available completed turn")
	}
	var snapshot agent.Snapshot
	switch source.Agent {
	case "", "zotigo":
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return nil, homeErr
		}
		// Match the native worker's TranscriptDir, including custom-store hosts.
		snapshot, err = nativeForkSnapshot(ctx, source, throughTurnID, filepath.Join(home, ".zotigo", "sessions", "compacted"))
		if err != nil {
			return nil, err
		}
	case "codex":
		if h.codexSync == nil {
			return nil, errors.New("codex fork host is unavailable")
		}
	default:
		return nil, fmt.Errorf("fork is unsupported for runtime %q", source.Agent)
	}
	journal := forkJournal{SourceID: sourceID, RequestedTurnID: requestedTurnID, ThroughTurnID: throughTurnID, Title: title}
	if err := saveSessionToolOperation(dir, path, journal); err != nil {
		return nil, err
	}
	child := &session.Session{Metadata: source.Metadata, AgentSnapshot: snapshot,
		ForkedFrom: &session.ForkOrigin{SessionID: sourceID, ThroughTurnID: throughTurnID}}
	child.ID = targetID
	child.CreatedAt, child.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	child.ConversationID = ""
	child.BackendUpdatedAt = time.Time{}
	child.BackendSyncVersion = 0
	child.LastPrompt = ""
	child.Capabilities.ChannelToolsVersion = 0
	if source.Agent == "codex" {
		child.ConversationID, err = forkCodexConversation(ctx, h.codexSync.host, source, throughTurnID)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errForkOutcomeUnknown, err)
		}
	} else {
		child.LastPrompt = sessionadapter.LastUserPrompt(snapshot.History)
		child.ForkPoints = make(map[string]session.ForkPoint)
		for key, point := range source.ForkPoints {
			if point.HistoryLength <= len(snapshot.History) {
				child.ForkPoints[key] = point
			}
		}
	}
	if err := h.store.Put(ctx, child); err != nil {
		return nil, fmt.Errorf("%w: %v", errForkOutcomeUnknown, err)
	}
	if source.Agent == "codex" {
		// The source projection may contain late-arriving items after a terminal
		// marker. Project the verified child history, not a slice of that log.
		lease, acquireErr := h.codexSync.host.Acquire(ctx)
		if acquireErr != nil {
			return nil, fmt.Errorf("%w: %v", errForkOutcomeUnknown, acquireErr)
		}
		turns, readErr := readCodexThreadHistory(ctx, lease.RPC, child.ConversationID)
		_ = lease.Release()
		if readErr != nil {
			return nil, fmt.Errorf("%w: %v", errForkOutcomeUnknown, readErr)
		}
		if err := syncCompletedCodexTurns(ctx, h.items, targetID, turns); err != nil {
			return nil, fmt.Errorf("%w: %v", errForkOutcomeUnknown, err)
		}
	} else {
		// Display history is a projection, never a queue to replay. New input has its
		// own host attribution; inherited command/approval objects cannot be executed.
		cloned := make([]session.DisplayItem, 0, boundary+1)
		for _, item := range items[:boundary+1] {
			if item.Type == session.DisplayItemSessionCommand {
				continue
			}
			// IDs are scoped to a session. Preserve runtime item IDs so later Codex
			// reconciliation recognizes inherited items instead of duplicating them.
			item.Sequence, item.LogOffset = 0, 0
			item.Command, item.Approval, item.Interaction, item.ToolExecution = nil, nil, nil, nil
			cloned = append(cloned, item)
		}
		batch, ok := h.store.(interface {
			AppendDisplayItems(context.Context, string, []session.DisplayItem) ([]session.DisplayItem, error)
		})
		if !ok {
			return nil, errForkOutcomeUnknown
		}
		if _, err = batch.AppendDisplayItems(ctx, targetID, cloned); err != nil {
			return nil, fmt.Errorf("%w: %v", errForkOutcomeUnknown, err)
		}
	}
	if _, err = h.catalog.AssignSession(ctx, targetID, *org.WorkspaceID); err != nil {
		return nil, fmt.Errorf("%w: %v", errForkOutcomeUnknown, err)
	}
	branchTitle := strings.TrimSpace(title)
	if branchTitle == "" {
		branchTitle = "Fork"
		if org.Title != nil {
			branchTitle = boundedToolText(*org.Title, 190) + " · Fork"
		}
	}
	if _, err = h.catalog.SetSessionTitle(ctx, targetID, branchTitle); err != nil {
		return nil, fmt.Errorf("%w: %v", errForkOutcomeUnknown, err)
	}
	journal.Complete = true
	if err := saveSessionToolOperation(dir, path, journal); err != nil {
		return nil, fmt.Errorf("%w: %v", errForkOutcomeUnknown, err)
	}
	return child, nil
}
