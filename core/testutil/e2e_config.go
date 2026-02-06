//go:build e2e

package testutil

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"

	"github.com/jayyao97/zotigo/core/config"
)

// ProviderConfig represents the provider-specific config in config.json
type ProviderConfig struct {
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
}

// E2EConfig represents the config.json structure for E2E tests
type E2EConfig struct {
	Provider  string          `json:"provider"`
	Streaming bool            `json:"streaming"`
	UserID    string          `json:"user_id"`
	OpenAI    *ProviderConfig `json:"openai,omitempty"`
	Anthropic *ProviderConfig `json:"anthropic,omitempty"`
}

// LoadE2EConfig loads the config.json from project root
func LoadE2EConfig() (*E2EConfig, error) {
	configPath := findConfigPath()
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	var cfg E2EConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// GetProfileConfig returns the profile config for the default provider
func (c *E2EConfig) GetProfileConfig() config.ProfileConfig {
	switch c.Provider {
	case "openai":
		if c.OpenAI != nil {
			return config.ProfileConfig{
				Provider: "openai",
				Model:    c.OpenAI.Model,
				APIKey:   c.OpenAI.APIKey,
				BaseURL:  c.OpenAI.BaseURL,
			}
		}
	case "anthropic":
		if c.Anthropic != nil {
			return config.ProfileConfig{
				Provider: "anthropic",
				Model:    c.Anthropic.Model,
				APIKey:   c.Anthropic.APIKey,
				BaseURL:  c.Anthropic.BaseURL,
			}
		}
	}
	return config.ProfileConfig{}
}

// MustLoadE2EConfig loads config and panics on error (for test setup)
func MustLoadE2EConfig() *E2EConfig {
	cfg, err := LoadE2EConfig()
	if err != nil {
		panic("failed to load E2E config: " + err.Error())
	}
	return cfg
}

// findConfigPath finds config.json in project root
func findConfigPath() string {
	// Try from current directory first
	if _, err := os.Stat("config.json"); err == nil {
		return "config.json"
	}

	// Try from project root (go up from source file)
	_, filename, _, ok := runtime.Caller(0)
	if ok {
		dir := filepath.Dir(filename)
		for i := 0; i < 5; i++ {
			configPath := filepath.Join(dir, "config.json")
			if _, err := os.Stat(configPath); err == nil {
				return configPath
			}
			dir = filepath.Dir(dir)
		}
	}

	// Fallback: assume we're in project root
	return "config.json"
}
