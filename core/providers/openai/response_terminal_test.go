package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
)

func TestResponseTerminalStopsReadingTrailer(t *testing.T) {
	const delta = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"hi\"}\n\n"
	const completed = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"total_tokens\":12}}}\n\n"
	for _, tc := range []struct {
		name, body string
		wantError  bool
	}{
		{"empty trailer", delta + completed + "\n\n", false},
		{"malformed trailer", delta + completed + "data: {\n\n", false},
		{"malformed before terminal", delta + "data: {\n\n" + completed, true},
		{"missing terminal", delta, true},
		{"provider failed", delta + "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"failed\"}}}\n\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			p, err := New(config.ProfileConfig{Provider: "openai", Model: "example", APIKey: "test", BaseURL: srv.URL, Params: map[string]any{"mode": "response"}})
			if err != nil {
				t.Fatal(err)
			}
			events, err := p.StreamChat(context.Background(), []protocol.Message{protocol.NewUserMessage("hi")}, nil)
			if err != nil {
				t.Fatal(err)
			}
			sawError, finished := false, false
			var text strings.Builder
			for evt := range events {
				if evt.Type == protocol.EventTypeError {
					sawError = true
				}
				if evt.Type == protocol.EventTypeContentDelta {
					text.WriteString(evt.ContentPartDelta.Text)
				}
				if evt.Type == protocol.EventTypeFinish {
					finished = true
					if evt.Usage == nil || evt.Usage.TotalInput() != 10 || evt.Usage.OutputTokens != 2 {
						t.Fatalf("lost usage %+v", evt.Usage)
					}
				}
			}
			if sawError != tc.wantError || finished == tc.wantError || text.String() != "hi" {
				t.Fatalf("error=%v finish=%v text=%q", sawError, finished, text.String())
			}
		})
	}
}
