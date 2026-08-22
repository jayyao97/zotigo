package zotigod

import (
	"context"
	"fmt"
	"time"

	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

const nativeWorkerIdleTimeout = 5 * time.Minute

type runtimeRegistry struct {
	adapters map[zotigoruntime.AgentKind]zotigoruntime.Adapter
}

func newRuntimeRegistry(adapters ...zotigoruntime.Adapter) *runtimeRegistry {
	registry := &runtimeRegistry{adapters: make(map[zotigoruntime.AgentKind]zotigoruntime.Adapter, len(adapters))}
	for _, adapter := range adapters {
		if adapter != nil {
			registry.adapters[adapter.Kind()] = adapter
		}
	}
	return registry
}

func (r *runtimeRegistry) adapter(kind zotigoruntime.AgentKind) (zotigoruntime.Adapter, error) {
	if r == nil {
		return nil, fmt.Errorf("runtime %q is not configured", kind)
	}
	adapter := r.adapters[kind]
	if adapter == nil {
		return nil, fmt.Errorf("runtime %q is not configured", kind)
	}
	return adapter, nil
}

type nativeRuntimeAdapter struct {
	launcher workerLauncher
}

func (a nativeRuntimeAdapter) Kind() zotigoruntime.AgentKind {
	return zotigoruntime.AgentZotigo
}

func (a nativeRuntimeAdapter) Probe(context.Context, zotigoruntime.ProbeRequest) (zotigoruntime.Capabilities, error) {
	return zotigoruntime.Capabilities{Installed: true}, nil
}

func (a nativeRuntimeAdapter) StartWorker(ctx context.Context, spec zotigoruntime.WorkerLaunchSpec) error {
	if a.launcher == nil {
		return nil
	}
	return a.launcher.Start(ctx, spec.SessionID, spec.WorkingDirectory)
}

func (a nativeRuntimeAdapter) WorkerLifecycle() zotigoruntime.WorkerLifecycle {
	return zotigoruntime.WorkerLifecycle{IdleTimeout: nativeWorkerIdleTimeout}
}
