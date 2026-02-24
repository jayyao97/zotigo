package builtin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebFetchTool_HTMLResponse(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><body>
			<nav>Menu</nav>
			<main>
				<h1>Hello World</h1>
				<p>This is a <strong>test</strong> page.</p>
				<a href="https://example.com">Link</a>
			</main>
			<footer>Footer</footer>
		</body></html>`))
	}))
	defer mockServer.Close()

	webClient := NewWebClient(WebConfig{TavilyBaseURL: mockServer.URL})
	tool := NewWebFetchTool(webClient)
	ctx := context.Background()

	t.Run("HTML with extract_main=true", func(t *testing.T) {
		result, err := tool.Execute(ctx, nil, `{"url": "`+mockServer.URL+`"}`)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		output := result.(string)
		if !strings.Contains(output, "# Hello World") {
			t.Errorf("expected heading in output, got: %s", output)
		}
		if !strings.Contains(output, "**test**") {
			t.Errorf("expected bold text in output, got: %s", output)
		}
		if !strings.Contains(output, "[Link](https://example.com)") {
			t.Errorf("expected markdown link in output, got: %s", output)
		}
		// Chrome should be stripped since extract_main is true.
		if strings.Contains(output, "Menu") {
			t.Errorf("expected nav to be stripped, got: %s", output)
		}
	})

	t.Run("HTML with extract_main=false", func(t *testing.T) {
		result, err := tool.Execute(ctx, nil, `{"url": "`+mockServer.URL+`", "extract_main": false}`)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		output := result.(string)
		// Plain text should include all visible text.
		if !strings.Contains(output, "Hello World") {
			t.Errorf("expected heading text in output, got: %s", output)
		}
	})
}

func TestWebFetchTool_MarkdownResponse(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check that Accept header requests markdown.
		accept := r.Header.Get("Accept")
		if !strings.Contains(accept, "text/markdown") {
			t.Errorf("expected Accept header to include text/markdown, got: %s", accept)
		}

		// Simulate Cloudflare markdown response.
		w.Header().Set("Content-Type", "text/markdown")
		w.Header().Set("X-Markdown-Tokens", "1234")
		w.Write([]byte("# Hello World\n\nThis is **markdown** content.\n"))
	}))
	defer mockServer.Close()

	webClient := NewWebClient(WebConfig{TavilyBaseURL: mockServer.URL})
	tool := NewWebFetchTool(webClient)

	result, err := tool.Execute(context.Background(), nil, `{"url": "`+mockServer.URL+`"}`)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	output := result.(string)
	// Markdown should be passed through as-is.
	if !strings.Contains(output, "# Hello World") {
		t.Errorf("expected markdown heading in output, got: %s", output)
	}
	if !strings.Contains(output, "**markdown**") {
		t.Errorf("expected bold markdown in output, got: %s", output)
	}
	// Should include token metadata.
	if !strings.Contains(output, "Markdown-Tokens: 1234") {
		t.Errorf("expected markdown tokens header, got: %s", output)
	}
}

func TestWebFetchTool_JSONResponse(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"name":"test","values":[1,2,3]}`))
	}))
	defer mockServer.Close()

	webClient := NewWebClient(WebConfig{TavilyBaseURL: mockServer.URL})
	tool := NewWebFetchTool(webClient)

	result, err := tool.Execute(context.Background(), nil, `{"url": "`+mockServer.URL+`"}`)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	output := result.(string)
	// JSON should be pretty-printed.
	if !strings.Contains(output, "\"name\": \"test\"") {
		t.Errorf("expected pretty-printed JSON in output, got: %s", output)
	}
}

func TestWebFetchTool_AutoPrependHTTPS(t *testing.T) {
	// We can't easily test URL rewriting since it would try to connect to a real host.
	// Instead, test that the tool handles a URL without scheme by checking args parsing.
	webClient := NewWebClient(WebConfig{})
	tool := NewWebFetchTool(webClient)

	// The request will fail (no real server) but the error should show the corrected URL.
	_, err := tool.Execute(context.Background(), nil, `{"url": "localhost:99999/nonexistent"}`)
	if err == nil {
		t.Error("expected error for unreachable host")
	}
	// It should have tried https://localhost:99999/nonexistent, not failed on URL parsing.
	if strings.Contains(err.Error(), "invalid arguments") {
		t.Errorf("expected fetch error, not argument error, got: %v", err)
	}
}

func TestWebFetchTool_MaxLength(t *testing.T) {
	// Create a server that returns a large body.
	bigContent := strings.Repeat("A", 100000)
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(bigContent))
	}))
	defer mockServer.Close()

	webClient := NewWebClient(WebConfig{TavilyBaseURL: mockServer.URL})
	tool := NewWebFetchTool(webClient)

	result, err := tool.Execute(context.Background(), nil, `{"url": "`+mockServer.URL+`", "max_length": 1000}`)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	output := result.(string)
	if !strings.Contains(output, "[... truncated]") {
		t.Errorf("expected truncation notice in output, got length: %d", len(output))
	}
	// Output should be metadata header + content, bounded by max_length on content.
	// Total output should be reasonably bounded.
	if len(output) > 2000 {
		t.Errorf("expected output to be bounded, got length: %d", len(output))
	}
}

func TestWebFetchTool_HTTPError(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "page not found", http.StatusNotFound)
	}))
	defer mockServer.Close()

	webClient := NewWebClient(WebConfig{TavilyBaseURL: mockServer.URL})
	tool := NewWebFetchTool(webClient)

	_, err := tool.Execute(context.Background(), nil, `{"url": "`+mockServer.URL+`"}`)
	if err == nil {
		t.Error("expected error for 404 response")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("expected HTTP 404 in error, got: %v", err)
	}
}

func TestWebFetchTool_MissingURL(t *testing.T) {
	webClient := NewWebClient(WebConfig{})
	tool := NewWebFetchTool(webClient)

	_, err := tool.Execute(context.Background(), nil, `{}`)
	if err == nil {
		t.Error("expected error for missing URL")
	}
	if !strings.Contains(err.Error(), "url is required") {
		t.Errorf("expected 'url is required' error, got: %v", err)
	}
}

func TestWebFetchTool_NilWebClient(t *testing.T) {
	tool := &WebFetchTool{}

	_, err := tool.Execute(context.Background(), nil, `{"url": "https://example.com"}`)
	if err == nil {
		t.Error("expected error for nil web client")
	}
}

func TestWebFetchTool_Schema(t *testing.T) {
	tool := NewWebFetchTool(nil)

	if tool.Name() != "web_fetch" {
		t.Errorf("expected name 'web_fetch', got %q", tool.Name())
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

	if _, ok := props["url"]; !ok {
		t.Error("expected 'url' property in schema")
	}
	if _, ok := props["max_length"]; !ok {
		t.Error("expected 'max_length' property in schema")
	}
	if _, ok := props["extract_main"]; !ok {
		t.Error("expected 'extract_main' property in schema")
	}

	required, ok := schemaMap["required"].([]string)
	if !ok {
		t.Fatal("required not found in schema")
	}
	if len(required) != 1 || required[0] != "url" {
		t.Errorf("expected required=['url'], got %v", required)
	}
}

func TestWebFetchTool_MetadataHeader(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("plain text content"))
	}))
	defer mockServer.Close()

	webClient := NewWebClient(WebConfig{TavilyBaseURL: mockServer.URL})
	tool := NewWebFetchTool(webClient)

	result, err := tool.Execute(context.Background(), nil, `{"url": "`+mockServer.URL+`"}`)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	output := result.(string)
	if !strings.Contains(output, "URL: "+mockServer.URL) {
		t.Errorf("expected URL in metadata header, got: %s", output)
	}
	if !strings.Contains(output, "Status: 200") {
		t.Errorf("expected status in metadata header, got: %s", output)
	}
	if !strings.Contains(output, "Content-Type: text/plain") {
		t.Errorf("expected content-type in metadata header, got: %s", output)
	}
	if !strings.Contains(output, "plain text content") {
		t.Errorf("expected body content in output, got: %s", output)
	}
}
