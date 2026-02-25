package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/tools"
)

// WebSearchTool searches the web using the Tavily Search API.
type WebSearchTool struct {
	web *WebClient
}

// NewWebSearchTool creates a new web_search tool backed by the given WebClient.
func NewWebSearchTool(web *WebClient) *WebSearchTool {
	return &WebSearchTool{web: web}
}

func (t *WebSearchTool) Name() string { return "web_search" }

func (t *WebSearchTool) Description() string {
	return "Search the web using Tavily Search API. Returns relevant search results with titles, URLs, and content snippets. Useful for finding up-to-date information, documentation, and answers to questions."
}

func (t *WebSearchTool) Schema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "The search query",
			},
			"search_depth": map[string]any{
				"type":        "string",
				"description": "Search depth: 'basic' (faster) or 'advanced' (more thorough). Default: 'basic'",
				"enum":        []string{"basic", "advanced"},
			},
			"topic": map[string]any{
				"type":        "string",
				"description": "Search category: 'general' (default), 'news' (recent news articles), or 'finance' (financial data)",
				"enum":        []string{"general", "news", "finance"},
			},
			"time_range": map[string]any{
				"type":        "string",
				"description": "Filter results by recency: 'day', 'week', 'month', or 'year'",
				"enum":        []string{"day", "week", "month", "year"},
			},
			"start_date": map[string]any{
				"type":        "string",
				"description": "Only include results published after this date (format: YYYY-MM-DD)",
			},
			"end_date": map[string]any{
				"type":        "string",
				"description": "Only include results published before this date (format: YYYY-MM-DD)",
			},
			"max_results": map[string]any{
				"type":        "integer",
				"description": "Maximum number of results to return (1-20, default: 5)",
			},
			"include_domains": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Only include results from these domains",
			},
			"exclude_domains": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Exclude results from these domains",
			},
		},
		"required": []string{"query"},
	}
}

// tavilyRequest is the request body for the Tavily search API.
type tavilyRequest struct {
	APIKey         string   `json:"api_key"`
	Query          string   `json:"query"`
	SearchDepth    string   `json:"search_depth,omitempty"`
	Topic          string   `json:"topic,omitempty"`
	TimeRange      string   `json:"time_range,omitempty"`
	StartDate      string   `json:"start_date,omitempty"`
	EndDate        string   `json:"end_date,omitempty"`
	MaxResults     int      `json:"max_results,omitempty"`
	IncludeDomains []string `json:"include_domains,omitempty"`
	ExcludeDomains []string `json:"exclude_domains,omitempty"`
}

// tavilyResponse is the response from the Tavily search API.
type tavilyResponse struct {
	Results []tavilyResult `json:"results"`
	Answer  string         `json:"answer,omitempty"`
}

type tavilyResult struct {
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	Content string  `json:"content"`
	Score   float64 `json:"score"`
}

func (t *WebSearchTool) Safety() tools.ToolSafety {
	return tools.ToolSafety{ReadOnly: true}
}

func (t *WebSearchTool) Execute(ctx context.Context, _ executor.Executor, argsJSON string) (any, error) {
	if t.web == nil {
		return nil, fmt.Errorf("web client not initialized")
	}

	var args struct {
		Query          string   `json:"query"`
		SearchDepth    string   `json:"search_depth"`
		Topic          string   `json:"topic"`
		TimeRange      string   `json:"time_range"`
		StartDate      string   `json:"start_date"`
		EndDate        string   `json:"end_date"`
		MaxResults     int      `json:"max_results"`
		IncludeDomains []string `json:"include_domains"`
		ExcludeDomains []string `json:"exclude_domains"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if args.Query == "" {
		return nil, fmt.Errorf("query is required")
	}

	if t.web.config.TavilyAPIKey == "" {
		return nil, fmt.Errorf("Tavily API key not configured. Set tools.web.tavily_api_key in config or TAVILY_API_KEY env var")
	}

	// Apply defaults and limits.
	if args.SearchDepth == "" {
		args.SearchDepth = "basic"
	}
	if args.MaxResults <= 0 {
		args.MaxResults = 5
	}
	if args.MaxResults > 20 {
		args.MaxResults = 20
	}

	reqBody := tavilyRequest{
		APIKey:         t.web.config.TavilyAPIKey,
		Query:          args.Query,
		SearchDepth:    args.SearchDepth,
		Topic:          args.Topic,
		TimeRange:      args.TimeRange,
		StartDate:      args.StartDate,
		EndDate:        args.EndDate,
		MaxResults:     args.MaxResults,
		IncludeDomains: args.IncludeDomains,
		ExcludeDomains: args.ExcludeDomains,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := t.web.config.TavilyBaseURL + "/search"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", t.web.config.UserAgent)

	resp, err := t.web.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024)) // 1MB limit
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Tavily API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	var tavilyResp tavilyResponse
	if err := json.Unmarshal(respBody, &tavilyResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return formatSearchResults(args.Query, &tavilyResp), nil
}

func formatSearchResults(query string, resp *tavilyResponse) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Search results for: %s\n", query))
	sb.WriteString(strings.Repeat("─", 40))
	sb.WriteByte('\n')

	if len(resp.Results) == 0 {
		sb.WriteString("No results found.")
		return sb.String()
	}

	for i, r := range resp.Results {
		sb.WriteString(fmt.Sprintf("\n[%d] %s\n", i+1, r.Title))
		sb.WriteString(fmt.Sprintf("    URL: %s\n", r.URL))
		if r.Content != "" {
			// Truncate long content snippets.
			content := r.Content
			if len(content) > 500 {
				content = content[:500] + "..."
			}
			sb.WriteString(fmt.Sprintf("    %s\n", content))
		}
	}

	if resp.Answer != "" {
		sb.WriteByte('\n')
		sb.WriteString(strings.Repeat("─", 40))
		sb.WriteString(fmt.Sprintf("\nSummary: %s\n", resp.Answer))
	}

	return sb.String()
}
