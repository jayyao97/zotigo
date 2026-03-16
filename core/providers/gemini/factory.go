package gemini

import (
	"context"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/providers"
	"google.golang.org/genai"
)

const ProviderName = "gemini"

func init() {
	providers.Register(ProviderName, New)
}

func New(cfg config.ProfileConfig) (providers.Provider, error) {
	clientCfg := &genai.ClientConfig{
		APIKey:  cfg.APIKey,
		Backend: genai.BackendGeminiAPI,
	}

	if cfg.BaseURL != "" {
		clientCfg.HTTPOptions = genai.HTTPOptions{
			BaseURL: cfg.BaseURL,
		}
	}

	client, err := genai.NewClient(context.Background(), clientCfg)
	if err != nil {
		return nil, err
	}

	p := &ChatProvider{
		client:        client,
		model:         cfg.Model,
		thinkingLevel: cfg.ThinkingLevel,
	}

	if temp, ok := cfg.Params["temperature"]; ok {
		switch v := temp.(type) {
		case float64:
			t := float32(v)
			p.temperature = &t
		case float32:
			p.temperature = &v
		}
	}

	if mt, ok := cfg.Params["max_tokens"]; ok {
		switch v := mt.(type) {
		case int:
			p.maxTokens = int32(v)
		case float64:
			p.maxTokens = int32(v)
		}
	}

	return p, nil
}
