package builtin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── Tavily tests ────────────────────────────────────────────────────────

func newTavilyMock() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/search" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var req tavilyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.APIKey != "test-tavily-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		resp := tavilyResponse{
			Results: []tavilyResult{
				{Title: "Go Programming", URL: "https://go.dev", Content: "Go is open-source.", Score: 0.95},
				{Title: "Go Tutorial", URL: "https://go.dev/tour", Content: "A Tour of Go.", Score: 0.85},
			},
			Answer: "Go is a language by Google.",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
}

func TestWebSearchTool_Tavily(t *testing.T) {
	mock := newTavilyMock()
	defer mock.Close()

	provider := &TavilySearchProvider{
		apiKey:  "test-tavily-key",
		baseURL: mock.URL,
		client:  http.DefaultClient,
	}
	tool := NewWebSearchTool(provider)
	ctx := context.Background()

	t.Run("basic search", func(t *testing.T) {
		result, err := tool.Execute(ctx, nil, `{"query": "golang"}`)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		output := result.(string)
		if !strings.Contains(output, "Go Programming") {
			t.Errorf("expected title in output, got: %s", output)
		}
		if !strings.Contains(output, "https://go.dev") {
			t.Errorf("expected URL in output, got: %s", output)
		}
		if !strings.Contains(output, "Go is a language by Google") {
			t.Errorf("expected AI summary in output, got: %s", output)
		}
	})

	t.Run("max_results clamped", func(t *testing.T) {
		_, err := tool.Execute(ctx, nil, `{"query": "golang", "max_results": 50}`)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
	})
}

func TestTavilySearchProvider_APIError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error": "rate limited"}`, http.StatusTooManyRequests)
	}))
	defer mock.Close()

	provider := &TavilySearchProvider{apiKey: "key", baseURL: mock.URL, client: http.DefaultClient}
	_, err := provider.Search(context.Background(), "test", 5, SearchOptions{})
	if err == nil {
		t.Error("expected error for API error response")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("expected HTTP 429 in error, got: %v", err)
	}
}

// ── Provider factory tests ──────────────────────────────────────────────

func TestNewSearchProvider(t *testing.T) {
	t.Run("tavily provider", func(t *testing.T) {
		wc := NewWebClient(WebConfig{TavilyAPIKey: "tavily-key"})
		sp := NewSearchProvider(wc)
		if sp == nil {
			t.Fatal("expected non-nil provider")
		}
		if _, ok := sp.(*TavilySearchProvider); !ok {
			t.Errorf("expected TavilySearchProvider, got %T", sp)
		}
	})

	t.Run("nil when no key", func(t *testing.T) {
		wc := NewWebClient(WebConfig{})
		// Clear env fallback
		wc.config.TavilyAPIKey = ""
		sp := NewSearchProvider(wc)
		if sp != nil {
			t.Errorf("expected nil provider, got %T", sp)
		}
	})
}

// ── Common tests ────────────────────────────────────────────────────────

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

	required, ok := schemaMap["required"].([]string)
	if !ok {
		t.Fatal("required not found in schema")
	}
	if len(required) != 1 || required[0] != "query" {
		t.Errorf("expected required=['query'], got %v", required)
	}
}

func TestWebSearchTool_MissingQuery(t *testing.T) {
	provider := &TavilySearchProvider{apiKey: "key", baseURL: "http://x", client: http.DefaultClient}
	tool := NewWebSearchTool(provider)
	_, err := tool.Execute(context.Background(), nil, `{}`)
	if err == nil {
		t.Error("expected error for missing query")
	}
}
