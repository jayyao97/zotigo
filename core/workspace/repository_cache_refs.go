package workspace

import (
	"context"
	"strings"
	"time"
)

func (s *Store) publishRepositoryDefault(ctx context.Context, repo Repository, branch, commit string) error {
	s.cacheRefsMu.Lock()
	defer s.cacheRefsMu.Unlock()
	// Hash-named refs let the database publish atomically without overwriting
	// the old backup first. Cleanup cannot remove a not-yet-published candidate.
	if commit != "" {
		if _, err := runGit(ctx, repo.CanonicalPath, "update-ref", "refs/zotigo/default-cache/"+commit, commit); err != nil {
			return err
		}
	}
	now := unixMillis(time.Now().UTC())
	_, err := s.db.ExecContext(ctx, `UPDATE repositories SET default_branch=?,default_commit=?,updated_at=?,last_synced_at=?,last_sync_error='' WHERE id=?`, branch, commit, now, now, repo.ID)
	return err
}

// A plan can select the old default while a concurrent refresh publishes a new
// one. Readers keep its ref reachable until the plan is persisted or abandoned.
// This mutex is held only for bookkeeping/local cleanup, never network work.
func (s *Store) holdDefaultCacheRefs(ctx context.Context) func() {
	s.cacheRefsMu.Lock()
	s.cacheRefReaders++
	s.cacheRefsMu.Unlock()
	return func() {
		s.cacheRefsMu.Lock()
		s.cacheRefReaders--
		var pending map[string]Repository
		if s.cacheRefReaders == 0 {
			pending = s.cacheRefCleanup
			s.cacheRefCleanup = nil
		}
		s.cacheRefsMu.Unlock()
		// Cleanup is bounded even if the creating request was canceled. Failure
		// leaves refs safe for the next successful refresh to collect.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		for _, repo := range pending {
			_ = s.pruneDefaultCacheRefs(cleanupCtx, repo)
		}
	}
}

func (s *Store) pruneDefaultCacheRefs(ctx context.Context, repo Repository) error {
	s.cacheRefsMu.Lock()
	defer s.cacheRefsMu.Unlock()
	if s.cacheRefReaders > 0 {
		if s.cacheRefCleanup == nil {
			s.cacheRefCleanup = make(map[string]Repository)
		}
		s.cacheRefCleanup[repo.ID] = repo
		return nil
	}
	// Use the committed database value: a failed publication must keep the
	// previous default, rather than retain the candidate from that refresh.
	current, err := s.GetRepository(ctx, repo.ID)
	if err != nil {
		return err
	}
	keep := map[string]bool{current.defaultCommit: true}
	rows, err := s.db.QueryContext(ctx, `SELECT c.base_commit FROM workspace_checkouts c
 JOIN source_repositories sr ON sr.source_id=c.source_id
 WHERE sr.repository_id=? AND c.status NOT IN ('ready','archived')`, repo.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var commit string
		if err := rows.Scan(&commit); err != nil {
			_ = rows.Close()
			return err
		}
		keep[commit] = true
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	refs, err := runGit(ctx, repo.CanonicalPath, "for-each-ref", "--format=%(refname) %(objectname)", "refs/zotigo/default-cache/")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(refs), "\n") {
		ref, commit, ok := strings.Cut(line, " ")
		if !ok || keep[commit] {
			continue
		}
		if _, err := runGit(ctx, repo.CanonicalPath, "update-ref", "-d", ref, commit); err != nil {
			return err
		}
	}
	return nil
}
