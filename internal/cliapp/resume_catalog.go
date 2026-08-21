package cliapp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/workspace"
)

func loadGlobalResumeSessions(manager *session.Manager) ([]session.Metadata, map[string]string, map[string]string, error) {
	sessions, err := manager.ListAll()
	if err != nil {
		return nil, nil, nil, err
	}
	descriptions := make(map[string]string, len(sessions))
	disabled := make(map[string]string)
	rootDir := manager.RootDir()
	if rootDir == "" {
		for _, metadata := range sessions {
			descriptions[metadata.ID] = "Legacy / Unassigned"
		}
		return sessions, descriptions, disabled, nil
	}
	catalog, err := workspace.OpenReadOnly(rootDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			for _, metadata := range sessions {
				descriptions[metadata.ID] = "Legacy / Unassigned"
			}
			return sessions, descriptions, disabled, nil
		}
		return nil, nil, nil, err
	}
	defer func() { _ = catalog.Close() }()

	ctx := context.Background()
	projects, err := catalog.ListProjects(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	projectNames := make(map[string]string, len(projects))
	workspaceByID := make(map[string]workspace.Workspace)
	for _, project := range projects {
		projectNames[project.ID] = project.Name
		workspaces, err := catalog.ListWorkspaces(ctx, project.ID, true)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, item := range workspaces {
			workspaceByID[item.ID] = item
		}
	}
	organizations, err := catalog.ListSessionOrganizations(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	organizationByID := make(map[string]workspace.SessionOrganization, len(organizations))
	for _, organization := range organizations {
		organizationByID[organization.SessionID] = organization
	}
	runtimeIDs := make(map[string]bool, len(sessions))
	for _, metadata := range sessions {
		runtimeIDs[metadata.ID] = true
	}
	for _, organization := range organizations {
		if runtimeIDs[organization.SessionID] {
			continue
		}
		sessions = append(sessions, session.Metadata{ID: organization.SessionID, UpdatedAt: organization.UpdatedAt})
		disabled[organization.SessionID] = "runtime missing"
	}
	for _, metadata := range sessions {
		organization, organized := organizationByID[metadata.ID]
		if !organized || organization.ProjectID == nil || organization.WorkspaceID == nil {
			descriptions[metadata.ID] = "Legacy / Unassigned"
			if metadata.WorkingDirectory != "" {
				if owned, err := catalog.DeletedWorkspaceOwnsPath(ctx, metadata.WorkingDirectory); err != nil {
					return nil, nil, nil, err
				} else if owned {
					disabled[metadata.ID] = "workspace deleted"
				}
			}
		} else {
			item, found := workspaceByID[*organization.WorkspaceID]
			projectName := projectNames[*organization.ProjectID]
			if projectName == "" {
				projectName = *organization.ProjectID
			}
			workspaceName := *organization.WorkspaceID
			if found {
				workspaceName = item.Title
			}
			descriptions[metadata.ID] = projectName + " / " + workspaceName
			if organization.Title != nil {
				descriptions[metadata.ID] += " / " + *organization.Title
			}
			if disabled[metadata.ID] == "runtime missing" {
				continue
			}
			switch {
			case organization.EffectiveArchived():
				disabled[metadata.ID] = "archived"
			case !found || item.Status != workspace.WorkspaceStatusReady:
				disabled[metadata.ID] = "workspace unavailable"
			case filepath.Clean(metadata.WorkingDirectory) != filepath.Clean(item.RootPath):
				disabled[metadata.ID] = "working directory mismatch"
			}
		}
		if metadata.WorkingDirectory != "" {
			if info, err := os.Stat(metadata.WorkingDirectory); err != nil || !info.IsDir() {
				disabled[metadata.ID] = "working directory unavailable"
			}
		}
	}
	sort.SliceStable(sessions, func(i, j int) bool {
		left := descriptions[sessions[i].ID]
		right := descriptions[sessions[j].ID]
		if left == right {
			if sessions[i].UpdatedAt.Equal(sessions[j].UpdatedAt) {
				return sessions[i].ID < sessions[j].ID
			}
			return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
		}
		return fmt.Sprintf("%s\x00%s", left, sessions[i].ID) < fmt.Sprintf("%s\x00%s", right, sessions[j].ID)
	})
	return sessions, descriptions, disabled, nil
}
