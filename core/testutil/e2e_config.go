//go:build e2e

package testutil

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"

	"github.com/jayyao97/zotigo/core/config"
)

const (
	e2eConfigFileName       = "e2e.config.json"
	legacyE2EConfigFileName = "config.json"
)

// ProviderConfig represents the provider-specific config in e2e.config.json.
type ProviderConfig struct {
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
}

// E2EConfig represents the E2E config structure.
type E2EConfig struct {
	Provider  string          `json:"provider"`
	Streaming bool            `json:"streaming"`
	UserID    string          `json:"user_id"`
	OpenAI    *ProviderConfig `json:"openai,omitempty"`
	Anthropic *ProviderConfig `json:"anthropic,omitempty"`
	Gemini    *ProviderConfig `json:"gemini,omitempty"`
}

// LoadE2EConfig loads e2e.config.json (or legacy config.json) from project root.
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
	case "gemini":
		if c.Gemini != nil {
			return config.ProfileConfig{
				Provider: "gemini",
				Model:    c.Gemini.Model,
				APIKey:   c.Gemini.APIKey,
				BaseURL:  c.Gemini.BaseURL,
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

// findConfigPath finds e2e.config.json (or legacy config.json) in project root.
func findConfigPath() string {
	// Try from current directory first
	for _, name := range []string{e2eConfigFileName, legacyE2EConfigFileName} {
		if _, err := os.Stat(name); err == nil {
			return name
		}
	}

	// Try from project root (go up from source file)
	_, filename, _, ok := runtime.Caller(0)
	if ok {
		dir := filepath.Dir(filename)
		for i := 0; i < 5; i++ {
			for _, name := range []string{e2eConfigFileName, legacyE2EConfigFileName} {
				configPath := filepath.Join(dir, name)
				if _, err := os.Stat(configPath); err == nil {
					return configPath
				}
			}
			dir = filepath.Dir(dir)
		}
	}

	// Fallback: prefer the new file name.
	return e2eConfigFileName
}
