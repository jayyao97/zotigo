package workerbase

import (
	"context"
	"log"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

type turnUsageRecorder interface {
	RecordTurnUsage(context.Context, zotigosession.TurnUsageRecord) error
}

// RecordTurnUsage adds one finished turn to the usage statistics. It runs at
// the live turn end only, never while history is replayed, so old sessions
// are not backfilled. Statistics must not affect the turn, so failures are
// logged rather than returned.
func RecordTurnUsage(store zotigosession.Store, sessionID, runtime, turnID, status, model string, requestContext *protocol.RequestContext, usage protocol.Usage) {
	recorder, ok := store.(turnUsageRecorder)
	if !ok || turnID == "" {
		return
	}
	// Bound the wait for the shared writer: the turn end waits on this call.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := recorder.RecordTurnUsage(ctx, zotigosession.TurnUsageRecord{
		SessionID: sessionID, TurnID: turnID, Runtime: runtime, Model: model, Status: status,
		Channel: zotigosession.IsChannelRequest(requestContext), CompletedAt: time.Now().UTC(), Usage: usage,
	})
	if err != nil {
		log.Printf("record turn usage session=%s turn=%s: %v", sessionID, turnID, err)
	}
}
