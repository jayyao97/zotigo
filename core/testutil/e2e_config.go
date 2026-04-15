//go:build e2e

package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/jayyao97/zotigo/core/config"
	"gopkg.in/yaml.v3"
)

// Config file names in priority order, including legacy fallbacks.
var e2eConfigFileNames = []string{
	"zotigo.e2e.yaml",
	"zotigo.e2e.yml",
	"e2e.config.json",
	"config.json",
}

// E2EConfig mirrors the main config structure for e2e testing.
type E2EConfig struct {
	DefaultProfile string                          `yaml:"default_profile"`
	Profiles       map[string]config.ProfileConfig `yaml:"profiles"`
}

// LoadE2EConfig loads zotigo.e2e.yaml from project root.
func LoadE2EConfig() (*E2EConfig, error) {
	configPath := findConfigPath()
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	var cfg E2EConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// GetProfileConfig returns the config for the default profile.
func (c *E2EConfig) GetProfileConfig() config.ProfileConfig {
	if p, ok := c.Profiles[c.DefaultProfile]; ok {
		return p
	}
	return config.ProfileConfig{}
}

// GetProfile returns the config for a named profile.
func (c *E2EConfig) GetProfile(name string) (config.ProfileConfig, bool) {
	p, ok := c.Profiles[name]
	return p, ok
}

// AllProfiles returns all configured profiles.
func (c *E2EConfig) AllProfiles() map[string]config.ProfileConfig {
	return c.Profiles
}

// ResolveClassifierProfile resolves the classifier profile for the default profile.
func (c *E2EConfig) ResolveClassifierProfile() (string, config.ProfileConfig, error) {
	active := c.GetProfileConfig()
	targetName := c.DefaultProfile
	if name := active.Safety.Classifier.Profile; name != "" {
		targetName = name
	}
	target, ok := c.Profiles[targetName]
	if !ok {
		return "", config.ProfileConfig{}, fmt.Errorf("classifier profile %q not found", targetName)
	}
	return targetName, target, nil
}

// ProfilesByProvider returns profiles matching a specific provider.
func (c *E2EConfig) ProfilesByProvider(provider string) map[string]config.ProfileConfig {
	result := make(map[string]config.ProfileConfig)
	for name, p := range c.Profiles {
		if p.Provider == provider {
			result[name] = p
		}
	}
	return result
}

// MustLoadE2EConfig loads config and panics on error (for test setup).
func MustLoadE2EConfig() *E2EConfig {
	cfg, err := LoadE2EConfig()
	if err != nil {
		panic("failed to load E2E config: " + err.Error())
	}
	return cfg
}

// findConfigPath finds the e2e config file from project root.
func findConfigPath() string {
	// Try from current directory first
	for _, name := range e2eConfigFileNames {
		if _, err := os.Stat(name); err == nil {
			return name
		}
	}

	// Try from project root (go up from source file)
	_, filename, _, ok := runtime.Caller(0)
	if ok {
		dir := filepath.Dir(filename)
		for i := 0; i < 5; i++ {
			for _, name := range e2eConfigFileNames {
				configPath := filepath.Join(dir, name)
				if _, err := os.Stat(configPath); err == nil {
					return configPath
				}
			}
			dir = filepath.Dir(dir)
		}
	}

	return e2eConfigFileNames[0]
}

func isYAMLFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".yaml" || ext == ".yml"
}
