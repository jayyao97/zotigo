package wiring

import (
	"time"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/observability"
	"github.com/jayyao97/zotigo/core/observability/langfuse"
)

// NewObserver constructs the configured observability backend or a no-op.
func NewObserver(cfg config.ObservabilityConfig, sessionIDPrefix string, staticMeta map[string]any) observability.Observer {
	if !cfg.Langfuse.IsEnabled() {
		return observability.Noop{}
	}
	return langfuse.New(langfuse.Config{
		Host:                cfg.Langfuse.Host,
		PublicKey:           cfg.Langfuse.PublicKey,
		SecretKey:           cfg.Langfuse.SecretKey,
		FlushInterval:       time.Duration(cfg.Langfuse.FlushInterval) * time.Second,
		SessionIDPrefix:     sessionIDPrefix,
		StaticTraceMetadata: staticMeta,
	})
}
