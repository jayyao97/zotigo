package workspace

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

type repositorySyncError struct{ error }

func verifyRepositoryIdentity(ctx context.Context, repo Repository) error {
	inspection, err := InspectSource(ctx, repo.CanonicalPath)
	if err != nil {
		return err
	}
	if inspection.Kind != SourceKindGit || inspection.CanonicalPath != repo.CanonicalPath || inspection.GitCommonDir != filepath.Join(repo.CanonicalPath, ".git") {
		return fmt.Errorf("%w: managed repository identity changed", ErrConflict)
	}
	origin, err := runGit(ctx, repo.CanonicalPath, "config", "--get", "remote.origin.url")
	if err != nil || strings.TrimSpace(origin) != repo.RemoteURL {
		return fmt.Errorf("%w: managed repository remote changed", ErrConflict)
	}
	return nil
}

// A foreground request waits briefly for fresh refs. A background refresh may
// still own the repository gate: Git ref updates are atomic, so its previously
// successful cache can be read without waiting for the network operation.
func (s *Store) refreshRepositoryForUse(ctx context.Context, repo Repository) (Repository, bool, func(), error) {
	return s.refreshRepositoryForUseWithin(ctx, repo, 10*time.Second)
}

func (s *Store) refreshRepositoryForUseWithin(ctx context.Context, repo Repository, timeout time.Duration) (Repository, bool, func(), error) {
	nothing := func() {}
	refreshCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	unlock, refreshErr := s.lockRepository(refreshCtx, repo.ID)
	if refreshErr == nil {
		// Do not start a new clone on a workspace creation path. Managed
		// sources are published only after preparation has succeeded.
		if err := verifyRepositoryIdentity(ctx, repo); err != nil {
			unlock()
			return Repository{}, false, nothing, err
		}
		fresh, err := s.refreshRepositoryLocked(refreshCtx, repo)
		if err == nil {
			return fresh, false, unlock, nil
		}
		refreshErr = err
		unlock()
	}
	if err := ctx.Err(); err != nil {
		return Repository{}, false, nothing, err
	}
	var syncErr *repositorySyncError
	if refreshCtx.Err() == nil && !errors.As(refreshErr, &syncErr) {
		return Repository{}, false, nothing, refreshErr
	}
	cached, err := s.GetRepository(ctx, repo.ID)
	if err != nil {
		return Repository{}, false, nothing, err
	}
	if cached.LastSyncedAt == nil {
		return Repository{}, false, nothing, fmt.Errorf("repository has no successfully synchronized cache: %w", refreshErr)
	}
	if err := verifyRepositoryIdentity(ctx, cached); err != nil {
		return Repository{}, false, nothing, err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE repositories SET last_sync_error=? WHERE id=?`, refreshErr.Error(), repo.ID)
	if err != nil {
		return Repository{}, false, nothing, err
	}
	cached.LastSyncError = refreshErr.Error()
	return cached, true, nothing, nil
}

func nullableTimeMillis(value *time.Time) any {
	if value == nil {
		return nil
	}
	return unixMillis(*value)
}
