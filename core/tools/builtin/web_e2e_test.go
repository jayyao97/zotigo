//go:build e2e

package builtin

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSearchE2E_Tavily(t *testing.T) {
	apiKey := os.Getenv("TAVILY_API_KEY")
	if apiKey == "" {
		t.Skip("TAVILY_API_KEY not set, skipping")
	}

	provider := &TavilySearchProvider{
		apiKey:  apiKey,
		baseURL: "https://api.tavily.com",
		client:  &http.Client{Timeout: 30 * time.Second},
	}
	ctx := context.Background()

	t.Run("basic search", func(t *testing.T) {
		results, err := provider.Search(ctx, "Go programming language official website", 3, SearchOptions{})
		if err != nil {
			t.Fatalf("Search failed: %v", err)
		}
		if len(results) == 0 {
			t.Fatal("expected at least 1 result")
		}
		for i, r := range results {
			t.Logf("[%d] %s\n    URL: %s\n    Content (%d chars): %.200s\n", i+1, r.Title, r.URL, len(r.Content), r.Content)
		}
		found := false
		for _, r := range results {
			if strings.Contains(strings.ToLower(r.Title+r.Content), "go") {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected 'go' in search results")
		}
	})

	t.Run("news query", func(t *testing.T) {
		results, err := provider.Search(ctx, "latest AI news 2025", 3, SearchOptions{Topic: "news"})
		if err != nil {
			t.Fatalf("Search failed: %v", err)
		}
		for i, r := range results {
			t.Logf("[%d] %s\n    URL: %s\n    Content (%d chars): %.200s\n", i+1, r.Title, r.URL, len(r.Content), r.Content)
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
