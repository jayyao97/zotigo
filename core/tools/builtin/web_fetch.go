package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/tools"
)

// WebFetchTool fetches a web page and returns its content as readable text.
type WebFetchTool struct {
	web *WebClient
}

// NewWebFetchTool creates a new web_fetch tool backed by the given WebClient.
func NewWebFetchTool(web *WebClient) *WebFetchTool {
	return &WebFetchTool{web: web}
}

func (t *WebFetchTool) Name() string { return "web_fetch" }

func (t *WebFetchTool) Description() string {
	return "Fetch a web page and return its content as readable text. Automatically converts HTML to a clean, readable format. Supports Cloudflare markdown responses for higher quality output. Use this to read documentation, articles, or any web page."
}

func (t *WebFetchTool) Schema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "The URL to fetch",
			},
			"max_length": map[string]any{
				"type":        "integer",
				"description": "Maximum number of characters to return (default: 50000)",
			},
			"extract_main": map[string]any{
				"type":        "boolean",
				"description": "Intelligently extract main content, removing navigation/chrome (default: true)",
			},
		},
		"required": []string{"url"},
	}
}

func (t *WebFetchTool) Safety() tools.ToolSafety {
	return tools.ToolSafety{ReadOnly: true}
}

func (t *WebFetchTool) Execute(ctx context.Context, _ executor.Executor, argsJSON string) (any, error) {
	if t.web == nil {
		return nil, fmt.Errorf("web client not initialized")
	}

	var args struct {
		URL         string `json:"url"`
		MaxLength   int    `json:"max_length"`
		ExtractMain *bool  `json:"extract_main"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if args.URL == "" {
		return nil, fmt.Errorf("url is required")
	}

	// Auto-prepend https:// if no scheme is present.
	if !strings.HasPrefix(args.URL, "http://") && !strings.HasPrefix(args.URL, "https://") {
		args.URL = "https://" + args.URL
	}

	if args.MaxLength <= 0 {
		args.MaxLength = 50000
	}

	extractMain := true
	if args.ExtractMain != nil {
		extractMain = *args.ExtractMain
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, args.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	// Prefer markdown (Cloudflare), fallback to HTML.
	req.Header.Set("Accept", "text/markdown, text/html;q=0.9, */*;q=0.1")
	req.Header.Set("User-Agent", t.web.config.UserAgent)

	resp, err := t.web.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()

	// Limit body read to MaxPageSize.
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(t.web.config.MaxPageSize)))
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateStr(string(body), 500))
	}

	contentType := resp.Header.Get("Content-Type")
	var content string

	switch {
	case strings.Contains(contentType, "text/markdown"):
		// Cloudflare returned markdown directly — use as-is.
		content = string(body)

	case strings.Contains(contentType, "text/html"):
		// Convert HTML to readable text.
		if extractMain {
			content = htmlToReadableText(body)
		} else {
			content = htmlToPlainText(body)
		}

	case strings.Contains(contentType, "application/json"):
		// Pretty-print JSON.
		var v any
		if err := json.Unmarshal(body, &v); err == nil {
			pretty, err := json.MarshalIndent(v, "", "  ")
			if err == nil {
				content = string(pretty)
			} else {
				content = string(body)
			}
		} else {
			content = string(body)
		}

	default:
		// Return as-is for other content types.
		content = string(body)
	}

	// Truncate if needed.
	if len(content) > args.MaxLength {
		content = content[:args.MaxLength] + "\n\n[... truncated]"
	}

	// Build result with metadata header.
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("URL: %s\n", args.URL))
	sb.WriteString(fmt.Sprintf("Status: %d\n", resp.StatusCode))
	sb.WriteString(fmt.Sprintf("Content-Type: %s\n", contentType))
	if tokens := resp.Header.Get("X-Markdown-Tokens"); tokens != "" {
		sb.WriteString(fmt.Sprintf("Markdown-Tokens: %s\n", tokens))
	}
	sb.WriteString(strings.Repeat("─", 40))
	sb.WriteByte('\n')
	sb.WriteString(content)

	return sb.String(), nil
}

// truncateStr is like truncate (in edit.go) but avoids redeclaration in the same package.
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
