package agent

import (
	"fmt"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
)

func (a *Agent) startNewTurn(userPrompt string, requestContext *protocol.RequestContext) {
	turnID := fmt.Sprintf("turn_%d", time.Now().UnixNano())
	now := time.Now()
	a.turnSafety = TurnSafetyState{
		TurnID:            turnID,
		CurrentUserPrompt: userPrompt,
		RequestContext:    requestContext.Clone(),
	}
	a.turns = append(a.turns, TurnAudit{
		ID:                turnID,
		CreatedAt:         now,
		UpdatedAt:         now,
		UserPromptSummary: summarizeText(userPrompt, 240),
		SnapshotStatus:    SnapshotStatusNotNeeded,
		SafetyEvents:      make([]AuditEvent, 0),
	})
}

func (a *Agent) currentTurnIndex() int {
	if a.turnSafety.TurnID == "" {
		return -1
	}
	for i := len(a.turns) - 1; i >= 0; i-- {
		if a.turns[i].ID == a.turnSafety.TurnID {
			return i
		}
	}
	return -1
}

func (a *Agent) appendSafetyEvent(event AuditEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	if event.TurnID == "" {
		event.TurnID = a.turnSafety.TurnID
	}
	if event.ContextSummary.RequestContext == nil {
		event.ContextSummary.RequestContext = a.turnSafety.RequestContext.Clone()
	}

	idx := a.currentTurnIndex()
	if idx == -1 {
		now := time.Now()
		turnID := a.turnSafety.TurnID
		if turnID == "" {
			turnID = fmt.Sprintf("turn_%d", now.UnixNano())
			a.turnSafety.TurnID = turnID
		}
		a.turns = append(a.turns, TurnAudit{
			ID:             turnID,
			CreatedAt:      now,
			UpdatedAt:      now,
			SnapshotStatus: SnapshotStatusNotNeeded,
			SafetyEvents:   make([]AuditEvent, 0),
		})
		idx = len(a.turns) - 1
	}

	a.turns[idx].UpdatedAt = time.Now()
	a.turns[idx].SafetyEvents = append(a.turns[idx].SafetyEvents, event)
}

func (a *Agent) setTurnSnapshot(status SnapshotStatus, snapshotID string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	idx := a.currentTurnIndex()
	if idx == -1 {
		return
	}
	a.turns[idx].UpdatedAt = time.Now()
	a.turns[idx].SnapshotStatus = status
	a.turns[idx].SnapshotID = snapshotID
	a.turnSafety.SnapshotID = snapshotID
	a.turnSafety.SnapshotCreated = status == SnapshotStatusCreated
	a.turnSafety.SnapshotFailed = status == SnapshotStatusFailed
	a.turnSafety.SnapshotAttempted = status == SnapshotStatusCreated || status == SnapshotStatusFailed
}
