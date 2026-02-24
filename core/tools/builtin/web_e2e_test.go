//go:build e2e

package builtin

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func loadTavilyKey(t *testing.T) string {
	t.Helper()

	// 1. Try e2e.config.json (project root)
	for _, path := range []string{"../../../e2e.config.json", "e2e.config.json"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cfg struct {
			Tavily struct {
				APIKey string `json:"api_key"`
			} `json:"tavily"`
		}
		if json.Unmarshal(data, &cfg) == nil && cfg.Tavily.APIKey != "" {
			return cfg.Tavily.APIKey
		}
	}

	// 2. Try ~/.zotigo/config.yaml via env
	if key := os.Getenv("TAVILY_API_KEY"); key != "" {
		return key
	}

	// 3. Hardcoded fallback for local dev (same as config)
	return "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"
}

func TestWebSearchE2E(t *testing.T) {
	apiKey := loadTavilyKey(t)
	if apiKey == "" {
		t.Skip("No Tavily API key available, skipping e2e test")
	}

	webClient := NewWebClient(WebConfig{
		TavilyAPIKey: apiKey,
		Timeout:      30 * time.Second,
	})
	tool := NewWebSearchTool(webClient)
	ctx := context.Background()

	t.Run("search for Go programming language", func(t *testing.T) {
		result, err := tool.Execute(ctx, nil, `{"query": "Go programming language official website", "max_results": 3}`)
		if err != nil {
			t.Fatalf("web_search failed: %v", err)
		}

		output := result.(string)
		t.Logf("Search result:\n%s", output)

		if !strings.Contains(output, "Search results for") {
			t.Error("expected result header")
		}
		if !strings.Contains(strings.ToLower(output), "go") {
			t.Error("expected 'go' in search results")
		}
	})

	t.Run("search with domain filter", func(t *testing.T) {
		result, err := tool.Execute(ctx, nil, `{
			"query": "rust programming",
			"max_results": 3,
			"include_domains": ["rust-lang.org"]
		}`)
		if err != nil {
			t.Fatalf("web_search failed: %v", err)
		}

		output := result.(string)
		t.Logf("Filtered search result:\n%s", output)
	})

	t.Run("search advanced depth", func(t *testing.T) {
		result, err := tool.Execute(ctx, nil, `{
			"query": "what is WebAssembly",
			"search_depth": "advanced",
			"max_results": 2
		}`)
		if err != nil {
			t.Fatalf("web_search failed: %v", err)
		}

		output := result.(string)
		t.Logf("Advanced search result:\n%s", output)

		if !strings.Contains(strings.ToLower(output), "wasm") && !strings.Contains(strings.ToLower(output), "webassembly") {
			t.Error("expected WebAssembly-related content in results")
		}
	})

	t.Run("search news with time_range", func(t *testing.T) {
		result, err := tool.Execute(ctx, nil, `{
			"query": "AI",
			"topic": "news",
			"time_range": "week",
			"max_results": 3
		}`)
		if err != nil {
			t.Fatalf("web_search failed: %v", err)
		}

		output := result.(string)
		t.Logf("News time_range result:\n%s", output)

		if !strings.Contains(output, "Search results for") {
			t.Error("expected result header")
		}
	})

	t.Run("search with date range", func(t *testing.T) {
		result, err := tool.Execute(ctx, nil, `{
			"query": "Go 1.24 release",
			"start_date": "2025-01-01",
			"end_date": "2025-12-31",
			"max_results": 3
		}`)
		if err != nil {
			t.Fatalf("web_search failed: %v", err)
		}

		output := result.(string)
		t.Logf("Date range result:\n%s", output)

		if !strings.Contains(output, "Search results for") {
			t.Error("expected result header")
		}
	})
}

func TestWebFetchE2E(t *testing.T) {
	webClient := NewWebClient(WebConfig{
		Timeout: 30 * time.Second,
	})
	tool := NewWebFetchTool(webClient)
	ctx := context.Background()

	t.Run("fetch example.com", func(t *testing.T) {
		result, err := tool.Execute(ctx, nil, `{"url": "https://example.com"}`)
		if err != nil {
			t.Fatalf("web_fetch failed: %v", err)
		}

		output := result.(string)
		t.Logf("Fetch result (first 500 chars):\n%.500s", output)

		if !strings.Contains(output, "Status: 200") {
			t.Error("expected HTTP 200 status")
		}
		if !strings.Contains(output, "Example Domain") {
			t.Error("expected 'Example Domain' in output")
		}
	})

	t.Run("fetch JSON API", func(t *testing.T) {
		result, err := tool.Execute(ctx, nil, `{"url": "https://httpbin.org/json"}`)
		if err != nil {
			t.Fatalf("web_fetch failed: %v", err)
		}

		output := result.(string)
		t.Logf("JSON fetch result (first 500 chars):\n%.500s", output)

		if !strings.Contains(output, "application/json") {
			t.Error("expected JSON content type")
		}
	})

	t.Run("fetch with auto https", func(t *testing.T) {
		result, err := tool.Execute(ctx, nil, `{"url": "example.com"}`)
		if err != nil {
			t.Fatalf("web_fetch failed: %v", err)
		}

		output := result.(string)
		if !strings.Contains(output, "Example Domain") {
			t.Error("expected auto-https to work")
		}
	})

	t.Run("fetch with max_length", func(t *testing.T) {
		result, err := tool.Execute(ctx, nil, `{"url": "https://example.com", "max_length": 100}`)
		if err != nil {
			t.Fatalf("web_fetch failed: %v", err)
		}

		output := result.(string)
		if !strings.Contains(output, "[... truncated]") {
			t.Logf("Output length: %d, content: %s", len(output), output)
		}
	})
}
