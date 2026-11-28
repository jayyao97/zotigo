package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
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
	// Dots are literal in profile names and provider parameter keys. Use a
	// separate path delimiter so loading, merging and writing preserve them.
	v := viper.NewWithOptions(viper.KeyDelimiter("::"))
	// Preserve the existing ZOTIGO_PROFILES.NAME.MODEL environment convention.
	v.SetEnvKeyReplacer(strings.NewReplacer("::", "."))
	v.SetConfigType("yaml")
	return &Manager{v: v}
}

func (m *Manager) LoadForDir(workDir string) (*Config, error) {
	defaults := DefaultConfig()
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
	if workDir != "" {
		projectConfigPath := filepath.Join(workDir, ProjectConfig)
		if _, err := os.Stat(projectConfigPath); err == nil {
			m.v.SetConfigFile(projectConfigPath)
			if err := m.v.MergeInConfig(); err != nil {
				return nil, fmt.Errorf("failed to merge project config: %w", err)
			}
		} else {
			nestedProjectConfig := filepath.Join(workDir, ConfigDirName, ConfigFileName)
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

	if cfg.Profiles == nil {
		cfg.Profiles = make(map[string]ProfileConfig)
	}

	// Apply safety.classifier defaults to every configured profile. Without
	// this, a custom profile (e.g. "my-ollama") would load with
	// Classifier.Enabled == nil, and IsEnabled() would silently return false —
	// contradicting the documented default.
	classifierDefaults := defaultSafetyClassifierConfig()
	for name, profile := range cfg.Profiles {
		merged := profile
		maxOutputTokens, err := merged.EffectiveMaxOutputTokens()
		if err != nil {
			return nil, fmt.Errorf("profile %q: %w", name, err)
		}
		merged.MaxOutputTokens = &maxOutputTokens
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
			if c.ReviewThreshold == "" {
				c.ReviewThreshold = classifierDefaults.ReviewThreshold
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

// EnsureGlobalConfig creates the default global config only when it is absent.
// The exclusive file creation makes concurrent calls safe without overwriting
// a config created by another process.
func (m *Manager) EnsureGlobalConfig() (path string, created bool, err error) {
	path, err = m.GetConfigPath()
	if err != nil {
		return "", false, fmt.Errorf("failed to get user home directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return path, false, fmt.Errorf("failed to create config directory: %w", err)
	}

	if _, err = os.Lstat(path); err == nil {
		return path, false, nil
	} else if !os.IsNotExist(err) {
		return path, false, err
	}
	data, err := encodeConfigYAML(DefaultConfig())
	if err != nil {
		return path, false, err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return path, false, err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	_, err = file.Write(data)
	if err = errors.Join(err, file.Close()); err != nil {
		return path, false, err
	}
	// Publish a fully written template only if the destination is still absent.
	if err = os.Link(file.Name(), path); os.IsExist(err) {
		return path, false, nil
	} else if err != nil {
		return path, false, err
	}
	return path, true, nil
}

func encodeConfigYAML(value any) ([]byte, error) {
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func (m *Manager) GetConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ConfigDirName, ConfigFileName), nil
}
