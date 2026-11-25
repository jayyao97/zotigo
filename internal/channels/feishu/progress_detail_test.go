package feishu

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/internal/channels"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func cotEventTypes(batches ...[]*larkim.MessageCot) []string {
	var types []string
	for _, batch := range batches {
		for _, event := range batch {
			types = append(types, *event.EventType)
		}
	}
	return types
}

func TestCOTRendersDetailedProgressAsTitlesAndReasoningNodes(t *testing.T) {
	api := &fakeCOTAPI{}
	handle := &cotProgressHandle{api: api, receipt: channels.DeliveryReceipt{COTID: "cot-1"}, originID: "om-1", runID: "run-cot-1", finalReply: func(context.Context, string, channels.TaskResult, string, bool) (string, error) {
		return "om-final", nil
	}}
	err := handle.Update(context.Background(), channels.Progress{Detail: channels.ProgressDetailFull, Events: []channels.PublicExecutionEvent{
		{Type: channels.ExecutionRunStarted},
		{ID: "think-1", Type: channels.ExecutionThinking, Text: "plan A"},
		{ID: "say-1", Type: channels.ExecutionNarration, Text: "Let me check."},
		{Type: channels.ExecutionToolStarted, Tool: &channels.PublicToolActivity{CallID: "call-1", Kind: "shell", DisplayName: "Run command", Subject: "go test ./..."}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"REASONING_START", "RUN_STARTED", "REASONING_MESSAGE_START", "REASONING_MESSAGE_CONTENT", "REASONING_MESSAGE_END", "REASONING_MESSAGE_START", "REASONING_MESSAGE_CONTENT", "REASONING_MESSAGE_END", "TOOL_CALL_START", "TOOL_CALL_END"}
	if got := cotEventTypes(api.appended...); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events=%v", got)
	}
	encoded, _ := json.Marshal(api.appended)
	for _, text := range []string{"plan A", "Let me check.", "Run command: go test ./..."} {
		if !strings.Contains(string(encoded), text) {
			t.Fatalf("missing %q in %s", text, encoded)
		}
	}
	// A later update does not reopen the section, and completion closes it.
	if err := handle.Update(context.Background(), channels.Progress{Detail: channels.ProgressDetailFull, Events: []channels.PublicExecutionEvent{{ID: "say-2", Type: channels.ExecutionNarration, Text: "Next."}}}); err != nil {
		t.Fatal(err)
	}
	if got := cotEventTypes(api.appended[1]); got[0] != "REASONING_MESSAGE_START" {
		t.Fatalf("second update=%v", got)
	}
	if err := handle.Complete(context.Background(), channels.TaskResult{Text: "done"}, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := cotEventTypes(api.appended[len(api.appended)-1]); strings.Join(got, ",") != "REASONING_END,RUN_FINISHED" {
		t.Fatalf("completion=%v", got)
	}
}

func TestCOTBriefProgressNeverOpensReasoning(t *testing.T) {
	api := &fakeCOTAPI{}
	handle := &cotProgressHandle{api: api, receipt: channels.DeliveryReceipt{COTID: "cot-1"}, originID: "om-1", runID: "run-cot-1", finalReply: func(context.Context, string, channels.TaskResult, string, bool) (string, error) {
		return "om-final", nil
	}}
	if err := handle.Update(context.Background(), channels.Progress{Events: []channels.PublicExecutionEvent{{Type: channels.ExecutionRunStarted}, {Type: channels.ExecutionToolStarted, Tool: &channels.PublicToolActivity{CallID: "call-1", Kind: "shell", DisplayName: "Run command"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := handle.Fail(context.Background(), channels.Progress{State: "failed"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cotEventTypes(api.appended...), ","); strings.Contains(got, "REASONING") || !strings.Contains(got, "TOOL_CALL_START") {
		t.Fatalf("events=%s", got)
	}
}

func TestCOTReasoningTextFitsEventLimit(t *testing.T) {
	events, err := reasoningMessageEvents("message-1", strings.Repeat("思考<>&", 2000), time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(*events[1].Content) > 4096 || !strings.Contains(*events[1].Content, "…") {
		t.Fatalf("content bytes=%d", len(*events[1].Content))
	}
}

func TestCardShowsStepsOnlyAtDetailedLevels(t *testing.T) {
	var cards []string
	handle := &progressHandle{messageID: "om-card", patch: func(_ context.Context, _ string, card string) error {
		cards = append(cards, card)
		return nil
	}}
	tool := &channels.PublicToolActivity{CallID: "call-1", Kind: "shell", DisplayName: "Run command", Subject: "go test ./..."}
	if err := handle.Update(context.Background(), channels.Progress{State: "running", Text: "Zotigo is working…", Events: []channels.PublicExecutionEvent{{Type: channels.ExecutionToolStarted, Tool: tool}}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cards[0], "Run command") {
		t.Fatalf("brief card listed steps: %s", cards[0])
	}
	for _, progress := range []channels.Progress{
		{State: "running", Text: "Zotigo is working…", Detail: channels.ProgressDetailFull, Events: []channels.PublicExecutionEvent{{Type: channels.ExecutionThinking, Text: "plan A"}, {Type: channels.ExecutionToolStarted, Tool: tool}}},
		{State: "running", Text: "Zotigo is working…", Detail: channels.ProgressDetailFull, Events: []channels.PublicExecutionEvent{{Type: channels.ExecutionToolFinished, Tool: &channels.PublicToolActivity{CallID: "call-1", Status: "completed"}}}},
	} {
		if err := handle.Update(context.Background(), progress); err != nil {
			t.Fatal(err)
		}
	}
	last := cards[len(cards)-1]
	for _, text := range []string{"Zotigo is working…", "*Thinking:* plan A", "Run command: `go test ./...` ✓"} {
		if !strings.Contains(last, text) {
			t.Fatalf("card is missing %q: %s", text, last)
		}
	}
}

func TestCardStepsKeepTheLatest(t *testing.T) {
	var steps cardSteps
	for index := 0; index < maxCardSteps+5; index++ {
		steps.add([]channels.PublicExecutionEvent{{Type: channels.ExecutionToolStarted, Tool: &channels.PublicToolActivity{CallID: string(rune('a' + index)), DisplayName: "Step"}}})
	}
	steps.add([]channels.PublicExecutionEvent{{Type: channels.ExecutionToolFinished, Tool: &channels.PublicToolActivity{CallID: string(rune('a' + maxCardSteps + 4)), Status: "failed"}}})
	if len(steps.lines) != maxCardSteps || !strings.HasSuffix(steps.markdown(), "- Step ✗") {
		t.Fatalf("steps=%d %q", len(steps.lines), steps.markdown())
	}
	if _, ok := steps.byCall["a"]; ok {
		t.Fatal("a dropped step kept its index")
	}
}

func TestCommandReplyIsPlainTextOutsideThreads(t *testing.T) {
	var body *larkim.ReplyMessageReqBody
	adapter := &Adapter{reply: func(_ context.Context, messageID string, value *larkim.ReplyMessageReqBody) (string, error) {
		if messageID != "om_dc13264520392913993dd051dba21dcf" {
			t.Fatalf("replied to %q", messageID)
		}
		body = value
		return "om-reply", nil
	}}
	if err := adapter.ReplyCommand(context.Background(), channels.InboundMessage{MessageID: "om_dc13264520392913993dd051dba21dcf"}, "This group is not bound."); err != nil {
		t.Fatal(err)
	}
	if *body.MsgType != "text" || *body.ReplyInThread || *body.Content != `{"text":"This group is not bound."}` {
		t.Fatalf("body=%+v", body)
	}
	// Feishu limits the idempotency key to 50 characters.
	if len(*body.Uuid) > 50 {
		t.Fatalf("uuid %q is too long", *body.Uuid)
	}
}
