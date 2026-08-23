package gemini

import (
	"context"
	"fmt"
	"math"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/providers"
	"google.golang.org/genai"
)

const ProviderName = "gemini"

func init() {
	providers.Register(ProviderName, New)
}

func New(cfg config.ProfileConfig) (providers.Provider, error) {
	maxOutputTokens, err := cfg.EffectiveMaxOutputTokens()
	if err != nil {
		return nil, err
	}
	if maxOutputTokens > math.MaxInt32 {
		return nil, fmt.Errorf("max_output_tokens %d exceeds Gemini's maximum representable value %d", maxOutputTokens, int64(math.MaxInt32))
	}
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
		maxTokens:     int32(maxOutputTokens),
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

	return p, nil
}
