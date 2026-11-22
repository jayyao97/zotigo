package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
)

func TestOpenAIUsageSeparatesCacheWrites(t *testing.T) {
	for _, tc := range []struct {
		name, details string
		want          protocol.Usage
	}{
		{"official read", `{"cached_tokens":80}`, protocol.Usage{InputTokens: 20, OutputTokens: 5, TotalTokens: 105, CacheReadInputTokens: 80}},
		{"relay write", `{"cache_write_tokens":97,"cached_tokens":0}`, protocol.Usage{InputTokens: 3, OutputTokens: 5, TotalTokens: 105, CacheCreationInputTokens: 97}},
		{"alternate spelling", `{"cached_creation_tokens":97,"cached_tokens":0}`, protocol.Usage{InputTokens: 3, OutputTokens: 5, TotalTokens: 105, CacheCreationInputTokens: 97}},
		{"no details", "", protocol.Usage{InputTokens: 100, OutputTokens: 5, TotalTokens: 105}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cached int64
			if tc.want.CacheReadInputTokens > 0 {
				cached = int64(tc.want.CacheReadInputTokens)
			}
			got := *openAIUsage(100, cached, 5, 105, tc.details)
			if got != tc.want {
				t.Fatalf("usage = %+v, want %+v", got, tc.want)
			}
			if got.TotalInput() != 100 {
				t.Fatalf("total input = %d, want the full prompt", got.TotalInput())
			}
		})
	}
}

// Captured from an OpenAI-compatible relay in front of a cache-writing model.
func TestStreamedUsageReadsRelayCacheWrites(t *testing.T) {
	for _, tc := range []struct{ mode, body string }{
		{"response", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":8630,\"input_tokens_details\":{\"cache_write_tokens\":8627,\"cached_tokens\":0},\"output_tokens\":5,\"total_tokens\":8635}}}\n\n"},
		{"chat", "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":8630,\"completion_tokens\":5,\"total_tokens\":8635,\"prompt_tokens_details\":{\"cache_write_tokens\":8627,\"cached_creation_tokens\":8627,\"cached_tokens\":0}}}\n\ndata: [DONE]\n\n"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			p, err := New(config.ProfileConfig{Provider: "openai", Model: "example", APIKey: "test", BaseURL: srv.URL, Params: map[string]any{"mode": tc.mode}})
			if err != nil {
				t.Fatal(err)
			}
			events, err := p.StreamChat(context.Background(), []protocol.Message{protocol.NewUserMessage("hi")}, nil)
			if err != nil {
				t.Fatal(err)
			}
			var usage *protocol.Usage
			for evt := range events {
				if evt.Usage != nil {
					usage = evt.Usage
				}
			}
			want := protocol.Usage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8635, CacheCreationInputTokens: 8627}
			if usage == nil || *usage != want {
				t.Fatalf("usage = %+v, want %+v", usage, want)
			}
		})
	}
}
