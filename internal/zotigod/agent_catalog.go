package zotigod

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jayyao97/zotigo/internal/codexapp"
	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

type agentCatalogResponse struct {
	DefaultAgent string         `json:"default_agent"`
	Agents       []agentCatalog `json:"agents"`
}

type agentCatalog struct {
	ID           string                   `json:"id"`
	Label        string                   `json:"label"`
	Availability string                   `json:"availability"`
	Version      string                   `json:"version,omitempty"`
	Capabilities agentCatalogCapabilities `json:"capabilities"`
	Models       []agentCatalogModel      `json:"models,omitempty"`
}

type agentCatalogCapabilities struct {
	Profiles  bool `json:"profiles"`
	Models    bool `json:"models"`
	Projects  bool `json:"projects,omitempty"`
	Steering  bool `json:"steering"`
	Approvals bool `json:"approvals"`
}

type agentCatalogModel struct {
	ID                        string   `json:"id"`
	DisplayName               string   `json:"display_name"`
	IsDefault                 bool     `json:"is_default"`
	SupportedReasoningEfforts []string `json:"supported_reasoning_efforts"`
}

func (h *handler) handleAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	response := agentCatalogResponse{DefaultAgent: string(zotigoruntime.AgentZotigo), Agents: []agentCatalog{{
		ID: string(zotigoruntime.AgentZotigo), Label: "Zotigo", Availability: "available",
		Capabilities: agentCatalogCapabilities{Profiles: true, Steering: true, Approvals: true},
	}}}
	if _, _, err := codexapp.Discover(); err == nil {
		response.Agents = append(response.Agents, agentCatalog{
			ID: string(zotigoruntime.AgentCodex), Label: "Codex", Availability: "installed",
			Capabilities: agentCatalogCapabilities{Models: true, Projects: true, Steering: true, Approvals: false},
		})
	}
	writeAPIJSON(w, http.StatusOK, response)
}

func (h *handler) handleCodexPrepare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	capabilities, err := h.codexCapabilities(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, fmt.Sprintf("prepare codex: %v", err))
		return
	}
	writeAPIJSON(w, http.StatusOK, codexCatalog(capabilities))
}

func (h *handler) codexCapabilities(ctx context.Context) (zotigoruntime.Capabilities, error) {
	adapter, err := h.runtimes.adapter(zotigoruntime.AgentCodex)
	if err != nil {
		return zotigoruntime.Capabilities{}, err
	}
	return adapter.Probe(ctx, zotigoruntime.ProbeRequest{})
}

func (h *handler) validateCodexSettings(ctx context.Context, modelID string, reasoningEffort string) error {
	capabilities, err := h.codexCapabilities(ctx)
	if err != nil {
		return fmt.Errorf("codex is unavailable: %w", err)
	}
	for _, model := range capabilities.Models {
		if model.ID != modelID {
			continue
		}
		for _, effort := range model.SupportedReasoningEfforts {
			if effort == reasoningEffort {
				return nil
			}
		}
		return fmt.Errorf("reasoning_effort %q is not supported by model %q", reasoningEffort, modelID)
	}
	return fmt.Errorf("unknown codex model %q", modelID)
}

func codexCatalog(capabilities zotigoruntime.Capabilities) agentCatalog {
	models := make([]agentCatalogModel, 0, len(capabilities.Models))
	for _, model := range capabilities.Models {
		models = append(models, agentCatalogModel{
			ID: model.ID, DisplayName: model.DisplayName, IsDefault: model.Default,
			SupportedReasoningEfforts: model.SupportedReasoningEfforts,
		})
	}
	return agentCatalog{
		ID: string(zotigoruntime.AgentCodex), Label: "Codex", Availability: "available", Version: capabilities.Version,
		Capabilities: agentCatalogCapabilities{Models: true, Projects: true, Steering: true, Approvals: false},
		Models:       models,
	}
}
