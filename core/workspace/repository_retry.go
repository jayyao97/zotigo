package workspace

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func repositoryRetryDelay(failures int) time.Duration {
	switch failures {
	case 1:
		return 10 * time.Minute
	case 2:
		return 30 * time.Minute
	case 3:
		return 2 * time.Hour
	default:
		return 6 * time.Hour
	}
}

func (s *Store) recordPreparationFailure(ctx context.Context, id string, cause error) error {
	var shared *repositoryCloneRetry
	if errors.As(cause, &shared) {
		_, err := s.db.ExecContext(ctx, `UPDATE repository_preparations SET next_attempt_at=? WHERE repository_id=?`, shared.at, id)
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var failures int
	if err := tx.QueryRowContext(ctx, `UPDATE repository_preparations SET failure_count=failure_count+1 WHERE repository_id=? RETURNING failure_count`, id).Scan(&failures); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE repository_preparations SET next_attempt_at=? WHERE repository_id=?`, unixMillis(time.Now().Add(repositoryRetryDelay(failures))), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) recordMigrationFailure(ctx context.Context, id string, cause error) error {
	var shared *repositoryCloneRetry
	if errors.As(cause, &shared) {
		_, err := s.db.ExecContext(ctx, `INSERT INTO source_repository_migrations(source_id,status,last_error,next_attempt_at)
 VALUES(?,'retry',?,?) ON CONFLICT(source_id) DO UPDATE SET status='retry',last_error=excluded.last_error,next_attempt_at=excluded.next_attempt_at`, id, cause.Error(), shared.at)
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var failures int
	if err := tx.QueryRowContext(ctx, `INSERT INTO source_repository_migrations(source_id,status,last_error,failure_count)
 VALUES(?,'retry',?,1) ON CONFLICT(source_id) DO UPDATE SET status='retry',last_error=excluded.last_error,failure_count=source_repository_migrations.failure_count+1 RETURNING failure_count`, id, cause.Error()).Scan(&failures); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE source_repository_migrations SET next_attempt_at=? WHERE source_id=?`, unixMillis(time.Now().Add(repositoryRetryDelay(failures))), id); err != nil {
		return err
	}
	return tx.Commit()
}

// Clone backoff belongs to the shared cache, not to any one project/source.
// Callers hold the repository gate when checking or changing this state.
type repositoryCloneRetry struct {
	at    int64
	cause error
}

func (e *repositoryCloneRetry) Error() string {
	if e.cause != nil {
		return e.cause.Error()
	}
	return fmt.Sprintf("repository preparation is waiting to retry at %s", fromUnixMillis(e.at).Format(time.RFC3339))
}
func (e *repositoryCloneRetry) Unwrap() error { return e.cause }

func (s *Store) recordCloneFailure(ctx context.Context, id string, cause error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errors.Join(cause, err)
	}
	defer func() { _ = tx.Rollback() }()
	var failures int
	if err := tx.QueryRowContext(ctx, `UPDATE repositories SET clone_failure_count=clone_failure_count+1 WHERE id=? RETURNING clone_failure_count`, id).Scan(&failures); err != nil {
		return errors.Join(cause, err)
	}
	at := unixMillis(time.Now().Add(repositoryRetryDelay(failures)))
	if _, err := tx.ExecContext(ctx, `UPDATE repositories SET clone_next_attempt_at=? WHERE id=?`, at, id); err != nil {
		return errors.Join(cause, err)
	}
	if err := tx.Commit(); err != nil {
		return errors.Join(cause, err)
	}
	return &repositoryCloneRetry{at: at, cause: cause}
}
