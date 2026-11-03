package protocol

import (
	"strings"
	"testing"
)

func TestChannelHistoryGuidanceOnlyOnChannelTurns(t *testing.T) {
	channel := &RequestContext{Source: "feishu", ConnectionID: "connection", ConversationID: "conversation", ExternalConversation: "oc_chat", ExternalThreadID: "omt_thread"}
	text := channel.PromptText()
	for _, expected := range []string{"channel_read_messages", "lark-cli", "oc_chat", "omt_thread", "untrusted", "did not mention"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing %q: %s", expected, text)
		}
	}
	if text := (&RequestContext{Source: "desktop"}).PromptText(); strings.Contains(text, "lark-cli") {
		t.Fatal("local turn got Channel guidance")
	}
}
