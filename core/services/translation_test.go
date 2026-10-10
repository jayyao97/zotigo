package services

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
)

func TestTranslateTextIsolatedRequest(t *testing.T) {
	p := &titleProvider{events: []protocol.Event{protocol.NewReasoningDeltaEvent("hidden"), protocol.NewTextDeltaEvent("你好\n世界")}}
	got, err := TranslateText(context.Background(), p, "Hello\nworld", "zh-CN")
	if err != nil || got != "你好\n世界" {
		t.Fatalf("translation = %q, %v", got, err)
	}
	if len(p.messages) != 2 || len(p.tools) != 0 || messageText(p.messages[1]) != "Hello\nworld" || !strings.Contains(messageText(p.messages[0]), "untrusted") {
		t.Fatalf("unexpected request: %#v", p.messages)
	}
}

func TestTranslateTextRejectsInvalidAndFailedResponses(t *testing.T) {
	for _, text := range []string{"", strings.Repeat("文", 8001)} {
		if _, err := TranslateText(context.Background(), &titleProvider{}, text, "en"); err == nil {
			t.Fatal("accepted invalid text")
		}
	}
	if _, err := TranslateText(context.Background(), &titleProvider{}, "hello", "invalid"); err == nil {
		t.Fatal("accepted invalid language")
	}
	for _, provider := range []*titleProvider{{}, {err: errors.New("provider failed")}, {events: []protocol.Event{protocol.NewTextDeltaEvent(strings.Repeat("x", 128*1024+1))}}} {
		if _, err := TranslateText(context.Background(), provider, "hello", "en"); err == nil {
			t.Fatal("accepted failed response")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := TranslateText(ctx, &titleProvider{events: []protocol.Event{protocol.NewTextDeltaEvent("hi")}}, "hello", "en"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}
