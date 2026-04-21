package openai

import (
	"strings"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

const ProviderName = "openai"

func init() {
	providers.Register(ProviderName, New)
}

// New chooses between the Chat Completions API (/v1/chat/completions)
// and the Responses API (/v1/responses) based on the configured model
// and an optional explicit `mode` override in Params.
//
// Routing rules (in priority order):
//
//  1. `params.mode: "response"` / `"chat"` — explicit user override.
//  2. Model starts with "gpt-5", "o1", "o3", or "o4" — route to
//     Responses API. These are reasoning-first models where the official
//     thinking event stream lives on /v1/responses.
//  3. Everything else — Chat Completions, same as before.
func New(cfg config.ProfileConfig) (providers.Provider, error) {
	opts := []option.RequestOption{}
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}

	client := openai.NewClient(opts...)
	mode := resolveOpenAIMode(cfg)

	switch mode {
	case "response":
		return &ResponseProvider{
			client:          &client,
			model:           cfg.Model,
			reasoningEffort: cfg.ThinkingLevel,
		}, nil
	default:
		return &ChatProvider{
			client:          &client,
			model:           cfg.Model,
			reasoningEffort: cfg.ThinkingLevel,
		}, nil
	}
}

// resolveOpenAIMode picks "chat" or "response". Explicit `mode` config
// wins; otherwise models that require the Responses API are auto-routed.
func resolveOpenAIMode(cfg config.ProfileConfig) string {
	if m, ok := cfg.Params["mode"].(string); ok && m != "" {
		return m
	}
	if modelUsesResponsesAPI(cfg.Model) {
		return "response"
	}
	return "chat"
}

// modelUsesResponsesAPI returns true for OpenAI models whose thinking
// event stream only lives on /v1/responses. The list is intentionally
// prefix-based — new variants in the same family (gpt-5-mini, o3-pro,
// etc.) auto-route without code changes.
func modelUsesResponsesAPI(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return false
	}
	for _, prefix := range []string{"gpt-5", "o1", "o3", "o4"} {
		if m == prefix || strings.HasPrefix(m, prefix+"-") || strings.HasPrefix(m, prefix+".") {
			return true
		}
	}
	return false
}
