package config

import "fmt"

// DefaultContextWindow is the assumed context size when a profile
// does not specify ContextWindow. 200k matches every modern Claude,
// GPT-5/o, and Gemini family; smaller models (gpt-4 8k, local llama)
// need an explicit `context_window: <N>` in the profile rather than a
// baked-in per-model table here. The upstream numbers shift too often
// (gpt-4o went 128k → 200k mid-life) for a registry to stay accurate,
// and a stale guess looks more authoritative than "I don't know"
// while being just as wrong. Profile config is the source of truth.
const DefaultContextWindow = 200_000

// Config represents the top-level configuration structure.
type Config struct {
	DefaultProfile string                   `mapstructure:"default_profile" yaml:"default_profile"`
	Profiles       map[string]ProfileConfig `mapstructure:"profiles" yaml:"profiles"`
	Security       SecurityConfig           `mapstructure:"security" yaml:"security"`
	UI             UIConfig                 `mapstructure:"ui" yaml:"ui"`
	Tools          ToolsConfig              `mapstructure:"tools" yaml:"tools"`
	Observability  ObservabilityConfig      `mapstructure:"observability,omitempty" yaml:"observability,omitempty"`
}

// ObservabilityConfig controls external trace ingestion. Currently
// only Langfuse is wired in; future backends (OpenTelemetry, etc.)
// would slot in alongside.
type ObservabilityConfig struct {
	Langfuse LangfuseConfig `mapstructure:"langfuse,omitempty" yaml:"langfuse,omitempty"`
}

// LangfuseConfig points the agent at a Langfuse project. Empty
// PublicKey or SecretKey disables the integration entirely (the
// agent falls back to a no-op observer). Host defaults to
// https://cloud.langfuse.com when unset.
type LangfuseConfig struct {
	Enabled       bool   `mapstructure:"enabled,omitempty" yaml:"enabled,omitempty"`
	Host          string `mapstructure:"host,omitempty" yaml:"host,omitempty"`
	PublicKey     string `mapstructure:"public_key,omitempty" yaml:"public_key,omitempty"`
	SecretKey     string `mapstructure:"secret_key,omitempty" yaml:"secret_key,omitempty"`
	FlushInterval int    `mapstructure:"flush_interval_sec,omitempty" yaml:"flush_interval_sec,omitempty"`
}

// IsEnabled reports whether Langfuse should be wired in. Both keys
// must be non-empty AND the explicit Enabled flag must be true —
// having keys configured but Enabled=false is a valid state for
// quickly toggling telemetry off without removing credentials.
func (c LangfuseConfig) IsEnabled() bool {
	return c.Enabled && c.PublicKey != "" && c.SecretKey != ""
}

// ProfileConfig defines a specific configuration for an AI model usage.
// It maps a user-defined name (e.g., "code-buddy") to a provider implementation (e.g., "openai").
type ProfileConfig struct {
	Provider string `mapstructure:"provider" yaml:"provider"` // e.g. "openai", "claude"
	Model    string `mapstructure:"model" yaml:"model"`       // e.g. "gpt-4o"
	APIKey   string `mapstructure:"api_key" yaml:"api_key"`
	BaseURL  string `mapstructure:"base_url,omitempty" yaml:"base_url,omitempty"`

	// ThinkingLevel enables extended thinking/reasoning mode.
	// Values: "" (disabled), "low", "medium", "high".
	// Providers map this to their native thinking config:
	//   Anthropic: budget_tokens (low=2048, medium=8192, high=32768)
	//   OpenAI: reasoning_effort (low, medium, high)
	//   Gemini: ThinkingLevel (LOW, MEDIUM, HIGH)
	ThinkingLevel string `mapstructure:"thinking_level,omitempty" yaml:"thinking_level,omitempty"`

	// ContextWindow sets the model's context window (in tokens) for
	// the TUI status display and any future budget-aware logic. Set
	// this for any model where DefaultContextWindow would mislead —
	// older small models (gpt-4 8k), local servers (llama.cpp /
	// vLLM), Azure deployments with reduced context, etc. When 0,
	// DefaultContextWindow is used.
	ContextWindow int `mapstructure:"context_window,omitempty" yaml:"context_window,omitempty"`

	// Safety config controls optional safety classifier behavior for this profile.
	Safety SafetyProfileConfig `mapstructure:"safety,omitempty" yaml:"safety,omitempty"`

	// Additional provider-specific params can be added here or in a generic map
	Params map[string]any `mapstructure:"params,omitempty" yaml:"params,omitempty"`
}

// SafetyProfileConfig holds runtime safety behavior for a profile.
type SafetyProfileConfig struct {
	Classifier SafetyClassifierConfig `mapstructure:"classifier,omitempty" yaml:"classifier,omitempty"`
}

// SafetyClassifierConfig controls the lightweight safety classifier.
// Enabled uses *bool so config merging can distinguish "not set" (nil) from
// "explicitly disabled" (false). ReviewThreshold accepts the SafetyLevel
// names (safe | low | medium | high) or "off" to disable classifier calls
// entirely — any call at or above the threshold gets routed to the
// classifier in Auto mode.
type SafetyClassifierConfig struct {
	Enabled                *bool  `mapstructure:"enabled" yaml:"enabled"`
	ReviewThreshold        string `mapstructure:"review_threshold,omitempty" yaml:"review_threshold,omitempty"`
	Profile                string `mapstructure:"profile,omitempty" yaml:"profile,omitempty"`
	TimeoutMs              int    `mapstructure:"timeout_ms,omitempty" yaml:"timeout_ms,omitempty"`
	MaxRecentActions       int    `mapstructure:"max_recent_actions,omitempty" yaml:"max_recent_actions,omitempty"`
	CaptureRawAuditContext bool   `mapstructure:"capture_raw_audit_context" yaml:"capture_raw_audit_context"`
	MaxAuditContextChars   int    `mapstructure:"max_audit_context_chars,omitempty" yaml:"max_audit_context_chars,omitempty"`
}

// IsEnabled returns whether the classifier is enabled.
// Returns false when Enabled is nil. After Manager.Load() merges defaults
// for every profile (built-in and user-defined), nil is replaced with the
// default value (true), so IsEnabled() returns true unless the user
// explicitly set enabled: false.
func (c SafetyClassifierConfig) IsEnabled() bool {
	if c.Enabled == nil {
		return false
	}
	return *c.Enabled
}

// BoolPtr is a helper for constructing *bool values in config literals.
func BoolPtr(v bool) *bool { return &v }

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
				Safety: SafetyProfileConfig{
					Classifier: defaultSafetyClassifierConfig(),
				},
			},
			"claude-sonnet": {
				Provider: "claude",
				Model:    "claude-4-6-sonnet-latest",
				Safety: SafetyProfileConfig{
					Classifier: defaultSafetyClassifierConfig(),
				},
			},
			"gemini-pro": {
				Provider: "gemini",
				Model:    "gemini-3.0-pro-latest",
				Safety: SafetyProfileConfig{
					Classifier: defaultSafetyClassifierConfig(),
				},
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

func defaultSafetyClassifierConfig() SafetyClassifierConfig {
	return SafetyClassifierConfig{
		Enabled:                BoolPtr(true),
		ReviewThreshold:        "medium",
		TimeoutMs:              20000,
		MaxRecentActions:       6,
		CaptureRawAuditContext: false,
		MaxAuditContextChars:   1200,
	}
}

// ResolveClassifierProfile resolves the classifier profile for an active profile.
// If classifier.profile is empty, it reuses the active profile itself.
func (c *Config) ResolveClassifierProfile(activeProfileName string) (string, ProfileConfig, error) {
	if c == nil {
		return "", ProfileConfig{}, fmt.Errorf("config is nil")
	}
	active, ok := c.Profiles[activeProfileName]
	if !ok {
		return "", ProfileConfig{}, fmt.Errorf("active profile %q not found", activeProfileName)
	}

	targetName := activeProfileName
	if name := active.Safety.Classifier.Profile; name != "" {
		targetName = name
	}

	target, ok := c.Profiles[targetName]
	if !ok {
		return "", ProfileConfig{}, fmt.Errorf("classifier profile %q not found", targetName)
	}
	return targetName, target, nil
}
