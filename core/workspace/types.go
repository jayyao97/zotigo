package workspace

import "time"

type SourceKind string

const (
	SourceKindGit    SourceKind = "git"
	SourceKindFolder SourceKind = "folder"
)

type FolderMode string

const (
	FolderModeDirect    FolderMode = "direct"
	FolderModeReference FolderMode = "reference"
	FolderModeCopy      FolderMode = "copy"
)

type WorkspaceStatus string

const (
	WorkspaceStatusProvisioning WorkspaceStatus = "provisioning"
	WorkspaceStatusReady        WorkspaceStatus = "ready"
	WorkspaceStatusError        WorkspaceStatus = "error"
	WorkspaceStatusArchiving    WorkspaceStatus = "archiving"
	WorkspaceStatusArchived     WorkspaceStatus = "archived"
	WorkspaceStatusDeleting     WorkspaceStatus = "deleting"
	WorkspaceStatusDeleted      WorkspaceStatus = "deleted"
)

type ProjectStatus string

const (
	ProjectStatusActive    ProjectStatus = "active"
	ProjectStatusArchiving ProjectStatus = "archiving"
	ProjectStatusArchived  ProjectStatus = "archived"
	ProjectStatusDeleting  ProjectStatus = "deleting"
)

type Project struct {
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	Status     ProjectStatus `json:"status"`
	ArchivedAt *time.Time    `json:"archived_at,omitempty"`
	CreatedAt  time.Time     `json:"created_at"`
	UpdatedAt  time.Time     `json:"updated_at"`
}

type Source struct {
	ID              string     `json:"id"`
	ProjectID       string     `json:"project_id"`
	Kind            SourceKind `json:"kind"`
	CanonicalPath   string     `json:"canonical_path"`
	GitCommonDir    string     `json:"git_common_dir,omitempty"`
	GitObjectFormat string     `json:"git_object_format,omitempty"`
	FolderMode      FolderMode `json:"folder_mode,omitempty"`
	SourceKey       string     `json:"source_key"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type Workspace struct {
	ID         string          `json:"id"`
	ProjectID  string          `json:"project_id"`
	Title      string          `json:"title"`
	RootPath   string          `json:"root_path"`
	Status     WorkspaceStatus `json:"status"`
	Error      string          `json:"error,omitempty"`
	ArchivedAt *time.Time      `json:"archived_at,omitempty"`
	DeletedAt  *time.Time      `json:"deleted_at,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

type SourceInput struct {
	Kind            SourceKind
	CanonicalPath   string
	GitCommonDir    string
	GitObjectFormat string
	FolderMode      FolderMode
	SourceKey       string
}

type WorkspaceSourceInput struct {
	SourceID       string     `json:"source_id"`
	Mode           FolderMode `json:"mode,omitempty"`
	BaseRef        string     `json:"base_ref,omitempty"`
	ExpectedCommit string     `json:"expected_commit,omitempty"`
	BranchName     string     `json:"branch_name,omitempty"`
}

type Checkout struct {
	WorkspaceID  string
	SourceID     string
	WorktreePath string
	BaseRef      string
	BaseCommit   string
	BranchName   string
	OwnedHead    string
	Status       string
	Error        string
}

type FolderBinding struct {
	WorkspaceID string
	SourceID    string
	Mode        FolderMode
	TargetPath  string
	Status      string
	Error       string
}

type ArchiveImpact struct {
	WorkspaceID        string   `json:"workspace_id"`
	SessionIDs         []string `json:"session_ids"`
	WorktreePaths      []string `json:"worktree_paths"`
	DirtyWorktreePaths []string `json:"dirty_worktree_paths"`
	RetainedBranches   []string `json:"retained_branches"`
}

type DeleteImpact struct {
	WorkspaceID         string   `json:"workspace_id"`
	SessionIDs          []string `json:"session_ids"`
	WorkspaceRoot       string   `json:"workspace_root"`
	WorktreePaths       []string `json:"worktree_paths"`
	DirtyWorktreePaths  []string `json:"dirty_worktree_paths"`
	LocalBranches       []string `json:"local_branches"`
	PreservesSources    bool     `json:"preserves_sources"`
	PreservesSessions   bool     `json:"preserves_runtime_sessions"`
	PreservesRemoteRefs bool     `json:"preserves_remote_refs"`
}

type ProjectArchiveImpact struct {
	ProjectID          string   `json:"project_id"`
	WorkspaceIDs       []string `json:"workspace_ids"`
	SessionIDs         []string `json:"session_ids"`
	WorktreePaths      []string `json:"worktree_paths"`
	DirtyWorktreePaths []string `json:"dirty_worktree_paths"`
	RetainedBranches   []string `json:"retained_branches"`
}

type ProjectDeleteImpact struct {
	ProjectID                  string   `json:"project_id"`
	WorkspaceIDs               []string `json:"workspace_ids"`
	SessionIDs                 []string `json:"session_ids"`
	WorkspaceRoots             []string `json:"workspace_roots"`
	WorktreePaths              []string `json:"worktree_paths"`
	DirtyWorktreePaths         []string `json:"dirty_worktree_paths"`
	LocalBranches              []string `json:"local_branches"`
	PreservesSourceDirectories bool     `json:"preserves_source_directories"`
	PreservesSessions          bool     `json:"preserves_runtime_sessions"`
	PreservesRemoteRefs        bool     `json:"preserves_remote_refs"`
}

type SessionOrganization struct {
	SessionID           string     `json:"session_id"`
	ProjectID           *string    `json:"project_id"`
	WorkspaceID         *string    `json:"workspace_id"`
	Title               *string    `json:"title"`
	PinnedAt            *time.Time `json:"pinned_at"`
	PinnedPosition      *int64     `json:"pinned_position"`
	WorkspacePosition   *int64     `json:"workspace_position"`
	SelfArchivedAt      *time.Time `json:"self_archived_at"`
	WorkspaceArchivedAt *time.Time `json:"workspace_archived_at"`
	Revision            int64      `json:"revision"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

func (o SessionOrganization) EffectiveArchived() bool {
	return o.SelfArchivedAt != nil || o.WorkspaceArchivedAt != nil
}
