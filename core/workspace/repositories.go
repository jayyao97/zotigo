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
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	RemoteURL     string    `json:"remote_url"`
	CanonicalPath string    `json:"canonical_path"`
	DefaultBranch string    `json:"default_branch"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

const repositoryColumns = `id, name, remote_url, canonical_path, default_branch, created_at, updated_at`

func scanRepository(row scanner) (Repository, error) {
	var repo Repository
	var created, updated int64
	if err := row.Scan(&repo.ID, &repo.Name, &repo.RemoteURL, &repo.CanonicalPath, &repo.DefaultBranch, &created, &updated); err != nil {
		return repo, scanError("repository", err)
	}
	repo.CreatedAt, repo.UpdatedAt = fromUnixMillis(created), fromUnixMillis(updated)
	return repo, nil
}

func (s *Store) GetRepository(ctx context.Context, id string) (Repository, error) {
	return scanRepository(s.db.QueryRowContext(ctx, `SELECT `+repositoryColumns+` FROM repositories WHERE id = ?`, id))
}

func (s *Store) ListRepositories(ctx context.Context) ([]Repository, error) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	if err := s.migrateRepositorySources(ctx); err != nil {
		return nil, err
	}
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

// refreshRepository never modifies a user checkout, and never silently uses a
// stale remote ref when fetch fails. Failed clones can be retried independently.
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

func (s *Store) refreshRepositoryLocked(ctx context.Context, repo Repository) (Repository, error) {
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
		err = s.db.QueryRowContext(ctx, `SELECT s.canonical_path FROM sources s JOIN source_repositories sr ON sr.source_id=s.id WHERE sr.repository_id=? AND sr.replacement_source_id IS NOT NULL ORDER BY s.id LIMIT 1`, repo.ID).Scan(&reference)
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
	inspection, err := InspectSource(ctx, repo.CanonicalPath)
	if err != nil {
		return Repository{}, err
	}
	if inspection.Kind != SourceKindGit || inspection.CanonicalPath != repo.CanonicalPath || inspection.GitCommonDir != filepath.Join(repo.CanonicalPath, ".git") {
		return Repository{}, fmt.Errorf("%w: managed repository identity changed", ErrConflict)
	}
	origin, err := runGit(ctx, repo.CanonicalPath, "config", "--get", "remote.origin.url")
	if err != nil || strings.TrimSpace(origin) != repo.RemoteURL {
		return Repository{}, fmt.Errorf("%w: managed repository remote changed", ErrConflict)
	}
	if err := s.importLocalRepositoryRefs(ctx, repo); err != nil {
		return Repository{}, err
	}
	if _, err := runRepositoryGit(ctx, 5*time.Minute, repo.CanonicalPath, "fetch", "--prune", "origin", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return Repository{}, fmt.Errorf("fetch repository failed; check remote access on this daemon: %w", err)
	}
	output, err := runRepositoryGit(ctx, time.Minute, repo.CanonicalPath, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return Repository{}, fmt.Errorf("detect remote default branch: %w", err)
	}
	branch := ""
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "ref: refs/heads/") && strings.HasSuffix(line, "\tHEAD") {
			branch = strings.TrimSuffix(strings.TrimPrefix(line, "ref: refs/heads/"), "\tHEAD")
		}
	}
	if branch != "" {
		if _, err := runGit(ctx, repo.CanonicalPath, "rev-parse", "--verify", "refs/remotes/origin/"+branch+"^{commit}"); err != nil {
			return Repository{}, fmt.Errorf("%w: remote default branch is unavailable", ErrConflict)
		}
	}
	_, err = s.db.ExecContext(ctx, `UPDATE repositories SET default_branch = ?, updated_at = ? WHERE id = ?`, branch, unixMillis(time.Now().UTC()), repo.ID)
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
	repo, err = s.refreshRepository(ctx, repo)
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

func (s *Store) importLocalRepositoryRefs(ctx context.Context, repo Repository) error {
	rows, err := s.db.QueryContext(ctx, `SELECT s.id,s.canonical_path FROM source_repositories sr JOIN sources s ON s.id=sr.source_id WHERE sr.repository_id=? AND sr.replacement_source_id IS NOT NULL AND sr.imported=0`, repo.ID)
	if err != nil {
		return err
	}
	type seed struct{ id, path string }
	var seeds []seed
	for rows.Next() {
		var item seed
		if err := rows.Scan(&item.id, &item.path); err != nil {
			_ = rows.Close()
			return err
		}
		seeds = append(seeds, item)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, seed := range seeds {
		// The originals are never deleted by migration. A missing original can
		// no longer supply its local-only refs, but must not block remote use.
		if _, err := os.Stat(seed.path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		prefix := "refs/zotigo/imported/" + seed.id + "/"
		if _, err := runRepositoryGit(ctx, 5*time.Minute, repo.CanonicalPath, "fetch", "--no-tags", "--", seed.path, "+refs/heads/*:"+prefix+"heads/*", "+refs/tags/*:"+prefix+"tags/*"); err != nil {
			return fmt.Errorf("preserve original local refs: %w", err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE source_repositories SET imported=1 WHERE source_id=?`, seed.id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) sourceRepository(ctx context.Context, source Source) (Source, error) {
	var id, remote, branch string
	err := s.db.QueryRowContext(ctx, `SELECT r.id,r.remote_url,r.default_branch FROM repositories r JOIN source_repositories sr ON sr.repository_id=r.id WHERE sr.source_id=?`, source.ID).Scan(&id, &remote, &branch)
	if errors.Is(err, sql.ErrNoRows) {
		return source, nil
	}
	if err != nil {
		return Source{}, err
	}
	source.RepositoryID, source.RemoteURL, source.DefaultBranch = id, remote, branch
	return source, nil
}

func (s *Store) prepareRepositorySelection(ctx context.Context, projectID string, selection WorkspaceSourceInput) (WorkspaceSourceInput, error) {
	var replacement sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT replacement_source_id FROM source_repositories WHERE source_id=?`, selection.SourceID).Scan(&replacement)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return selection, err
	}
	if replacement.Valid {
		selection.SourceID = replacement.String
	}
	source, err := s.GetSource(ctx, projectID, selection.SourceID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return selection, fmt.Errorf("%w: workspace source must be registered in the project", ErrInvalid)
		}
		return selection, err
	}
	var registered bool
	if err := s.db.QueryRowContext(ctx, `SELECT registered FROM sources WHERE id=?`, source.ID).Scan(&registered); err != nil {
		return selection, err
	}
	if !registered {
		return selection, fmt.Errorf("%w: workspace source must be registered in the project", ErrInvalid)
	}
	if source.RepositoryID == "" {
		return selection, nil
	}
	repo, err := s.GetRepository(ctx, source.RepositoryID)
	if err != nil {
		return selection, err
	}
	unlock, err := s.lockRepository(ctx, repo.ID)
	if err != nil {
		return selection, err
	}
	defer unlock()
	repo, err = s.refreshRepositoryLocked(ctx, repo)
	if err != nil {
		return selection, err
	}
	ref := strings.TrimSpace(selection.BaseRef)
	if ref == "" || ref == "HEAD" {
		ref = repo.DefaultBranch
	}
	if ref == "" {
		return selection, fmt.Errorf("%w: remote default branch could not be detected; choose a base branch", ErrInvalid)
	}
	if strings.HasPrefix(ref, "refs/zotigo/imported/") {
		selection.BaseRef = ref
	} else {
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
		if _, err := runGit(ctx, repo.CanonicalPath, "check-ref-format", "refs/heads/"+ref); err != nil {
			return selection, fmt.Errorf("%w: invalid base branch", ErrInvalid)
		}
		selection.BaseRef = "refs/remotes/origin/" + ref
	}
	commit, err := runGit(ctx, repo.CanonicalPath, "rev-parse", "--verify", selection.BaseRef+"^{commit}")
	if err != nil {
		return selection, fmt.Errorf("%w: base ref is unavailable", ErrConflict)
	}
	selection.resolvedBaseCommit = strings.TrimSpace(commit)
	return selection, nil
}

// Only catalog references change during migration. Existing worktrees continue
// resolving their old source for archive/restore/delete, including dirty indexes.
// No network access occurs while enumerating or migrating project sources.
func (s *Store) migrateRepositorySources(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT s.id,s.project_id,s.kind,s.canonical_path,s.git_common_dir,s.git_object_format,s.folder_mode,s.source_key,s.created_at,s.updated_at FROM sources s LEFT JOIN source_repositories sr ON sr.source_id=s.id WHERE s.registered=1 AND s.kind='git' AND sr.source_id IS NULL`)
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
	for _, source := range sources {
		if verifySourceIdentity(ctx, source) != nil {
			continue
		}
		remotes, err := runGit(ctx, source.CanonicalPath, "remote")
		if err != nil {
			continue
		}
		names := strings.Fields(remotes)
		if len(names) != 1 {
			continue
		}
		remote, err := runGit(ctx, source.CanonicalPath, "remote", "get-url", names[0])
		if err != nil {
			continue
		}
		remote = strings.TrimSpace(remote)
		if !filepath.IsAbs(remote) && !strings.Contains(remote, ":") {
			resolved, err := canonicalDirectory(filepath.Join(source.CanonicalPath, remote))
			if err != nil {
				continue
			}
			remote = resolved
		}
		if validateRemote(remote) != nil {
			continue
		}
		repo, err := s.registerRepository(ctx, remote, filepath.Base(source.CanonicalPath))
		if err != nil {
			if errors.Is(err, ErrInvalid) {
				continue
			}
			return err
		}
		if err := s.replaceLocalSource(ctx, source, repo); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) replaceLocalSource(ctx context.Context, source Source, repo Repository) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
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
	return tx.Commit()
}
