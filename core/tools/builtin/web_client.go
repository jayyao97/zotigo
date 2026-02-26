package builtin

import (
	"net/http"
	"os"
	"time"
)

// WebConfig holds configuration for web-related tools.
type WebConfig struct {
	TavilyAPIKey  string
	TavilyBaseURL string // default "https://api.tavily.com", overridable for tests
	UserAgent     string
	Timeout       time.Duration
	MaxPageSize   int
}

// WebClient is a shared HTTP client and config bundle for web tools.
type WebClient struct {
	config WebConfig
	client *http.Client
}

// NewWebClient creates a new WebClient with the given configuration.
// Use NewSearchProvider(wc) to obtain the search backend (nil if no key is set).
func NewWebClient(cfg WebConfig) *WebClient {
	if cfg.TavilyAPIKey == "" {
		cfg.TavilyAPIKey = os.Getenv("TAVILY_API_KEY")
	}
	if cfg.TavilyBaseURL == "" {
		cfg.TavilyBaseURL = "https://api.tavily.com"
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "Zotigo/1.0"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.MaxPageSize <= 0 {
		cfg.MaxPageSize = 5 * 1024 * 1024 // 5MB
	}

	return &WebClient{
		config: cfg,
		client: &http.Client{Timeout: cfg.Timeout},
	}
}
