package session

import (
	"context"
	"fmt"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
)

func createTestSession(store Store, workDir string) (*Session, error) {
	sess := &Session{
		Metadata: Metadata{
			ID:               fmt.Sprintf("sess_%d", time.Now().UnixNano()),
			WorkingDirectory: workDir,
			CreatedAt:        time.Now(),
			UpdatedAt:        time.Now(),
		},
		AgentSnapshot: agent.Snapshot{
			State:     agent.StateIdle,
			CreatedAt: time.Now(),
		},
		Turns: make([]Turn, 0),
	}
	sess.UpdatedAt = time.Now()
	if err := store.Put(context.Background(), sess); err != nil {
		return nil, err
	}
	return sess, nil
}
