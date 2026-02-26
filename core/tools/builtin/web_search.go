package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/tools"
)

// SearchOptions holds per-query options passed from the tool to the provider.
type SearchOptions struct {
	Topic     string // "general" or "news"
	TimeRange string // "day", "week", "month", "year"
}

// SearchProvider is the interface for web search backends.
type SearchProvider interface {
	// Search performs a web search and returns provider-agnostic results.
	Search(ctx context.Context, query string, maxResults int, opts SearchOptions) ([]searchResult, error)
}

// searchResult is the provider-agnostic result used for formatting.
type searchResult struct {
	Title   string
	URL     string
	Content string
}

// NewSearchProvider creates a SearchProvider based on available API keys.
// Returns nil if no key is configured.
func NewSearchProvider(wc *WebClient) SearchProvider {
	if wc.config.TavilyAPIKey != "" {
		return &TavilySearchProvider{
			apiKey:  wc.config.TavilyAPIKey,
			baseURL: wc.config.TavilyBaseURL,
			client:  wc.client,
			ua:      wc.config.UserAgent,
		}
	}
	return nil
}

// WebSearchTool searches the web via a pluggable SearchProvider.
type WebSearchTool struct {
	provider SearchProvider
}

// NewWebSearchTool creates a new web_search tool backed by the given SearchProvider.
func NewWebSearchTool(provider SearchProvider) *WebSearchTool {
	return &WebSearchTool{provider: provider}
}

func (t *WebSearchTool) Name() string { return "web_search" }

func (t *WebSearchTool) Description() string {
	return "Search the web for up-to-date information. Returns relevant search results with titles, URLs, and content. Use topic='news' for recent events."
}

func (t *WebSearchTool) Schema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "The search query",
			},
			"max_results": map[string]any{
				"type":        "integer",
				"description": "Maximum number of results to return (1-20, default: 5)",
			},
			"topic": map[string]any{
				"type":        "string",
				"enum":        []string{"general", "news"},
				"description": "Search category: 'general' for broad searches, 'news' for recent events and updates (default: general)",
			},
			"time_range": map[string]any{
				"type":        "string",
				"enum":        []string{"day", "week", "month", "year"},
				"description": "Filter results by publish date recency (optional, useful for time-sensitive queries)",
			},
		},
		"required": []string{"query"},
	}
}

func (t *WebSearchTool) Safety() tools.ToolSafety {
	return tools.ToolSafety{ReadOnly: true}
}

func (t *WebSearchTool) Execute(ctx context.Context, _ executor.Executor, argsJSON string) (any, error) {
	var args struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
		Topic      string `json:"topic"`
		TimeRange  string `json:"time_range"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Query == "" {
		return nil, fmt.Errorf("query is required")
	}
	if args.MaxResults <= 0 {
		args.MaxResults = 5
	}
	if args.MaxResults > 20 {
		args.MaxResults = 20
	}

	opts := SearchOptions{
		Topic:     args.Topic,
		TimeRange: args.TimeRange,
	}

	results, err := t.provider.Search(ctx, args.Query, args.MaxResults, opts)
	if err != nil {
		return nil, err
	}

	return formatSearchResults(args.Query, results), nil
}

// ── Shared formatting ───────────────────────────────────────────────────

func formatSearchResults(query string, results []searchResult) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Search results for: %s\n", query))
	sb.WriteString(strings.Repeat("─", 40))
	sb.WriteByte('\n')

	if len(results) == 0 {
		sb.WriteString("No results found.")
		return sb.String()
	}

	for i, r := range results {
		sb.WriteString(fmt.Sprintf("\n[%d] %s\n", i+1, r.Title))
		if r.URL != "" {
			sb.WriteString(fmt.Sprintf("    URL: %s\n", r.URL))
		}
		if r.Content != "" {
			content := r.Content
			if len(content) > 5000 {
				content = content[:5000] + "\n    [... truncated]"
			}
			sb.WriteString(fmt.Sprintf("    %s\n", content))
		}
	}

	return sb.String()
}
