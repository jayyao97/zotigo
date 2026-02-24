package builtin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebSearchTool_Execute(t *testing.T) {
	// Mock Tavily API server.
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/search" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		var req tavilyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// Validate API key.
		if req.APIKey != "test-api-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		resp := tavilyResponse{
			Results: []tavilyResult{
				{
					Title:   "Go Programming Language",
					URL:     "https://go.dev",
					Content: "Go is an open-source programming language supported by Google.",
					Score:   0.95,
				},
				{
					Title:   "Go Tutorial",
					URL:     "https://go.dev/tour",
					Content: "A Tour of Go is an introduction to the Go programming language.",
					Score:   0.85,
				},
			},
			Answer: "Go is a programming language by Google.",
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	webClient := NewWebClient(WebConfig{
		TavilyAPIKey:  "test-api-key",
		TavilyBaseURL: mockServer.URL,
	})
	tool := NewWebSearchTool(webClient)
	ctx := context.Background()

	t.Run("basic search", func(t *testing.T) {
		result, err := tool.Execute(ctx, nil, `{"query": "golang"}`)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		output := result.(string)
		if !strings.Contains(output, "Go Programming Language") {
			t.Errorf("expected title in output, got: %s", output)
		}
		if !strings.Contains(output, "https://go.dev") {
			t.Errorf("expected URL in output, got: %s", output)
		}
		if !strings.Contains(output, "Go is a programming language by Google") {
			t.Errorf("expected answer in output, got: %s", output)
		}
	})

	t.Run("search with params", func(t *testing.T) {
		result, err := tool.Execute(ctx, nil, `{
			"query": "golang",
			"search_depth": "advanced",
			"max_results": 2,
			"include_domains": ["go.dev"]
		}`)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		output := result.(string)
		if !strings.Contains(output, "Go Programming Language") {
			t.Errorf("expected results in output, got: %s", output)
		}
	})

	t.Run("missing query", func(t *testing.T) {
		_, err := tool.Execute(ctx, nil, `{}`)
		if err == nil {
			t.Error("expected error for missing query")
		}
	})

	t.Run("missing API key", func(t *testing.T) {
		noKeyClient := NewWebClient(WebConfig{
			TavilyAPIKey:  "",
			TavilyBaseURL: mockServer.URL,
		})
		// Clear env fallback by using empty key explicitly.
		noKeyClient.config.TavilyAPIKey = ""
		noKeyTool := NewWebSearchTool(noKeyClient)

		_, err := noKeyTool.Execute(ctx, nil, `{"query": "test"}`)
		if err == nil {
			t.Error("expected error for missing API key")
		}
		if !strings.Contains(err.Error(), "API key") {
			t.Errorf("expected API key error, got: %v", err)
		}
	})

	t.Run("nil web client", func(t *testing.T) {
		nilTool := &WebSearchTool{}
		_, err := nilTool.Execute(ctx, nil, `{"query": "test"}`)
		if err == nil {
			t.Error("expected error for nil web client")
		}
	})

	t.Run("max_results clamped to 10", func(t *testing.T) {
		// This just verifies it doesn't error; the mock doesn't check max_results.
		_, err := tool.Execute(ctx, nil, `{"query": "golang", "max_results": 50}`)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
	})
}

func TestWebSearchTool_Schema(t *testing.T) {
	tool := NewWebSearchTool(nil)

	if tool.Name() != "web_search" {
		t.Errorf("expected name 'web_search', got %q", tool.Name())
	}

	schema := tool.Schema()
	schemaMap, ok := schema.(map[string]any)
	if !ok {
		t.Fatal("schema is not a map")
	}

	props, ok := schemaMap["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties not found in schema")
	}

	if _, ok := props["query"]; !ok {
		t.Error("expected 'query' property in schema")
	}
	if _, ok := props["search_depth"]; !ok {
		t.Error("expected 'search_depth' property in schema")
	}

	required, ok := schemaMap["required"].([]string)
	if !ok {
		t.Fatal("required not found in schema")
	}
	if len(required) != 1 || required[0] != "query" {
		t.Errorf("expected required=['query'], got %v", required)
	}
}

func TestWebSearchTool_APIError(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error": "rate limited"}`, http.StatusTooManyRequests)
	}))
	defer mockServer.Close()

	webClient := NewWebClient(WebConfig{
		TavilyAPIKey:  "test-key",
		TavilyBaseURL: mockServer.URL,
	})
	tool := NewWebSearchTool(webClient)

	_, err := tool.Execute(context.Background(), nil, `{"query": "test"}`)
	if err == nil {
		t.Error("expected error for API error response")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("expected HTTP 429 in error, got: %v", err)
	}
}
