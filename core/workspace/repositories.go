package workspace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Repository is a daemon-owned Git cache shared by project source references.
// Existing worktrees keep their original source identity until they are removed.
type Repository struct {
	defaultCommit string
	LastSyncedAt  *time.Time `json:"last_synced_at,omitempty"`
	LastSyncError string     `json:"last_sync_error,omitempty"`
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	RemoteURL     string     `json:"remote_url"`
	CanonicalPath string     `json:"canonical_path"`
	DefaultBranch string     `json:"default_branch"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

const repositoryColumns = `id, name, remote_url, canonical_path, default_branch, created_at, updated_at, last_synced_at, last_sync_error, default_commit`

func scanRepository(row scanner) (Repository, error) {
	var repo Repository
	var created, updated int64
	var synced sql.NullInt64
	if err := row.Scan(&repo.ID, &repo.Name, &repo.RemoteURL, &repo.CanonicalPath, &repo.DefaultBranch, &created, &updated, &synced, &repo.LastSyncError, &repo.defaultCommit); err != nil {
		return repo, scanError("repository", err)
	}
	repo.CreatedAt, repo.UpdatedAt = fromUnixMillis(created), fromUnixMillis(updated)
	repo.LastSyncedAt = nullableTime(synced)
	return repo, nil
}

func (s *Store) GetRepository(ctx context.Context, id string) (Repository, error) {
	return scanRepository(s.db.QueryRowContext(ctx, `SELECT `+repositoryColumns+` FROM repositories WHERE id = ?`, id))
}

func (s *Store) ListRepositories(ctx context.Context) ([]Repository, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+repositoryColumns+` FROM repositories ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	repos := make([]Repository, 0)
	for rows.Next() {
		repo, err := scanRepository(rows)
		if err != nil {
			return nil, err
		}
		repos = append(repos, repo)
	}
	return repos, rows.Err()
}

func validateRemote(remote string) error {
	if remote == "" || strings.HasPrefix(remote, "-") || strings.ContainsAny(remote, "\x00\r\n\t") {
		return fmt.Errorf("%w: provide a Git remote URL or absolute repository path", ErrInvalid)
	}
	if filepath.IsAbs(remote) {
		return nil
	}
	if strings.Contains(remote, " ") {
		return fmt.Errorf("%w: spaces must be escaped in remote URLs", ErrInvalid)
	}
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "ssh" && u.Scheme != "git" && u.Scheme != "file") || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("%w: unsupported Git remote URL", ErrInvalid)
		}
		if u.User != nil {
			if _, ok := u.User.Password(); ok || (u.Scheme != "ssh") {
				return fmt.Errorf("%w: use the daemon's Git credentials, not credentials in the URL", ErrInvalid)
			}
		}
		if (u.Scheme == "file" && (u.Host != "" || !filepath.IsAbs(u.Path))) || (u.Scheme != "file" && (u.Host == "" || u.Path == "")) {
			return fmt.Errorf("%w: invalid Git remote URL", ErrInvalid)
		}
		return nil
	}
	// SCP-style SSH addresses; reject external remote helpers such as ext::.
	left, right, ok := strings.Cut(remote, ":")
	if !ok || left == "" || right == "" || strings.ContainsAny(left, "/\\") || strings.HasPrefix(right, ":") {
		return fmt.Errorf("%w: unsupported Git remote URL", ErrInvalid)
	}
	return nil
}

func (s *Store) registerRepository(ctx context.Context, remote, name string) (Repository, error) {
	remote = strings.TrimSpace(remote)
	if err := validateRemote(remote); err != nil {
		return Repository{}, err
	}
	existing, err := scanRepository(s.db.QueryRowContext(ctx, `SELECT `+repositoryColumns+` FROM repositories WHERE remote_url = ?`, remote))
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Repository{}, err
	}
	if name == "" {
		name = strings.TrimSuffix(path.Base(strings.TrimRight(remote, "/")), ".git")
		if _, suffix, ok := strings.Cut(name, ":"); ok {
			name = suffix
		}
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || len(name) > 100 || strings.ContainsAny(name, "/\\\x00\r\n") {
		return Repository{}, fmt.Errorf("%w: invalid repository name", ErrInvalid)
	}
	now := time.Now().UTC()
	id := "repo_" + uuid.NewString()
	root, err := canonicalDirectory(s.rootDir)
	if err != nil {
		return Repository{}, err
	}
	canonicalPath := filepath.Join(root, "repositories", id, name)
	_, err = s.db.ExecContext(ctx, `INSERT INTO repositories(id,name,remote_url,canonical_path,created_at,updated_at) VALUES(?,?,?,?,?,?)`, id, name, remote, canonicalPath, unixMillis(now), unixMillis(now))
	if err != nil {
		return Repository{}, err
	}
	return s.GetRepository(ctx, id)
}

func (s *Store) CreateRepository(ctx context.Context, remote, name string) (Repository, error) {
	s.operationMu.Lock()
	repo, err := s.registerRepository(ctx, remote, name)
	s.operationMu.Unlock()
	if err != nil {
		return Repository{}, err
	}
	return s.refreshRepository(ctx, repo)
}

// refreshRepository never modifies a user checkout. Failed clones remain retryable.
func (s *Store) refreshRepository(ctx context.Context, repo Repository) (Repository, error) {
	unlock, err := s.lockRepository(ctx, repo.ID)
	if err != nil {
		return Repository{}, err
	}
	defer unlock()
	return s.refreshRepositoryLocked(ctx, repo)
}

// Repository network operations are serialized independently of catalog writes.
// Waiting for another request must remain cancellable as well.
func (s *Store) lockRepository(ctx context.Context, id string) (func(), error) {
	s.repositoryMu.Lock()
	if s.repositoryLocks == nil {
		s.repositoryLocks = make(map[string]chan struct{})
	}
	gate := s.repositoryLocks[id]
	if gate == nil {
		gate = make(chan struct{}, 1)
		s.repositoryLocks[id] = gate
	}
	s.repositoryMu.Unlock()
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func runRepositoryGit(ctx context.Context, timeout time.Duration, directory string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return runGit(ctx, directory, args...)
}

func (s *Store) refreshRepositoryLocked(ctx context.Context, repo Repository) (result Repository, resultErr error) {
	defer func() {
		if resultErr != nil && ctx.Err() == nil {
			_, _ = s.db.ExecContext(ctx, `UPDATE repositories SET last_sync_error=? WHERE id=?`, resultErr.Error(), repo.ID)
		}
	}()
	if _, err := os.Stat(repo.CanonicalPath); os.IsNotExist(err) {
		parent := filepath.Dir(repo.CanonicalPath)
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return Repository{}, err
		}
		staging, err := os.MkdirTemp(parent, "clone-")
		if err != nil {
			return Repository{}, err
		}
		defer func() { _ = os.RemoveAll(staging) }()
		// Clone the remote into an independent object store. Import local-only
		// branches under a separate namespace rather than overwriting remote refs.
		args := []string{"clone", "--no-checkout", "--no-hardlinks"}
		var reference string
		err = s.db.QueryRowContext(ctx, `SELECT s.canonical_path FROM sources s JOIN source_repository_migrations m ON m.source_id=s.id WHERE m.repository_id=? AND m.status IN ('pending','retry') ORDER BY s.id LIMIT 1`, repo.ID).Scan(&reference)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Repository{}, err
		}
		if reference != "" {
			args = append(args, "--reference-if-able", reference, "--dissociate")
		}
		args = append(args, "--", repo.RemoteURL, staging)
		if _, err := runRepositoryGit(ctx, 30*time.Minute, parent, args...); err != nil {
			return Repository{}, fmt.Errorf("clone repository failed; check remote access on this daemon: %w", err)
		}
		if err := os.Rename(staging, repo.CanonicalPath); err != nil {
			return Repository{}, err
		}
	} else if err != nil {
		return Repository{}, err
	}
	if err := verifyRepositoryIdentity(ctx, repo); err != nil {
		return Repository{}, err
	}
	if _, err := runRepositoryGit(ctx, 5*time.Minute, repo.CanonicalPath, "fetch", "--prune", "origin", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return Repository{}, &repositorySyncError{fmt.Errorf("fetch repository failed; check remote access on this daemon: %w", err)}
	}
	output, err := runRepositoryGit(ctx, time.Minute, repo.CanonicalPath, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return Repository{}, &repositorySyncError{fmt.Errorf("detect remote default branch: %w", err)}
	}
	branch := ""
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "ref: refs/heads/") && strings.HasSuffix(line, "\tHEAD") {
			branch = strings.TrimSuffix(strings.TrimPrefix(line, "ref: refs/heads/"), "\tHEAD")
		}
	}
	defaultCommit := ""
	if branch != "" {
		resolved, err := runGit(ctx, repo.CanonicalPath, "rev-parse", "--verify", "refs/remotes/origin/"+branch+"^{commit}")
		if err != nil {
			return Repository{}, fmt.Errorf("%w: remote default branch is unavailable", ErrConflict)
		}
		defaultCommit = strings.TrimSpace(resolved)
		// A later fetch can prune or force-update origin refs before failing. Keep
		// successful default commits independently reachable for offline fallback.
		if _, err := runGit(ctx, repo.CanonicalPath, "update-ref", "refs/zotigo/default-cache/"+defaultCommit, defaultCommit); err != nil {
			return Repository{}, err
		}
	}
	_, err = s.db.ExecContext(ctx, `UPDATE repositories SET default_branch = ?, default_commit = ?, updated_at = ?, last_synced_at = ?, last_sync_error = '' WHERE id = ?`, branch, defaultCommit, unixMillis(time.Now().UTC()), unixMillis(time.Now().UTC()), repo.ID)
	if err != nil {
		return Repository{}, err
	}
	return s.GetRepository(ctx, repo.ID)
}

func (s *Store) AddRepositorySource(ctx context.Context, projectID, repositoryID string) (Source, error) {
	if err := s.requireActiveProject(ctx, projectID); err != nil {
		return Source{}, err
	}
	repo, err := s.GetRepository(ctx, repositoryID)
	if err != nil {
		return Source{}, err
	}
	if repo.LastSyncedAt == nil {
		repo, err = s.refreshRepository(ctx, repo)
	} else {
		var unlock func()
		repo, _, unlock, err = s.refreshRepositoryForUse(ctx, repo)
		unlock()
	}
	if err != nil {
		return Source{}, err
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	if err := s.requireActiveProject(ctx, projectID); err != nil {
		return Source{}, err
	}
	inspection, err := InspectSource(ctx, repo.CanonicalPath)
	if err != nil {
		return Source{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Source{}, err
	}
	defer func() { _ = tx.Rollback() }()
	now := unixMillis(time.Now().UTC())
	var sourceID string
	err = tx.QueryRowContext(ctx, `INSERT INTO sources(id,project_id,kind,canonical_path,git_common_dir,git_object_format,source_key,created_at,updated_at) VALUES(?,?,'git',?,?,?,?,?,?) ON CONFLICT(project_id,canonical_path) DO UPDATE SET registered=1,updated_at=excluded.updated_at RETURNING id`, "source_"+uuid.NewString(), projectID, inspection.CanonicalPath, inspection.GitCommonDir, inspection.GitObjectFormat, inspection.SourceKey, now, now).Scan(&sourceID)
	if err != nil {
		return Source{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO source_repositories(source_id,repository_id) VALUES(?,?) ON CONFLICT(source_id) DO NOTHING`, sourceID, repo.ID)
	if err != nil {
		return Source{}, err
	}
	if err := tx.Commit(); err != nil {
		return Source{}, err
	}
	return s.GetSource(ctx, projectID, sourceID)
}

func (s *Store) sourceRepository(ctx context.Context, source Source) (Source, error) {
	var id, remote, branch, syncError string
	var synced sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT r.id,r.remote_url,r.default_branch,r.last_synced_at,r.last_sync_error FROM repositories r JOIN source_repositories sr ON sr.repository_id=r.id WHERE sr.source_id=?`, source.ID).Scan(&id, &remote, &branch, &synced, &syncError)
	if errors.Is(err, sql.ErrNoRows) {
		return source, nil
	}
	if err != nil {
		return Source{}, err
	}
	source.RepositoryID, source.RemoteURL, source.DefaultBranch = id, remote, branch
	source.LastSyncedAt, source.LastSyncError = nullableTime(synced), syncError
	return source, nil
}

func (s *Store) prepareRepositorySelection(ctx context.Context, projectID string, selection WorkspaceSourceInput) (WorkspaceSourceInput, error) {
	// Take a consistent registration snapshot before any network work. A
	// background migration may otherwise switch IDs between these reads.
	source, err := func() (Source, error) {
		s.operationMu.Lock()
		defer s.operationMu.Unlock()
		var replacement sql.NullString
		err := s.db.QueryRowContext(ctx, `SELECT replacement_source_id FROM source_repositories WHERE source_id=?`, selection.SourceID).Scan(&replacement)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Source{}, err
		}
		if replacement.Valid {
			selection.SourceID = replacement.String
		}
		source, err := s.GetSource(ctx, projectID, selection.SourceID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return Source{}, fmt.Errorf("%w: workspace source must be registered in the project", ErrInvalid)
			}
			return Source{}, err
		}
		var registered bool
		if err := s.db.QueryRowContext(ctx, `SELECT registered FROM sources WHERE id=?`, source.ID).Scan(&registered); err != nil {
			return Source{}, err
		}
		if !registered {
			return Source{}, fmt.Errorf("%w: workspace source must be registered in the project", ErrInvalid)
		}

		return source, nil
	}()
	if err != nil {
		return selection, err
	}
	if source.RepositoryID == "" {
		selection.preparedLocalSource = true
		return selection, nil
	}
	repo, err := s.GetRepository(ctx, source.RepositoryID)
	if err != nil {
		return selection, err
	}
	repo, usedCache, unlock, err := s.refreshRepositoryForUse(ctx, repo)
	if err != nil {
		return selection, err
	}
	defer unlock()
	selection.usedCachedBase, selection.baseSyncedAt = usedCache, repo.LastSyncedAt
	ref := strings.TrimSpace(selection.BaseRef)
	defaultRequested := ref == "" || ref == "HEAD"
	if defaultRequested {
		ref = repo.DefaultBranch
	}
	if ref == "" {
		return selection, fmt.Errorf("%w: remote default branch could not be detected; choose a base branch", ErrInvalid)
	}
	if strings.HasPrefix(ref, "refs/zotigo/imported/") {
		selection.BaseRef = ref
	} else {
		// Advertised default names are literal, including an origin/ prefix.
		if ref != repo.DefaultBranch {
			if strings.HasPrefix(ref, "refs/remotes/origin/") {
				ref = strings.TrimPrefix(ref, "refs/remotes/origin/")
			} else if strings.HasPrefix(ref, "refs/heads/") {
				ref = strings.TrimPrefix(ref, "refs/heads/")
			} else if strings.HasPrefix(ref, "origin/") {
				// Prefer an actual branch named origin/foo to the shorthand for foo.
				if _, err := runGit(ctx, repo.CanonicalPath, "show-ref", "--verify", "refs/remotes/origin/"+ref); err != nil {
					ref = strings.TrimPrefix(ref, "origin/")
				}
			}

		}
		if _, err := runGit(ctx, repo.CanonicalPath, "check-ref-format", "refs/heads/"+ref); err != nil {
			return selection, fmt.Errorf("%w: invalid base branch", ErrInvalid)
		}
		selection.BaseRef = "refs/remotes/origin/" + ref
	}
	resolveRef := selection.BaseRef
	if usedCache && selection.BaseRef == "refs/remotes/origin/"+repo.DefaultBranch && repo.defaultCommit != "" {
		resolveRef = "refs/zotigo/default-cache/" + repo.defaultCommit
	}
	commit, err := runGit(ctx, repo.CanonicalPath, "rev-parse", "--verify", resolveRef+"^{commit}")
	if err != nil {
		return selection, fmt.Errorf("%w: base ref is unavailable", ErrConflict)
	}
	selection.resolvedBaseCommit = strings.TrimSpace(commit)
	return selection, nil
}

func (s *Store) replaceLocalSource(ctx context.Context, source Source, repo Repository) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var registered bool
	if err := tx.QueryRowContext(ctx, `SELECT s.registered AND p.status='active' FROM sources s JOIN projects p ON p.id=s.project_id WHERE s.id=?`, source.ID).Scan(&registered); err != nil {
		return err
	}
	if !registered {
		return fmt.Errorf("%w: original source is no longer active", ErrConflict)
	}
	var replacement string
	err = tx.QueryRowContext(ctx, `SELECT id FROM sources WHERE project_id=? AND canonical_path=?`, source.ProjectID, repo.CanonicalPath).Scan(&replacement)
	if errors.Is(err, sql.ErrNoRows) {
		replacement = "source_" + uuid.NewString()
		common := filepath.Join(repo.CanonicalPath, ".git")
		_, err = tx.ExecContext(ctx, `INSERT INTO sources(id,project_id,kind,canonical_path,git_common_dir,git_object_format,source_key,created_at,updated_at) VALUES(?,?,'git',?,?,?,?,?,?)`, replacement, source.ProjectID, repo.CanonicalPath, common, source.GitObjectFormat, sourceKey(SourceKindGit, common), unixMillis(source.CreatedAt), unixMillis(time.Now().UTC()))
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sources SET registered=1 WHERE id=?`, replacement); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO source_repositories(source_id,repository_id) VALUES(?,?) ON CONFLICT(source_id) DO NOTHING`, replacement, repo.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO source_repositories(source_id,repository_id,replacement_source_id) VALUES(?,?,?)`, source.ID, repo.ID, replacement); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sources SET registered=0 WHERE id=?`, source.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE source_repository_migrations SET status='complete',last_error='',next_attempt_at=0 WHERE source_id=?`, source.ID); err != nil {
		return err
	}
	return tx.Commit()
}
