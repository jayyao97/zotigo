package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// TavilySearchProvider implements SearchProvider using the Tavily Search API.
type TavilySearchProvider struct {
	apiKey  string
	baseURL string
	client  *http.Client
	ua      string
}

type tavilyRequest struct {
	APIKey        string `json:"api_key"`
	Query         string `json:"query"`
	MaxResults    int    `json:"max_results,omitempty"`
	SearchDepth   string `json:"search_depth,omitempty"`
	Topic         string `json:"topic,omitempty"`
	TimeRange     string `json:"time_range,omitempty"`
	IncludeAnswer bool   `json:"include_answer"`
}

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

func (p *TavilySearchProvider) Search(ctx context.Context, query string, maxResults int, opts SearchOptions) ([]searchResult, error) {
	body, err := json.Marshal(tavilyRequest{
		APIKey:        p.apiKey,
		Query:         query,
		MaxResults:    maxResults,
		SearchDepth:   "advanced",
		Topic:         opts.Topic,
		TimeRange:     opts.TimeRange,
		IncludeAnswer: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := p.baseURL + "/search"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if p.ua != "" {
		req.Header.Set("User-Agent", p.ua)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
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

	results := make([]searchResult, len(tavilyResp.Results))
	for i, r := range tavilyResp.Results {
		results[i] = searchResult{Title: r.Title, URL: r.URL, Content: r.Content}
	}

	// Append Tavily's AI summary as an extra result if present
	if tavilyResp.Answer != "" {
		results = append(results, searchResult{
			Title:   "AI Summary",
			URL:     "",
			Content: tavilyResp.Answer,
		})
	}

	return results, nil
}
