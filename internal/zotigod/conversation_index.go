package zotigod

import (
	"context"
	"log"
	"time"

	"github.com/jayyao97/zotigo/core/session"
)

func maintainConversationIndex(ctx context.Context, store *session.FileStore, logger *log.Logger) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		if err := store.RebuildDisplayIndex(ctx); err != nil && ctx.Err() == nil && logger != nil {
			logger.Printf("Conversation index maintenance: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
