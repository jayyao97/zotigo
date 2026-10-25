package providers_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	_ "github.com/jayyao97/zotigo/core/providers/deepseek"
)

func TestRetryAcrossNativeProviders(t *testing.T) {
	for _, name := range []string{"openai", "openai-response", "openai-response-failed", "anthropic", "deepseek", "gemini"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", name, streaming), func(t *testing.T) {
				t.Parallel()
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if calls.Add(1) == 1 {
						if !streaming {
							w.WriteHeader(503)
							_, _ = fmt.Fprint(w, `{"error":{"message":"busy","code":503}}`)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						switch name {
						case "anthropic", "deepseek":
							_, _ = fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n")
						case "gemini":
							_, _ = fmt.Fprint(w, "event: error\ndata: {\"error\":{\"code\":503,\"message\":\"busy\"}}\n\n")
						case "openai-response":
							_, _ = fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"busy\"}\n\n")
						case "openai-response-failed":
							_, _ = fmt.Fprint(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"busy\"}}}\n\n")
						default:
							_, _ = fmt.Fprint(w, "data: {\"error\":{\"code\":\"server_error\",\"message\":\"busy\"}}\n\n")
						}
						return
					}
					finishSSE(w, name)
				}))
				defer server.Close()
				cfg := config.ProfileConfig{Provider: name, Model: "test", APIKey: "test", BaseURL: server.URL}
				if name == "openai-response" || name == "openai-response-failed" {
					cfg.Provider = "openai"
					cfg.Params = map[string]any{"mode": "response"}
				}
				p, err := providers.NewProvider(cfg)
				if err != nil {
					t.Fatal(err)
				}
				stream, err := p.StreamChat(context.Background(), []protocol.Message{protocol.NewUserMessage("hello")}, nil)
				if err != nil {
					t.Fatal(err)
				}
				retries := 0
				var statuses []string
				requestID := ""
				for event := range stream {
					if event.Retry != nil {
						statuses = append(statuses, event.Retry.Status)
						if requestID == "" {
							requestID = event.Retry.RequestID
						}
						if requestID == "" || requestID != event.Retry.RequestID {
							t.Error("retry lifecycle lost request identity")
						}
					}
					if event.Error != nil {
						t.Errorf("stream: %v", event.Error)
					}
					if event.Retry != nil && event.Retry.Status == "waiting" {
						retries++
					}
				}
				if calls.Load() != 2 || retries != 1 {
					t.Fatalf("requests=%d retries=%d", calls.Load(), retries)
				}
				if fmt.Sprint(statuses) != "[waiting retrying recovered]" {
					t.Fatalf("statuses=%v", statuses)
				}
			})
		}
	}
}
