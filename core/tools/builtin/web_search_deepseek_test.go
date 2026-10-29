package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeepSeekSearch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/tools/search" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request["query"] != "中文 site:example.com after:2026-01-01" || request["count"] != float64(5) || request["need_content"] != false || len(request) != 3 {
			t.Errorf("unexpected body: %v", request)
		}
		_, _ = fmt.Fprint(w, `{"results":[{"title":"标题","url":"https://example.com","summary":"完整摘要","snippet":"短摘要"},{"title":"备用","snippet":"备用摘要"}]}`)
	}))
	defer server.Close()
	provider, err := NewSearchProvider(NewWebClient(WebConfig{SearchProvider: "deepseek", DeepSeekAPIKey: "test-key", DeepSeekBaseURL: server.URL}))
	if err != nil {
		t.Fatal(err)
	}
	tool := NewWebSearchTool(provider)
	result, err := tool.Execute(context.Background(), nil, `{"query":"中文 site:example.com after:2026-01-01"}`)
	if err != nil {
		t.Fatal(err)
	}
	text := result.(string)
	for _, want := range []string{"标题", "https://example.com", "完整摘要", "备用摘要"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %s", want, text)
		}
	}
	properties := tool.Schema().(map[string]any)["properties"].(map[string]any)
	if properties["topic"] != nil || properties["time_range"] != nil {
		t.Fatal("advertises unsupported parameters")
	}
}

func TestDeepSeekSearchValidationAndResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantError  bool
	}{
		{"empty", `{"results":[]}`, 200, false},
		{"missing", `{}`, 200, true},
		{"null", `{"results":null}`, 200, true},
		{"malformed", `{`, 200, true},
		{"unauthorized", `secret`, 401, true},
		{"rate limited", `secret`, 429, true},
		{"oversized", strings.Repeat("x", 1024*1024+1), 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); _, _ = fmt.Fprint(w, tc.body) }))
			defer server.Close()
			provider := &DeepSeekSearchProvider{baseURL: server.URL, client: server.Client()}
			_, err := provider.Search(context.Background(), strings.Repeat("中", 128), 5, SearchOptions{})
			if (err != nil) != tc.wantError {
				t.Fatalf("unexpected error: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("response body leaked")
			}
		})
	}
	provider := &DeepSeekSearchProvider{}
	for _, query := range []string{"", "  \n", strings.Repeat("中", 129)} {
		if _, err := provider.Search(context.Background(), query, 5, SearchOptions{}); err == nil {
			t.Fatalf("accepted invalid query %q", query)
		}
	}
	for _, opts := range []SearchOptions{{Topic: "news"}, {TimeRange: "day"}} {
		if _, err := provider.Search(context.Background(), "query", 5, opts); err == nil {
			t.Fatal("accepted unsupported option")
		}
	}
}

func TestSearchProviderSelection(t *testing.T) {
	t.Setenv("TAVILY_API_KEY", "")
	t.Setenv("DEEPSEEK_API_KEY", "env-key")
	for _, tc := range []struct {
		name      string
		config    WebConfig
		want      string
		wantError bool
	}{
		{"unchanged default", WebConfig{}, "<nil>", false},
		{"tavily precedence", WebConfig{TavilyAPIKey: "key"}, "*builtin.TavilySearchProvider", false},
		{"explicit deepseek", WebConfig{SearchProvider: "deepseek", TavilyAPIKey: "key"}, "*builtin.DeepSeekSearchProvider", false},
		{"unknown", WebConfig{SearchProvider: "typo"}, "<nil>", true},
		{"missing tavily key", WebConfig{SearchProvider: "tavily"}, "<nil>", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider, err := NewSearchProvider(NewWebClient(tc.config))
			if (err != nil) != tc.wantError || fmt.Sprintf("%T", provider) != tc.want {
				t.Fatalf("provider %T, error %v", provider, err)
			}
		})
	}
	t.Setenv("DEEPSEEK_API_KEY", "")
	if _, err := NewSearchProvider(NewWebClient(WebConfig{SearchProvider: "deepseek"})); err == nil {
		t.Fatal("missing key accepted")
	}
}
