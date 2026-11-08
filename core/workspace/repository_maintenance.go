package workspace

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const repositoryMaintenanceInterval = 10 * time.Minute

// StartRepositoryMaintenance belongs to the serving daemon lifecycle. Opening a
// catalog for inspection or a CLI command never starts Git processes.
func (s *Store) StartRepositoryMaintenance(ctx context.Context, report func(error)) {
	s.startRepositoryMaintenance(ctx, repositoryMaintenanceInterval, report)
}

func (s *Store) startRepositoryMaintenance(ctx context.Context, interval time.Duration, report func(error)) {
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	if s.maintenanceCancel != nil {
		return
	}
	ctx, s.maintenanceCancel = context.WithCancel(ctx)
	s.maintenanceDone = make(chan struct{})
	s.maintenanceWake = make(chan struct{}, 1)
	go func() {
		defer close(s.maintenanceDone)
		// Initial clones may be slow. They must not delay refreshing caches
		// already used by other projects.
		var tasks sync.WaitGroup
		for _, migration := range []bool{true, false} {
			tasks.Add(1)
			go func() {
				defer tasks.Done()
				ticker := time.NewTicker(interval)
				defer ticker.Stop()
				var wake <-chan struct{}
				if migration {
					wake = s.maintenanceWake
				}
				for ctx.Err() == nil {
					var err error
					if migration {
						err = s.migrateRepositorySources(ctx)
					} else {
						err = s.maintainRepositories(ctx)
					}
					if err != nil && ctx.Err() == nil && report != nil {
						report(err)
					}
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
					case <-wake:
					}
				}
			}()
		}
		tasks.Wait()
	}()
}

func (s *Store) wakeRepositoryMigration() {
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	if s.maintenanceWake != nil {
		select {
		case s.maintenanceWake <- struct{}{}:
		default:
		}
	}
}

func (s *Store) stopRepositoryMaintenance() {
	s.maintenanceMu.Lock()
	cancel, done := s.maintenanceCancel, s.maintenanceDone
	s.maintenanceMu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (s *Store) maintainRepositories(ctx context.Context) error {
	var result error
	repos, err := s.ListRepositories(ctx)
	if err != nil {
		return errors.Join(result, err)
	}
	for _, repo := range repos {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Newly migrated repositories were just synchronized; do not fetch twice.
		if repo.LastSyncedAt == nil || time.Since(*repo.LastSyncedAt) < repositoryMaintenanceInterval {
			continue
		}
		if _, err := s.refreshRepository(ctx, repo); err != nil {
			result = errors.Join(result, fmt.Errorf("refresh repository %s: %w", repo.ID, err))
		}
	}
	return result
}

// Eligibility checks are durable. No-remote sources are not repeatedly probed
// on every list/create request; transient failures retry in a later pass.
func (s *Store) migrateRepositorySources(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT s.id,s.project_id,s.kind,s.canonical_path,s.git_common_dir,s.git_object_format,s.folder_mode,s.source_key,s.created_at,s.updated_at
 FROM sources s JOIN projects p ON p.id=s.project_id
 LEFT JOIN source_repositories sr ON sr.source_id=s.id
 LEFT JOIN source_repository_migrations m ON m.source_id=s.id
 WHERE s.registered=1 AND s.kind='git' AND p.status='active' AND sr.source_id IS NULL
 AND (m.source_id IS NULL OR (m.status IN ('pending','retry') AND m.next_attempt_at <= ?))`, unixMillis(time.Now().UTC()))
	if err != nil {
		return err
	}
	var sources []Source
	for rows.Next() {
		source, err := scanSource(rows)
		if err != nil {
			_ = rows.Close()
			return err
		}
		sources = append(sources, source)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	var result error
	for _, source := range sources {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.migrateRepositorySource(ctx, source); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			_, recordErr := s.db.ExecContext(ctx, `INSERT INTO source_repository_migrations(source_id,status,last_error,next_attempt_at)
 VALUES(?,'retry',?,?) ON CONFLICT(source_id) DO UPDATE SET status='retry',last_error=excluded.last_error,next_attempt_at=excluded.next_attempt_at`, source.ID, err.Error(), unixMillis(time.Now().Add(repositoryMaintenanceInterval)))
			result = errors.Join(result, fmt.Errorf("migrate source %s: %w", source.ID, err), recordErr)
		}
	}
	return result
}

func (s *Store) migrateRepositorySource(ctx context.Context, source Source) error {
	if err := verifySourceIdentity(ctx, source); err != nil {
		return err
	}
	remotes, err := runRepositoryGit(ctx, 5*time.Second, source.CanonicalPath, "remote")
	if err != nil {
		return err
	}
	names := strings.Fields(remotes)
	if len(names) != 1 {
		return s.markLocalRepositorySource(ctx, source.ID)
	}
	remote, err := runRepositoryGit(ctx, 5*time.Second, source.CanonicalPath, "remote", "get-url", names[0])
	if err != nil {
		return err
	}
	remote = strings.TrimSpace(remote)
	if !filepath.IsAbs(remote) && !strings.Contains(remote, ":") {
		resolved, err := canonicalDirectory(filepath.Join(source.CanonicalPath, remote))
		if err != nil {
			return err
		}
		remote = resolved
	}
	if validateRemote(remote) != nil {
		return s.markLocalRepositorySource(ctx, source.ID)
	}
	s.operationMu.Lock()
	repo, err := s.registerRepository(ctx, remote, filepath.Base(source.CanonicalPath))
	s.operationMu.Unlock()
	if errors.Is(err, ErrInvalid) {
		return s.markLocalRepositorySource(ctx, source.ID)
	}
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO source_repository_migrations(source_id,repository_id,status)
 VALUES(?,?,'pending') ON CONFLICT(source_id) DO UPDATE SET repository_id=excluded.repository_id,status='pending'`, source.ID, repo.ID)
	if err != nil {
		return err
	}
	unlock, err := s.lockRepository(ctx, repo.ID)
	if err != nil {
		return err
	}
	defer unlock()
	repo, err = s.refreshRepositoryLocked(ctx, repo)
	if err != nil {
		return err
	}
	if repo.DefaultBranch == "" {
		return fmt.Errorf("%w: remote default branch is not available yet", ErrConflict)
	}
	prefix := "refs/zotigo/imported/" + source.ID + "/"
	if _, err := runRepositoryGit(ctx, 5*time.Minute, repo.CanonicalPath, "fetch", "--no-tags", "--", source.CanonicalPath, "+refs/heads/*:"+prefix+"heads/*", "+refs/tags/*:"+prefix+"tags/*"); err != nil {
		return fmt.Errorf("preserve original local refs: %w", err)
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	return s.replaceLocalSource(ctx, source, repo)
}

func (s *Store) markLocalRepositorySource(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO source_repository_migrations(source_id,status) VALUES(?,'local')
 ON CONFLICT(source_id) DO UPDATE SET status='local',last_error='',next_attempt_at=0`, id)
	return err
}
