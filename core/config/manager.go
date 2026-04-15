package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/viper"
)

const (
	ConfigFileName = "config.yaml"
	ConfigDirName  = ".zotigo"
	ProjectConfig  = "zotigo.yaml"
)

type Manager struct {
	v *viper.Viper
}

func NewManager() *Manager {
	v := viper.New()
	v.SetConfigType("yaml")
	return &Manager{v: v}
}

func (m *Manager) Load() (*Config, error) {
	defaults := DefaultConfig()
	m.v.SetDefault("default_profile", defaults.DefaultProfile)
	m.v.SetDefault("profiles", defaults.Profiles)
	m.v.SetDefault("security", defaults.Security)
	m.v.SetDefault("ui", defaults.UI)
	m.v.SetDefault("tools", defaults.Tools)

	// Load Global
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get user home directory: %w", err)
	}
	globalConfigPath := filepath.Join(home, ConfigDirName, ConfigFileName)

	m.v.SetConfigFile(globalConfigPath)
	if err := m.v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok && !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to read global config: %w", err)
		}
	}

	// Load Project
	cwd, err := os.Getwd()
	if err == nil {
		projectConfigPath := filepath.Join(cwd, ProjectConfig)
		if _, err := os.Stat(projectConfigPath); err == nil {
			m.v.SetConfigFile(projectConfigPath)
			if err := m.v.MergeInConfig(); err != nil {
				return nil, fmt.Errorf("failed to merge project config: %w", err)
			}
		} else {
			nestedProjectConfig := filepath.Join(cwd, ConfigDirName, ConfigFileName)
			if _, err := os.Stat(nestedProjectConfig); err == nil {
				m.v.SetConfigFile(nestedProjectConfig)
				if err := m.v.MergeInConfig(); err != nil {
					return nil, fmt.Errorf("failed to merge project nested config: %w", err)
				}
			}
		}
	}

	m.v.SetEnvPrefix("ZOTIGO")
	m.v.AutomaticEnv()

	var cfg Config
	if err := m.v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Post-process: Ensure default profiles are present if not overridden
	if cfg.Profiles == nil {
		cfg.Profiles = make(map[string]ProfileConfig)
	}
	for name, profile := range defaults.Profiles {
		if _, exists := cfg.Profiles[name]; !exists {
			cfg.Profiles[name] = profile
		}
	}

	// Apply safety.classifier defaults to EVERY profile, including user-defined
	// ones. Without this, a custom profile (e.g. "my-ollama") would load with
	// Classifier.Enabled == nil, and IsEnabled() would silently return false —
	// contradicting the documented default.
	classifierDefaults := defaultSafetyClassifierConfig()
	for name, profile := range cfg.Profiles {
		merged := profile
		userOmittedClassifier := merged.Safety.Classifier == (SafetyClassifierConfig{})
		if userOmittedClassifier {
			merged.Safety.Classifier = classifierDefaults
		} else {
			c := &merged.Safety.Classifier
			// *bool nil means "not set" — inherit default.
			// Non-nil means the user explicitly chose true or false.
			if c.Enabled == nil {
				c.Enabled = classifierDefaults.Enabled
			}
			if c.Mode == "" {
				c.Mode = classifierDefaults.Mode
			}
			if c.Profile == "" {
				c.Profile = classifierDefaults.Profile
			}
			if c.TimeoutMs == 0 {
				c.TimeoutMs = classifierDefaults.TimeoutMs
			}
			if c.MaxRecentActions == 0 {
				c.MaxRecentActions = classifierDefaults.MaxRecentActions
			}
			if c.MaxAuditContextChars == 0 {
				c.MaxAuditContextChars = classifierDefaults.MaxAuditContextChars
			}
		}
		cfg.Profiles[name] = merged
	}

	if cfg.Tools.Web.UserAgent == "" {
		cfg.Tools.Web.UserAgent = defaults.Tools.Web.UserAgent
	}
	if cfg.Tools.Web.TimeoutSec == 0 {
		cfg.Tools.Web.TimeoutSec = defaults.Tools.Web.TimeoutSec
	}
	if cfg.Tools.Web.MaxPageSize == 0 {
		cfg.Tools.Web.MaxPageSize = defaults.Tools.Web.MaxPageSize
	}

	return &cfg, nil
}

func (m *Manager) Save(cfg *Config) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get user home directory: %w", err)
	}

	configDir := filepath.Join(home, ConfigDirName)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	filePath := filepath.Join(configDir, ConfigFileName)

	vSave := viper.New()
	vSave.SetConfigType("yaml")

	vSave.Set("default_profile", cfg.DefaultProfile)
	vSave.Set("profiles", cfg.Profiles)
	vSave.Set("security", cfg.Security)
	vSave.Set("ui", cfg.UI)
	vSave.Set("tools", cfg.Tools)

	if err := vSave.WriteConfigAs(filePath); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

func (m *Manager) GetConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ConfigDirName, ConfigFileName), nil
}
