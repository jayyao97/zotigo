package deepseek

import (
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/providers/anthropic"
)

const (
	ProviderName   = "deepseek"
	defaultBaseURL = "https://api.deepseek.com/anthropic"
)

func init() {
	providers.Register(ProviderName, New)
}

// New builds the DeepSeek service wrapper on top of the Anthropic Messages
// driver used by DeepSeek's official compatibility endpoint.
func New(cfg config.ProfileConfig) (providers.Provider, error) {
	cfg = withDefaults(cfg)
	inner, err := anthropic.New(cfg)
	if err != nil {
		return nil, err
	}
	return &Provider{Provider: inner}, nil
}

func withDefaults(cfg config.ProfileConfig) config.ProfileConfig {
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	return cfg
}
