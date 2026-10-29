package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

// DeepSeekSearchProvider uses the standalone Search API, independently of the LLM profile.
type DeepSeekSearchProvider struct {
	apiKey  string
	baseURL string
	client  *http.Client
	ua      string
}

func (p *DeepSeekSearchProvider) Description() string {
	return "Search the web for up-to-date information. Returns titles, URLs, and summaries. Query supports site:, -site:, lang:, after:YYYY-MM-DD and before:YYYY-MM-DD."
}

func (p *DeepSeekSearchProvider) Schema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query":       map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "description": "Search query, at most 128 Unicode characters including operators. Use site: for domains and after:/before: for dates."},
			"max_results": map[string]any{"type": "integer", "minimum": 1, "maximum": 20, "description": "Maximum results (1-20, default: 5)"},
		},
		"required": []string{"query"},
	}
}

func (p *DeepSeekSearchProvider) Search(ctx context.Context, query string, maxResults int, opts SearchOptions) ([]searchResult, error) {
	if strings.TrimSpace(query) == "" || !utf8.ValidString(query) || utf8.RuneCountInString(query) > 128 {
		return nil, fmt.Errorf("deepseek search query must contain 1-128 Unicode characters and not be all whitespace")
	}
	if (opts.Topic != "" && opts.Topic != "general") || opts.TimeRange != "" {
		return nil, fmt.Errorf("deepseek search does not support topic or time_range; use after:YYYY-MM-DD and before:YYYY-MM-DD in query")
	}
	if maxResults < 1 || maxResults > 50 {
		return nil, fmt.Errorf("deepseek search count must be between 1 and 50")
	}
	body, err := json.Marshal(struct {
		Query       string `json:"query"`
		Count       int    `json:"count"`
		NeedContent bool   `json:"need_content"`
	}{Query: query, Count: maxResults})
	if err != nil {
		return nil, fmt.Errorf("encode deepseek search: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.baseURL, "/")+"/v1/tools/search", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create deepseek search request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", p.ua)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("deepseek search request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	const maxResponse = 1024 * 1024
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return nil, fmt.Errorf("read deepseek search response: %w", err)
	}
	if len(data) > maxResponse {
		return nil, fmt.Errorf("deepseek search response exceeds 1 MiB")
	}
	if resp.StatusCode != http.StatusOK {
		// Do not echo arbitrary response bodies: proxies may include request credentials.
		return nil, fmt.Errorf("deepseek search API error (HTTP %d)", resp.StatusCode)
	}
	var payload struct {
		Results *[]struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Summary string `json:"summary"`
			Snippet string `json:"snippet"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode deepseek search response: %w", err)
	}
	if payload.Results == nil {
		return nil, fmt.Errorf("deepseek search response missing results array")
	}
	results := make([]searchResult, 0, len(*payload.Results))
	for _, result := range *payload.Results {
		content := result.Summary
		if strings.TrimSpace(content) == "" {
			content = result.Snippet
		}
		results = append(results, searchResult{Title: result.Title, URL: result.URL, Content: content})
	}
	return results, nil
}
