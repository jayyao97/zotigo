package cliapp

import (
	"context"
	"path/filepath"
	"sort"

	"github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigodclient"
)

type resumeEntries struct {
	sessions     []session.Metadata
	descriptions map[string]string
	disabled     map[string]string
}

// availabilityReasons maps the service's catalog availability to the
// picker's disabled reason. "ready" sessions are selectable.
var availabilityReasons = map[string]string{
	"archived":            "archived",
	"runtime_missing":     "runtime missing",
	"workspace_not_ready": "workspace unavailable",
	"cwd_mismatch":        "working directory outside its workspace",
	"cwd_unavailable":     "working directory unavailable",
}

// loadResumeSessions lists resumable sessions from the session service.
// Without all, only sessions whose cwd is exactly this directory are shown,
// matching the previous per-directory resume behavior.
func loadResumeSessions(ctx context.Context, client *zotigodclient.Client, cwd string, all bool) (resumeEntries, error) {
	projections, err := client.CatalogSessions(ctx)
	if err != nil {
		return resumeEntries{}, err
	}
	entries := resumeEntries{descriptions: map[string]string{}, disabled: map[string]string{}}
	labels := newWorkspaceLabels(ctx, client)
	for _, projection := range projections {
		runtime := projection.Runtime
		if runtime == nil {
			continue
		}
		if !all && filepath.Clean(runtime.WorkingDirectory) != filepath.Clean(cwd) {
			continue
		}
		entries.sessions = append(entries.sessions, session.Metadata{
			ID:               runtime.ID,
			WorkingDirectory: runtime.WorkingDirectory,
			LastPrompt:       runtime.LastPrompt,
			UpdatedAt:        runtime.UpdatedAt,
		})
		description := labels.describe(projection)
		if all {
			description += " · " + runtime.WorkingDirectory
		}
		entries.descriptions[runtime.ID] = description
		if reason, unavailable := availabilityReasons[projection.Availability]; unavailable {
			entries.disabled[runtime.ID] = reason
		}
		if runtime.Agent != "" && runtime.Agent != "zotigo" {
			entries.disabled[runtime.ID] = runtime.Agent + " session (open it in Desktop)"
		}
	}
	sort.SliceStable(entries.sessions, func(i, j int) bool {
		if entries.sessions[i].UpdatedAt.Equal(entries.sessions[j].UpdatedAt) {
			return entries.sessions[i].ID < entries.sessions[j].ID
		}
		return entries.sessions[i].UpdatedAt.After(entries.sessions[j].UpdatedAt)
	})
	return entries, nil
}

type workspaceLabels struct {
	ctx    context.Context
	client *zotigodclient.Client
	cache  map[string]string
}

func newWorkspaceLabels(ctx context.Context, client *zotigodclient.Client) *workspaceLabels {
	return &workspaceLabels{ctx: ctx, client: client, cache: map[string]string{}}
}

func (l *workspaceLabels) describe(projection zotigodclient.SessionProjection) string {
	label := "Directory"
	if workspaceID := projection.WorkspaceID(); workspaceID != "" {
		label = l.workspace(workspaceID)
	}
	if projection.Organization != nil && projection.Organization.Title != nil && *projection.Organization.Title != "" {
		label += " / " + *projection.Organization.Title
	}
	return label
}

func (l *workspaceLabels) workspace(id string) string {
	if label, ok := l.cache[id]; ok {
		return label
	}
	label := id
	if workspace, err := l.client.GetWorkspace(l.ctx, id); err == nil {
		label = workspace.Title
		if project, err := l.client.GetProject(l.ctx, workspace.ProjectID); err == nil {
			label = project.Name + " / " + workspace.Title
		}
	}
	l.cache[id] = label
	return label
}
