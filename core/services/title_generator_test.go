package services

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

type titleProvider struct {
	messages []protocol.Message
	tools    []tools.Tool
	options  providers.ResolvedOptions
	events   []protocol.Event
	err      error
}

func (p *titleProvider) StreamChat(_ context.Context, messages []protocol.Message, toolList []tools.Tool, opts ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	p.messages = messages
	p.tools = toolList
	p.options = providers.ResolveOptions(opts)
	if p.err != nil {
		return nil, p.err
	}
	stream := make(chan protocol.Event, len(p.events))
	for _, event := range p.events {
		stream <- event
	}
	close(stream)
	return stream, nil
}

func (*titleProvider) Name() string { return "title-test" }

func TestGenerateTitleBuildsIsolatedRequestAndNormalizesText(t *testing.T) {
	provider := &titleProvider{events: []protocol.Event{
		protocol.NewReasoningDeltaEvent("private reasoning"),
		protocol.NewTextDeltaEvent("## \"修复  DeepSeek\nSafety 配置\""),
		protocol.NewFinishEvent(protocol.FinishReasonStop),
	}}

	title, err := GenerateTitle(context.Background(), provider, "  帮我修一下  ", "  已定位配置问题  ")
	if err != nil {
		t.Fatalf("GenerateTitle: %v", err)
	}
	if title != "修复 DeepSeek Safety 配置" {
		t.Fatalf("title = %q", title)
	}
	if len(provider.messages) != 2 || provider.messages[0].Role != protocol.RoleSystem || provider.messages[1].Role != protocol.RoleUser {
		t.Fatalf("messages = %#v", provider.messages)
	}
	if !strings.Contains(messageText(provider.messages[0]), "untrusted") {
		t.Fatalf("system instruction does not mark input untrusted: %q", messageText(provider.messages[0]))
	}
	if got := messageText(provider.messages[1]); !strings.Contains(got, "帮我修一下") || !strings.Contains(got, "已定位配置问题") {
		t.Fatalf("request omitted source content: %q", got)
	}
	if len(provider.tools) != 0 {
		t.Fatalf("tools = %#v, want none", provider.tools)
	}
	if provider.options.ReasoningEffort != "low" {
		t.Fatalf("reasoning effort = %q", provider.options.ReasoningEffort)
	}
}

func TestGenerateTitleBoundsInputAndUnicodeOutput(t *testing.T) {
	provider := &titleProvider{events: []protocol.Event{protocol.NewTextDeltaEvent(strings.Repeat("标", titleOutputRuneLimit+10))}}
	userInput := strings.Repeat("用", titleInputRuneLimit+10)

	title, err := GenerateTitle(context.Background(), provider, userInput, "")
	if err != nil {
		t.Fatalf("GenerateTitle: %v", err)
	}
	if got := len([]rune(title)); got != titleOutputRuneLimit {
		t.Fatalf("title rune count = %d, want %d", got, titleOutputRuneLimit)
	}
	if got := messageText(provider.messages[1]); strings.Contains(got, strings.Repeat("用", titleInputRuneLimit+1)) {
		t.Fatal("user input was not truncated")
	}
}

func messageText(message protocol.Message) string {
	var text strings.Builder
	for _, part := range message.Content {
		if part.Type == protocol.ContentTypeText {
			text.WriteString(part.Text)
		}
	}
	return text.String()
}

func TestGenerateTitleRejectsEmptyInput(t *testing.T) {
	_, err := GenerateTitle(context.Background(), &titleProvider{}, " \n", "")
	if err == nil || !strings.Contains(err.Error(), "input is empty") {
		t.Fatalf("err = %v", err)
	}
}

func TestGenerateTitleReturnsProviderAndStreamErrors(t *testing.T) {
	t.Run("request", func(t *testing.T) {
		providerErr := errors.New("request failed")
		_, err := GenerateTitle(context.Background(), &titleProvider{err: providerErr}, "user", "assistant")
		if !errors.Is(err, providerErr) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("stream", func(t *testing.T) {
		streamErr := errors.New("stream failed")
		provider := &titleProvider{events: []protocol.Event{protocol.NewErrorEvent(streamErr)}}
		_, err := GenerateTitle(context.Background(), provider, "user", "assistant")
		if !errors.Is(err, streamErr) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestGenerateTitleReturnsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := GenerateTitle(ctx, &titleProvider{}, "user", "assistant")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestGenerateTitleRejectsEmptyResponse(t *testing.T) {
	provider := &titleProvider{events: []protocol.Event{protocol.NewReasoningDeltaEvent("reasoning only")}}
	_, err := GenerateTitle(context.Background(), provider, "user", "assistant")
	if err == nil || !strings.Contains(err.Error(), "response is empty") {
		t.Fatalf("err = %v", err)
	}
}
