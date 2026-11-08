package workspace

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Explicit remote registration is durable and returns before network work. A
// dedicated worker prepares this queue independently of migration and cache refresh.
func (s *Store) prepareRegisteredRepositories(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT repository_id FROM repository_preparations WHERE next_attempt_at <= ? ORDER BY next_attempt_at,repository_id`, unixMillis(time.Now().UTC()))
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	var result error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		repo, err := s.GetRepository(ctx, id)
		if err == nil && repo.LastSyncedAt == nil {
			unlock, acquired := s.tryLockRepository(repo.ID)
			if !acquired {
				continue
			}
			_, err = func() (Repository, error) {
				defer unlock()
				current, err := s.GetRepository(ctx, id)
				if err != nil || current.LastSyncedAt != nil {
					return current, err
				}
				return s.refreshRepositoryLocked(ctx, current)
			}()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			recordErr := s.recordPreparationFailure(ctx, id, err)
			result = errors.Join(result, fmt.Errorf("prepare repository %s: %w", id, err), recordErr)
			continue
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM repository_preparations WHERE repository_id=?`, id); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}
