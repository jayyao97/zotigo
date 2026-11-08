package workspace

import (
	"context"
	"path/filepath"
	"strings"
)

type RepositoryBranch struct {
	Name       string `json:"name"`
	Ref        string `json:"ref"`
	Kind       string `json:"kind"`
	SourceID   string `json:"source_id,omitempty"`
	SourceName string `json:"source_name,omitempty"`
}

// ListSourceBranches reads local refs only. Imported branches are the snapshot
// preserved during migration, not a live mirror of the former user checkout.
func (s *Store) ListSourceBranches(ctx context.Context, projectID, sourceID string) ([]RepositoryBranch, error) {
	s.operationMu.Lock()
	source, err := func() (Source, error) {
		var currentID string
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(sr.replacement_source_id,s.id)
 FROM sources s LEFT JOIN source_repositories sr ON sr.source_id=s.id
 WHERE s.id=? AND s.project_id=?`, sourceID, projectID).Scan(&currentID); err != nil {
			return Source{}, scanError("source", err)
		}
		var registered bool
		if err := s.db.QueryRowContext(ctx, `SELECT registered FROM sources WHERE id=?`, currentID).Scan(&registered); err != nil {
			return Source{}, err
		}
		if !registered {
			return Source{}, ErrNotFound
		}
		return s.GetSource(ctx, projectID, currentID)
	}()
	s.operationMu.Unlock()
	if err != nil {
		return nil, err
	}
	if source.Kind != SourceKindGit {
		return nil, ErrInvalid
	}
	if err := verifySourceIdentity(ctx, source); err != nil {
		return nil, err
	}
	prefixes := []string{"refs/heads/"}
	names := make(map[string]string)
	if source.RepositoryID != "" {
		prefixes = []string{"refs/remotes/origin/", "refs/zotigo/imported/"}
		rows, err := s.db.QueryContext(ctx, `SELECT s.id,s.canonical_path FROM sources s
 JOIN source_repositories sr ON sr.source_id=s.id WHERE sr.repository_id=? AND sr.replacement_source_id IS NOT NULL`, source.RepositoryID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, path string
			if err := rows.Scan(&id, &path); err != nil {
				_ = rows.Close()
				return nil, err
			}
			names[id] = filepath.Base(path)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	args := append([]string{"for-each-ref", "--format=%(refname)%09%(symref)", "--sort=refname"}, prefixes...)
	// Branch enumeration is larger than scalar Git probes, particularly for
	// migration snapshots whose full refs include a source UUID.
	output, err := runGitWithOutputLimit(ctx, source.CanonicalPath, 8*1024*1024, args...)
	if err != nil {
		return nil, err
	}
	branches := make([]RepositoryBranch, 0)
	for _, line := range strings.Split(output, "\n") {
		ref, symbolic, ok := strings.Cut(line, "\t")
		if !ok || symbolic != "" {
			continue
		}
		switch {
		case strings.HasPrefix(ref, "refs/remotes/origin/"):
			branches = append(branches, RepositoryBranch{Name: strings.TrimPrefix(ref, "refs/remotes/origin/"), Ref: ref, Kind: "remote"})
		case strings.HasPrefix(ref, "refs/heads/"):
			branches = append(branches, RepositoryBranch{Name: strings.TrimPrefix(ref, "refs/heads/"), Ref: ref, Kind: "local"})
		case strings.HasPrefix(ref, "refs/zotigo/imported/"):
			id, remaining, ok := strings.Cut(strings.TrimPrefix(ref, "refs/zotigo/imported/"), "/")
			if ok && strings.HasPrefix(remaining, "heads/") {
				branches = append(branches, RepositoryBranch{Name: strings.TrimPrefix(remaining, "heads/"), Ref: ref, Kind: "imported", SourceID: id, SourceName: names[id]})
			}
		}
	}
	return branches, nil
}
