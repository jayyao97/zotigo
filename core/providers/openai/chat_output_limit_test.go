package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
)

func TestChatOutputLimitOnWire(t *testing.T) {
	for _, limit := range []int64{0, 8192} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			bodies := make(chan map[string]any, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				bodies <- body
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"id\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			cfg := config.ProfileConfig{Provider: "openai", Model: "gpt-5", APIKey: "test", BaseURL: server.URL, Params: map[string]any{"mode": "chat"}}
			want := int64(120000)
			if limit > 0 {
				cfg.MaxOutputTokens = &limit
				want = limit
			}
			provider, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			events, err := provider.StreamChat(context.Background(), []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.ContentPart{{Type: protocol.ContentTypeText, Text: "hi"}}}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			for event := range events {
				if event.Type == protocol.EventTypeError {
					t.Fatalf("stream error: %+v", event)
				}
			}
			body := <-bodies
			if body["max_completion_tokens"] != float64(want) {
				t.Fatalf("output limit = %v, want %d", body["max_completion_tokens"], want)
			}
			if _, ok := body["max_tokens"]; ok {
				t.Fatal("must not send competing legacy cap")
			}
		})
	}
}
