package providers

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
)

func TestReasoningOrigin(t *testing.T) {
	for _, tc := range []struct {
		cfg  config.ProfileConfig
		want string
	}{
		{config.ProfileConfig{Provider: "anthropic", BaseURL: "https://relay.example/"}, OriginAnthropic},
		{config.ProfileConfig{Provider: "anthropic"}, OriginAnthropic},
		{config.ProfileConfig{Provider: "anthropic", BaseURL: "https://api.deepseek.com/anthropic"}, OriginDeepSeek},
		{config.ProfileConfig{Provider: "deepseek"}, OriginDeepSeek},
		{config.ProfileConfig{Provider: "openai", BaseURL: "https://relay.example/v1"}, OriginOpenAI},
		{config.ProfileConfig{Provider: "gemini"}, OriginGemini},
	} {
		if got := ReasoningOrigin(tc.cfg); got != tc.want {
			t.Errorf("ReasoningOrigin(%s, %q) = %q, want %q", tc.cfg.Provider, tc.cfg.BaseURL, got, tc.want)
		}
	}
}

// shape renders an assistant message's parts so a test can compare what a
// provider would be handed.
func shape(messages []protocol.Message) string {
	var out []string
	for _, message := range messages {
		var parts []string
		for _, part := range message.Content {
			switch part.Type {
			case protocol.ContentTypeReasoning:
				parts = append(parts, fmt.Sprintf("reasoning(%q sig=%q enc=%q)", part.Text, part.Signature, part.EncryptedContent))
			case protocol.ContentTypeToolCall:
				parts = append(parts, "tool_call")
			case protocol.ContentTypeToolResult:
				parts = append(parts, "tool_result")
			default:
				parts = append(parts, "text")
			}
		}
		out = append(out, string(message.Role)+"["+strings.Join(parts, " ")+"]")
	}
	return strings.Join(out, " ")
}

func TestProjectReasoning(t *testing.T) {
	reasoning := func(origin, text, signature, encrypted string) protocol.ContentPart {
		return protocol.ContentPart{Type: protocol.ContentTypeReasoning, Origin: origin, Text: text, Signature: signature, EncryptedContent: encrypted, ReasoningID: map[bool]string{true: "rs_1"}[encrypted != ""]}
	}
	text := protocol.ContentPart{Type: protocol.ContentTypeText, Text: "answer"}
	call := protocol.ContentPart{Type: protocol.ContentTypeToolCall, ToolCall: &protocol.ToolCall{ID: "call_1", Name: "shell"}}
	assistant := func(parts ...protocol.ContentPart) []protocol.Message {
		return []protocol.Message{protocol.NewUserMessage("hi"), {Role: protocol.RoleAssistant, Content: parts}}
	}
	claude := reasoning(OriginAnthropic, "claude thought", "sig-claude", "")
	gpt := reasoning(OriginOpenAI, "gpt summary", "", "blob")
	gemini := reasoning(OriginGemini, "gemini thought", "", "")
	deepseek := reasoning(OriginDeepSeek, "deepseek thought", "sig-ds", "")
	legacySigned := reasoning("", "old thought", "sig-old", "")
	legacyUnsigned := reasoning("", "old thought", "", "")

	for _, tc := range []struct {
		name   string
		target string
		input  []protocol.Message
		want   string
	}{
		{"claude keeps its own reasoning, signature intact", OriginAnthropic, assistant(claude, call),
			`user[text] assistant[reasoning("claude thought" sig="sig-claude" enc="") tool_call]`},
		{"claude does not get gpt reasoning", OriginAnthropic, assistant(gpt, call), `user[text] assistant[tool_call]`},
		{"claude does not get gemini reasoning", OriginAnthropic, assistant(gemini, text), `user[text] assistant[text]`},
		{"claude does not get deepseek reasoning, signed or not", OriginAnthropic, assistant(deepseek, text), `user[text] assistant[text]`},
		{"claude does not get unsigned reasoning of unknown origin", OriginAnthropic, assistant(legacyUnsigned, text), `user[text] assistant[text]`},
		{"claude keeps signed reasoning of unknown origin", OriginAnthropic, assistant(legacySigned, text),
			`user[text] assistant[reasoning("old thought" sig="sig-old" enc="") text]`},
		{"gpt keeps its own encrypted reasoning", OriginOpenAI, assistant(gpt, call),
			`user[text] assistant[reasoning("gpt summary" sig="" enc="blob") tool_call]`},
		{"gpt does not get claude reasoning", OriginOpenAI, assistant(claude, text), `user[text] assistant[text]`},
		{"gemini does not get claude reasoning", OriginGemini, assistant(claude, call), `user[text] assistant[tool_call]`},
		{"gemini keeps its own reasoning", OriginGemini, assistant(gemini, call),
			`user[text] assistant[reasoning("gemini thought" sig="" enc="") tool_call]`},
		{"reasoning of unknown origin is left to the provider", OriginGemini, assistant(legacyUnsigned, text),
			`user[text] assistant[reasoning("old thought" sig="" enc="") text]`},
		{"a message that was only foreign reasoning is dropped", OriginOpenAI, assistant(claude), `user[text]`},
		{"deepseek keeps foreign reasoning as unsigned reasoning", OriginDeepSeek, assistant(claude, call),
			`user[text] assistant[reasoning("claude thought" sig="" enc="") tool_call]`},
		{"deepseek keeps its own reasoning, signature intact", OriginDeepSeek, assistant(deepseek, call),
			`user[text] assistant[reasoning("deepseek thought" sig="sig-ds" enc="") tool_call]`},
		{"deepseek gets a placeholder on a tool-calling turn without reasoning", OriginDeepSeek, assistant(call),
			`user[text] assistant[reasoning("(reasoning from an earlier model is not available)" sig="" enc="") tool_call]`},
		{"deepseek gets a placeholder when foreign reasoning has no text", OriginDeepSeek, assistant(reasoning(OriginOpenAI, "", "", "blob"), call),
			`user[text] assistant[reasoning("(reasoning from an earlier model is not available)" sig="" enc="") tool_call]`},
		{"deepseek needs no placeholder without a tool call", OriginDeepSeek, assistant(text), `user[text] assistant[text]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := shape(tc.input)
			if got := shape(projectReasoning(tc.input, tc.target)); got != tc.want {
				t.Errorf("projected\n  got  %s\n  want %s", got, tc.want)
			}
			if after := shape(tc.input); after != before {
				t.Errorf("input history was modified:\n  before %s\n  after  %s", before, after)
			}
		})
	}
}
