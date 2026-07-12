package wiring

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/observability"
)

func TestNewObserverUsesNoopWhenDisabled(t *testing.T) {
	observer := NewObserver(config.ObservabilityConfig{}, "session", nil)
	if _, ok := observer.(observability.Noop); !ok {
		t.Fatalf("expected no-op observer, got %T", observer)
	}
}

func TestNewObserverBuildsConfiguredBackend(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	observer := NewObserver(config.ObservabilityConfig{Langfuse: config.LangfuseConfig{
		Enabled:       true,
		Host:          server.URL,
		PublicKey:     "public",
		SecretKey:     "secret",
		FlushInterval: 1,
	}}, "session", map[string]any{"worker": true})
	if _, ok := observer.(observability.Noop); ok {
		t.Fatal("expected configured observability backend")
	}
	if err := observer.Close(context.Background()); err != nil {
		t.Fatalf("close observer: %v", err)
	}
}
