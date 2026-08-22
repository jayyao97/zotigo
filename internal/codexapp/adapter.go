package codexapp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

type HostProvider interface {
	Ensure(context.Context) (RPC, string, error)
	SocketPath() string
}

type WorkerStarter func(context.Context, zotigoruntime.WorkerLaunchSpec, string) error

type Adapter struct {
	host        HostProvider
	startWorker WorkerStarter
}

func NewAdapter(host HostProvider, startWorker WorkerStarter) *Adapter {
	return &Adapter{host: host, startWorker: startWorker}
}

func (a *Adapter) Kind() zotigoruntime.AgentKind {
	return zotigoruntime.AgentCodex
}

func (a *Adapter) Probe(ctx context.Context, _ zotigoruntime.ProbeRequest) (zotigoruntime.Capabilities, error) {
	client, version, err := a.host.Ensure(ctx)
	if err != nil {
		return zotigoruntime.Capabilities{}, err
	}
	capabilities := zotigoruntime.Capabilities{Installed: true, Version: version}
	var cursor any
	for {
		params := map[string]any{}
		if cursor != nil {
			params["cursor"] = cursor
		}
		var response struct {
			Data []struct {
				ID                        string `json:"id"`
				Model                     string `json:"model"`
				DisplayName               string `json:"displayName"`
				IsDefault                 bool   `json:"isDefault"`
				SupportedReasoningEfforts []struct {
					ReasoningEffort string `json:"reasoningEffort"`
				} `json:"supportedReasoningEfforts"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		if err := client.Call(ctx, "model/list", params, &response); err != nil {
			return zotigoruntime.Capabilities{}, fmt.Errorf("list codex models: %w", err)
		}
		for _, model := range response.Data {
			reasoning := make([]string, 0, len(model.SupportedReasoningEfforts))
			for _, effort := range model.SupportedReasoningEfforts {
				reasoning = append(reasoning, effort.ReasoningEffort)
			}
			capabilities.Models = append(capabilities.Models, zotigoruntime.Model{
				ID: model.Model, DisplayName: model.DisplayName, Default: model.IsDefault,
				SupportedReasoningEfforts: reasoning,
			})
		}
		if response.NextCursor == nil || *response.NextCursor == "" {
			break
		}
		cursor = *response.NextCursor
	}
	return capabilities, nil
}

func (a *Adapter) StartWorker(ctx context.Context, spec zotigoruntime.WorkerLaunchSpec) error {
	if spec.WorkspaceBinding == nil || spec.WorkspaceBinding.ExternalID == "" {
		return fmt.Errorf("codex worker requires a bound workspace")
	}
	if a.startWorker == nil {
		return fmt.Errorf("codex worker starter is not configured")
	}
	if _, _, err := a.host.Ensure(ctx); err != nil {
		return err
	}
	return a.startWorker(ctx, spec, a.host.SocketPath())
}

func (a *Adapter) ReadWorkspace(ctx context.Context, externalID string) (zotigoruntime.ExternalWorkspace, error) {
	client, version, err := a.host.Ensure(ctx)
	if err != nil {
		return zotigoruntime.ExternalWorkspace{}, err
	}
	_ = version
	var response struct {
		Project project `json:"project"`
	}
	if err := client.Call(ctx, "project/read", map[string]any{"projectId": externalID}, &response); err != nil {
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) && strings.Contains(strings.ToLower(rpcErr.Message), "not found") {
			return zotigoruntime.ExternalWorkspace{}, zotigoruntime.ErrWorkspaceNotFound
		}
		return zotigoruntime.ExternalWorkspace{}, fmt.Errorf("read codex project: %w", err)
	}
	return response.Project.externalWorkspace(), nil
}

func (a *Adapter) FindWorkspace(ctx context.Context, spec zotigoruntime.WorkspaceSpec) (*zotigoruntime.ExternalWorkspace, error) {
	client, _, err := a.host.Ensure(ctx)
	if err != nil {
		return nil, err
	}
	root, err := canonicalPath(spec.RootPath)
	if err != nil {
		return nil, err
	}
	var metadataMatches []zotigoruntime.ExternalWorkspace
	var rootMatches []zotigoruntime.ExternalWorkspace
	var cursor any
	for {
		params := map[string]any{}
		if cursor != nil {
			params["cursor"] = cursor
		}
		var response struct {
			Data       []project `json:"data"`
			NextCursor *string   `json:"nextCursor"`
		}
		if err := client.Call(ctx, "project/list", params, &response); err != nil {
			return nil, fmt.Errorf("list codex projects: %w", err)
		}
		for _, candidate := range response.Data {
			external := candidate.externalWorkspace()
			if external.RootPath == "" {
				continue
			}
			candidateRoot, candidateErr := canonicalPath(external.RootPath)
			if candidateErr != nil || candidateRoot != root {
				continue
			}
			rootMatches = append(rootMatches, external)
			if candidate.Metadata["zotigod.workspace_id"] == spec.WorkspaceID {
				metadataMatches = append(metadataMatches, external)
			}
		}
		if response.NextCursor == nil || *response.NextCursor == "" {
			break
		}
		cursor = *response.NextCursor
	}
	if len(metadataMatches) > 1 || (len(metadataMatches) == 0 && len(rootMatches) > 1) {
		return nil, zotigoruntime.ErrWorkspaceConflict
	}
	if len(metadataMatches) == 1 {
		return &metadataMatches[0], nil
	}
	if len(rootMatches) == 1 {
		return &rootMatches[0], nil
	}
	return nil, nil
}

func (a *Adapter) CreateWorkspace(ctx context.Context, intent zotigoruntime.WorkspaceCreateIntent) (zotigoruntime.ExternalWorkspace, error) {
	client, _, err := a.host.Ensure(ctx)
	if err != nil {
		return zotigoruntime.ExternalWorkspace{}, err
	}
	root, err := canonicalPath(intent.RootPath)
	if err != nil {
		return zotigoruntime.ExternalWorkspace{}, err
	}
	var response struct {
		Project project `json:"project"`
	}
	params := map[string]any{
		"name":           intent.Name,
		"roots":          []map[string]string{{"path": root}},
		"metadata":       map[string]string{"zotigod.workspace_id": intent.WorkspaceID},
		"idempotencyKey": intent.IdempotencyKey,
	}
	if err := client.Call(ctx, "project/create", params, &response); err != nil {
		var rpcErr *RPCError
		message := ""
		if errors.As(err, &rpcErr) {
			message = strings.ToLower(rpcErr.Message)
		}
		if strings.Contains(message, "idempotency") && strings.Contains(message, "deleted project") {
			return zotigoruntime.ExternalWorkspace{}, zotigoruntime.ErrWorkspaceCreateTombstone
		}
		return zotigoruntime.ExternalWorkspace{}, fmt.Errorf("create codex project: %w", err)
	}
	external := response.Project.externalWorkspace()
	if external.RootPath == "" {
		return zotigoruntime.ExternalWorkspace{}, fmt.Errorf("codex project response must contain exactly one non-empty root")
	}
	createdRoot, err := canonicalPath(external.RootPath)
	if err != nil || createdRoot != root || external.Metadata["zotigod.workspace_id"] != intent.WorkspaceID {
		return zotigoruntime.ExternalWorkspace{}, fmt.Errorf("codex project response does not match create intent")
	}
	return external, nil
}

type project struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Roots    []projectRoot     `json:"roots"`
	Metadata map[string]string `json:"metadata"`
}

type projectRoot struct {
	Path string `json:"path"`
}

func (p project) externalWorkspace() zotigoruntime.ExternalWorkspace {
	root := ""
	if len(p.Roots) == 1 {
		root = p.Roots[0].Path
	}
	return zotigoruntime.ExternalWorkspace{ID: p.ID, Name: p.Name, RootPath: root, Metadata: p.Metadata}
}

func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root symlinks: %w", err)
	}
	return filepath.Clean(resolved), nil
}

var _ zotigoruntime.Adapter = (*Adapter)(nil)
var _ zotigoruntime.WorkspaceAdapter = (*Adapter)(nil)
