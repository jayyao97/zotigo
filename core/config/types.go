package config

// Config represents the top-level configuration structure.
type Config struct {
	DefaultProfile string                   `mapstructure:"default_profile" yaml:"default_profile"`
	Profiles       map[string]ProfileConfig `mapstructure:"profiles" yaml:"profiles"`
	Security       SecurityConfig           `mapstructure:"security" yaml:"security"`
	UI             UIConfig                 `mapstructure:"ui" yaml:"ui"`
	Tools          ToolsConfig              `mapstructure:"tools" yaml:"tools"`
}

// ProfileConfig defines a specific configuration for an AI model usage.
// It maps a user-defined name (e.g., "code-buddy") to a provider implementation (e.g., "openai").
type ProfileConfig struct {
	Provider string `mapstructure:"provider" yaml:"provider"` // e.g. "openai", "claude"
	Model    string `mapstructure:"model" yaml:"model"`       // e.g. "gpt-4o"
	APIKey   string `mapstructure:"api_key" yaml:"api_key"`
	BaseURL  string `mapstructure:"base_url,omitempty" yaml:"base_url,omitempty"`
	
	// Additional provider-specific params can be added here or in a generic map
	Params map[string]any `mapstructure:"params,omitempty" yaml:"params,omitempty"`
}

// SecurityConfig holds security-related settings.
type SecurityConfig struct {
	SandboxEnabled bool     `mapstructure:"sandbox_enabled" yaml:"sandbox_enabled"`
	AllowedTools   []string `mapstructure:"allowed_tools" yaml:"allowed_tools"`
}

// UIConfig holds UI preferences.
type UIConfig struct {
	Theme string `mapstructure:"theme" yaml:"theme"`
}

// ToolsConfig holds configuration for built-in tools.
type ToolsConfig struct {
	Web WebToolsConfig `mapstructure:"web" yaml:"web"`
}

// WebToolsConfig holds configuration for web-related tools (web_search, web_fetch).
type WebToolsConfig struct {
	TavilyAPIKey string `mapstructure:"tavily_api_key" yaml:"tavily_api_key"`
	UserAgent    string `mapstructure:"user_agent" yaml:"user_agent"`
	TimeoutSec   int    `mapstructure:"timeout_sec" yaml:"timeout_sec"`
	MaxPageSize  int    `mapstructure:"max_page_size" yaml:"max_page_size"`
}

// DefaultConfig returns the default configuration values.
func DefaultConfig() *Config {
	return &Config{
		DefaultProfile: "gpt-4o",
		Profiles: map[string]ProfileConfig{
			"gpt-4o": {
				Provider: "openai",
				Model:    "gpt-4o",
			},
			"claude-sonnet": {
				Provider: "claude",
				Model:    "claude-3-5-sonnet-latest",
			},
			"gemini-pro": {
				Provider: "gemini",
				Model:    "gemini-1.5-pro-latest",
			},
		},
		Security: SecurityConfig{
			SandboxEnabled: true,
			AllowedTools:   []string{"ls", "cat", "grep"},
		},
		UI: UIConfig{
			Theme: "dark",
		},
		Tools: ToolsConfig{
			Web: WebToolsConfig{
				UserAgent:   "Zotigo/1.0",
				TimeoutSec:  15,
				MaxPageSize: 5 * 1024 * 1024, // 5MB
			},
		},
	}
}