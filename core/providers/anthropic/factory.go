package anthropic

import (
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/providers"
)

const ProviderName = "anthropic"

func init() {
	providers.Register(ProviderName, New)
}

func New(cfg config.ProfileConfig) (providers.Provider, error) {
	opts := []option.RequestOption{}
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}

	client := anthropic.NewClient(opts...)

	p := &ChatProvider{
		client:        &client,
		model:         cfg.Model,
		thinkingLevel: cfg.ThinkingLevel,
	}

	// Allow explicit budget override via params
	if bt, ok := cfg.Params["thinking_budget_tokens"]; ok {
		switch v := bt.(type) {
		case int:
			p.thinkingBudget = int64(v)
		case float64:
			p.thinkingBudget = int64(v)
		}
	}

	return p, nil
}
