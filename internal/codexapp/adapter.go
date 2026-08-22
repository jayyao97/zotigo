package codexapp

import (
	"context"
	"fmt"
	"time"

	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

type HostProvider interface {
	Acquire(context.Context) (*Lease, error)
}

type WorkerStarter func(context.Context, zotigoruntime.WorkerLaunchSpec, string, func()) error

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
	lease, err := a.host.Acquire(ctx)
	if err != nil {
		return zotigoruntime.Capabilities{}, err
	}
	defer func() { _ = lease.Release() }()
	capabilities := zotigoruntime.Capabilities{Installed: true, Version: lease.Version}
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
		if err := lease.RPC.Call(ctx, "model/list", params, &response); err != nil {
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
	if a.startWorker == nil {
		return fmt.Errorf("codex worker starter is not configured")
	}
	lease, err := a.host.Acquire(ctx)
	if err != nil {
		return err
	}
	if err := a.startWorker(ctx, spec, lease.SocketPath, func() { _ = lease.Release() }); err != nil {
		_ = lease.Release()
		return err
	}
	return nil
}

func (a *Adapter) WorkerLifecycle() zotigoruntime.WorkerLifecycle {
	return zotigoruntime.WorkerLifecycle{IdleTimeout: 250 * time.Millisecond}
}

var _ zotigoruntime.Adapter = (*Adapter)(nil)
