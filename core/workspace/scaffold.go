package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const ownerMarkerName = ".zotigo-owner.json"

type ownerMarker struct {
	Version     int    `json:"version"`
	ProjectID   string `json:"project_id"`
	WorkspaceID string `json:"workspace_id"`
	Nonce       string `json:"nonce"`
}

func (s *Store) ProvisionWorkspaceScaffold(ctx context.Context, workspaceID string) (Workspace, error) {
	return s.ProvisionWorkspace(ctx, workspaceID)
}

func provisionScaffold(workspace Workspace, nonce string) error {
	if info, err := os.Lstat(workspace.RootPath); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: workspace root is not an owned directory", ErrConflict)
		}
		return validateOwnerMarker(workspace.RootPath, workspace.ProjectID, workspace.ID, nonce)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect workspace root: %w", err)
	}

	parent := filepath.Dir(workspace.RootPath)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create workspace parent: %w", err)
	}
	staging := filepath.Join(parent, "."+workspace.ID+".staging")
	if _, err := os.Lstat(staging); err == nil {
		if markerErr := validateOwnerMarker(staging, workspace.ProjectID, workspace.ID, nonce); markerErr != nil {
			return fmt.Errorf("%w: workspace staging path is not owned", ErrConflict)
		}
		if err := os.RemoveAll(staging); err != nil {
			return fmt.Errorf("remove owned workspace staging path: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect workspace staging path: %w", err)
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		return fmt.Errorf("create workspace staging path: %w", err)
	}
	marker := ownerMarker{Version: 1, ProjectID: workspace.ProjectID, WorkspaceID: workspace.ID, Nonce: nonce}
	data, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("encode workspace owner marker: %w", err)
	}
	if err := os.WriteFile(filepath.Join(staging, ownerMarkerName), data, 0o600); err != nil {
		return fmt.Errorf("write workspace owner marker: %w", err)
	}
	for _, directory := range []string{"code", "artifacts", "notes"} {
		if err := os.Mkdir(filepath.Join(staging, directory), 0o700); err != nil {
			return fmt.Errorf("create workspace %s directory: %w", directory, err)
		}
	}
	if err := os.Rename(staging, workspace.RootPath); err != nil {
		return fmt.Errorf("publish workspace scaffold: %w", err)
	}
	return nil
}

func validateOwnerMarker(root string, projectID string, workspaceID string, nonce string) error {
	data, err := os.ReadFile(filepath.Join(root, ownerMarkerName))
	if err != nil {
		return fmt.Errorf("%w: read workspace owner marker: %v", ErrConflict, err)
	}
	var marker ownerMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return fmt.Errorf("%w: decode workspace owner marker: %v", ErrConflict, err)
	}
	if marker.Version != 1 || marker.ProjectID != projectID || marker.WorkspaceID != workspaceID || marker.Nonce != nonce {
		return fmt.Errorf("%w: workspace owner marker does not match catalog", ErrConflict)
	}
	return nil
}
