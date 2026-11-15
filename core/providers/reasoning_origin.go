package providers

import (
	"context"
	"net/url"
	"strings"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/tools"
)

// Model services whose reasoning cannot be replayed to one another.
const (
	OriginAnthropic = "anthropic"
	OriginOpenAI    = "openai"
	OriginGemini    = "gemini"
	OriginDeepSeek  = "deepseek"
)

// deepSeekPlaceholderReasoning stands in for reasoning that DeepSeek requires
// on a tool-calling turn but another service never produced in readable form.
const deepSeekPlaceholderReasoning = "(reasoning from an earlier model is not available)"

// ReasoningOrigin names the model service a profile talks to. Reasoning parts
// are stamped with it when recorded, and a part is replayed only to the
// service that produced it: thinking signatures and encrypted reasoning are
// meaningless to any other service, and Claude rejects a thinking block whose
// signature it did not issue.
//
// Models of one service share an origin. Their reasoning is sent as recorded
// and the service itself decides what the current model may read.
func ReasoningOrigin(cfg config.ProfileConfig) string {
	if cfg.Provider == OriginDeepSeek {
		return OriginDeepSeek
	}
	// DeepSeek is also reached through the Anthropic driver by pointing a
	// profile at its Anthropic-compatible endpoint.
	if parsed, err := url.Parse(cfg.BaseURL); err == nil && strings.Contains(strings.ToLower(parsed.Hostname()), "deepseek") {
		return OriginDeepSeek
	}
	return cfg.Provider
}

type ownReasoningProvider struct {
	Provider
	origin string
}

// WithOwnReasoningOnly makes a provider see only reasoning it can replay. The
// stored history is not changed; the filtering applies to each request.
func WithOwnReasoningOnly(factory FactoryFunc) FactoryFunc {
	return func(cfg config.ProfileConfig) (Provider, error) {
		provider, err := factory(cfg)
		if err != nil {
			return nil, err
		}
		return &ownReasoningProvider{Provider: provider, origin: ReasoningOrigin(cfg)}, nil
	}
}

func (p *ownReasoningProvider) StreamChat(ctx context.Context, messages []protocol.Message, toolList []tools.Tool, opts ...StreamChatOption) (<-chan protocol.Event, error) {
	return p.Provider.StreamChat(ctx, projectReasoning(messages, p.origin), toolList, opts...)
}

// projectReasoning returns messages as the service named target may receive
// them. It never modifies the input.
//
//   - Reasoning from another service is removed. It is not turned into answer
//     text: a model that reads earlier reasoning as something it said starts
//     writing its reasoning into its answers.
//   - Claude also loses reasoning that carries no signature (an interrupted
//     stream), which it would reject.
//   - DeepSeek requires reasoning on every tool-calling turn, and accepts
//     reasoning it did not sign. Foreign reasoning is kept for it as plain
//     unsigned reasoning, and a tool-calling turn left without any gets a
//     placeholder.
//
// Reasoning recorded before origins were tracked is left to the provider's own
// conversion, as before. An assistant message left with no content is dropped.
func projectReasoning(messages []protocol.Message, target string) []protocol.Message {
	var out []protocol.Message
	for index, message := range messages {
		projected, changed := projectAssistantReasoning(message, target)
		if !changed {
			if out != nil {
				out = append(out, message)
			}
			continue
		}
		if out == nil {
			out = append(make([]protocol.Message, 0, len(messages)), messages[:index]...)
		}
		if len(projected.Content) > 0 {
			out = append(out, projected)
		}
	}
	if out == nil {
		return messages
	}
	return out
}

func projectAssistantReasoning(message protocol.Message, target string) (protocol.Message, bool) {
	if message.Role != protocol.RoleAssistant {
		return message, false
	}
	content := make([]protocol.ContentPart, 0, len(message.Content)+1)
	changed, hasReasoning, hasToolCall := false, false, false
	for _, part := range message.Content {
		if part.Type == protocol.ContentTypeToolCall {
			hasToolCall = true
		}
		if part.Type != protocol.ContentTypeReasoning {
			content = append(content, part)
			continue
		}
		foreign := part.Origin != "" && part.Origin != target
		switch {
		case foreign && target == OriginDeepSeek:
			if strings.TrimSpace(part.Text) == "" {
				changed = true
				continue
			}
			part = protocol.ContentPart{Type: protocol.ContentTypeReasoning, Text: part.Text, Origin: part.Origin}
			changed = true
		case foreign, target == OriginAnthropic && part.Signature == "":
			changed = true
			continue
		}
		hasReasoning = true
		content = append(content, part)
	}
	if target == OriginDeepSeek && hasToolCall && !hasReasoning {
		content = append([]protocol.ContentPart{{Type: protocol.ContentTypeReasoning, Text: deepSeekPlaceholderReasoning}}, content...)
		changed = true
	}
	if !changed {
		return message, false
	}
	message.Content = content
	return message, true
}
