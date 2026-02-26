package openai

import (
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

const ProviderName = "openai"

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

	// openai.NewClient returns *Client in v3 (usually).
	// But previous error said "cannot use client (struct) as *Client".
	// Wait, if NewClient returns *Client, then `client := ...` is `*Client`.
	// `&ResponseProvider{ client: client }` expects `*Client`.
	// If previous error said `cannot use client (variable of struct type Client)`, it means NewClient returned struct.
	// I will take address `&client`.

	client := openai.NewClient(opts...)

	mode := "chat"
	if m, ok := cfg.Params["mode"].(string); ok {
		mode = m
	}

	// Need to dereference or take address depending on what NewClient returns.
	// Let's assume NewClient returns `*Client` because that's standard Go.
	// Why did error say "variable of struct type Client"?
	// Maybe I misread "value of type Client"?
	// Actually, looking at `client.go` (via `ls`), it's likely `func NewClient(...) *Client`.
	// I'll try `client` first. If it fails, I'll add `&`.
	// Wait, if I check `factory.go` error:
	// `cannot use client (variable of struct type "github.com/openai/openai-go/v3".Client) as *"github.com/openai/openai-go/v3".Client`
	// This CONFIRMS `client` variable IS A STRUCT.
	// So `openai.NewClient` returns `Client` struct by value.

	switch mode {
	case "response":
		return &ResponseProvider{
			client: &client,
			model:  cfg.Model,
		}, nil
	default:
		return &ChatProvider{
			client: &client,
			model:  cfg.Model,
		}, nil
	}
}
