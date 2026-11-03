package zotigod

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

func TestCodexCatalogDoesNotApplyNativeMaxOutputTokens(t *testing.T) {
	catalog := codexCatalog(zotigoruntime.Capabilities{
		Models: []zotigoruntime.Model{{ID: "codex-test", DisplayName: "Codex Test", Default: true}},
	})
	payload, err := json.Marshal(catalog)
	if err != nil {
		t.Fatalf("marshal catalog: %v", err)
	}
	if strings.Contains(string(payload), "max_output_tokens") {
		t.Fatalf("Codex app-server catalog inherited native runtime output limit: %s", payload)
	}
	if catalog.Capabilities.PreToolUseHooks || !catalog.Capabilities.PostToolUseHooks {
		t.Fatalf("unexpected Codex hook capabilities: %#v", catalog.Capabilities)
	}
}

func TestUnavailableCodexCatalogPreservesNativeAgent(t *testing.T) {
	h := &handler{codexUnavailable: errors.New("Codex executable missing")}
	diagnostics := h.codexSyncDiagnostics(context.Background(), true)
	if len(diagnostics) != 1 || diagnostics[0].Code != "codex_unavailable" {
		t.Fatalf("diagnostics: %+v", diagnostics)
	}
	if diagnostics := h.codexSyncDiagnostics(context.Background(), false); len(diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", diagnostics)
	}
	response := httptest.NewRecorder()
	h.handleAgents(response, httptest.NewRequest("GET", "/agents", nil))
	var catalog agentCatalogResponse
	decodeCatalogData(t, response, &catalog)
	if len(catalog.Agents) != 1 || catalog.Agents[0].Availability != "available" || len(catalog.UnavailableAgents) != 1 || catalog.UnavailableAgents[0].Availability != "unavailable" || catalog.UnavailableAgents[0].Reason != "Codex executable missing" {
		t.Fatalf("catalog: %+v", catalog)
	}
	response = httptest.NewRecorder()
	h.handleCodexPrepare(response, httptest.NewRequest("POST", "/agents/codex/prepare", nil))
	if response.Code != 503 || !strings.Contains(response.Body.String(), "Codex executable missing") {
		t.Fatalf("prepare: %d %s", response.Code, response.Body.String())
	}
}
