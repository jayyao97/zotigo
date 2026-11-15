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

// The Responses API announces every output item with output_item.added:
// reasoning, message and function_call alike. Only function_call items are
// tool calls. Callers that must not receive tool calls, such as the
// compaction summarizer, reject a stream that reports one.
func TestResponseOutputItemAddedReportsOnlyFunctionCalls(t *testing.T) {
	added := func(index int, item string) string {
		return fmt.Sprintf("data: {\"type\":\"response.output_item.added\",\"output_index\":%d,\"item\":%s}\n\n", index, item)
	}
	const completed = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"
	for _, tc := range []struct {
		name, body    string
		wantToolCalls []string
	}{
		{
			name: "reasoning and message items are not tool calls",
			body: added(0, `{"id":"rs_1","type":"reasoning","summary":[]}`) +
				added(1, `{"id":"msg_1","type":"message","role":"assistant","status":"in_progress","content":[]}`) +
				"data: {\"type\":\"response.output_text.delta\",\"output_index\":1,\"delta\":\"hi\"}\n\n" + completed,
		},
		{
			name: "a function_call item is a tool call",
			body: added(0, `{"id":"rs_1","type":"reasoning","summary":[]}`) +
				added(1, `{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read_file","arguments":""}`) +
				"data: {\"type\":\"response.function_call_arguments.done\",\"output_index\":1,\"item_id\":\"fc_1\",\"arguments\":\"{}\"}\n\n" + completed,
			wantToolCalls: []string{"delta:call_1:read_file", "end:call_1:read_file"},
		},
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
			var got []string
			for evt := range events {
				switch evt.Type {
				case protocol.EventTypeError:
					t.Fatalf("stream error: %v", evt.Error)
				case protocol.EventTypeToolCallDelta:
					got = append(got, "delta:"+evt.ToolCallDelta.ID+":"+evt.ToolCallDelta.Name)
				case protocol.EventTypeToolCallEnd:
					got = append(got, "end:"+evt.ToolCall.ID+":"+evt.ToolCall.Name)
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.wantToolCalls) {
				t.Fatalf("tool call events = %v, want %v", got, tc.wantToolCalls)
			}
		})
	}
}
