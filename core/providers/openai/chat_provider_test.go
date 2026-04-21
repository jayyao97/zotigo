package openai

import (
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/openai/openai-go/v3/packages/respjson"
)

func TestExtractReasoningDelta(t *testing.T) {
	tests := []struct {
		name  string
		extra map[string]respjson.Field
		want  string
	}{
		{
			name:  "nil map",
			extra: nil,
			want:  "",
		},
		{
			name: "reasoning_content (DeepSeek / llama.cpp)",
			extra: map[string]respjson.Field{
				"reasoning_content": respjson.NewField(`"step 1: think"`),
			},
			want: "step 1: think",
		},
		{
			name: "reasoning (OpenRouter)",
			extra: map[string]respjson.Field{
				"reasoning": respjson.NewField(`"analysis"`),
			},
			want: "analysis",
		},
		{
			name: "thinking fallback",
			extra: map[string]respjson.Field{
				"thinking": respjson.NewField(`"older llama.cpp"`),
			},
			want: "older llama.cpp",
		},
		{
			name: "unknown keys ignored",
			extra: map[string]respjson.Field{
				"something_else": respjson.NewField(`"noise"`),
			},
			want: "",
		},
		{
			name: "empty string returns empty",
			extra: map[string]respjson.Field{
				"reasoning_content": respjson.NewField(`""`),
			},
			want: "",
		},
		{
			name: "null value is skipped",
			extra: map[string]respjson.Field{
				"reasoning_content": respjson.NewField(`null`),
			},
			want: "",
		},
		{
			name: "reasoning_content wins over reasoning when both present",
			extra: map[string]respjson.Field{
				"reasoning_content": respjson.NewField(`"primary"`),
				"reasoning":         respjson.NewField(`"secondary"`),
			},
			want: "primary",
		},
		{
			name: "malformed JSON skipped",
			extra: map[string]respjson.Field{
				"reasoning_content": respjson.NewInvalidField(`not a json string`),
			},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractReasoningDelta(tc.extra)
			if got != tc.want {
				t.Errorf("extractReasoningDelta() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMapFinishReason(t *testing.T) {
	tests := []struct {
		name     string
		reason   string
		expected protocol.FinishReason
	}{
		{
			name:     "stop",
			reason:   "stop",
			expected: protocol.FinishReasonStop,
		},
		{
			name:     "length",
			reason:   "length",
			expected: protocol.FinishReasonLength,
		},
		{
			name:     "tool calls",
			reason:   "tool_calls",
			expected: protocol.FinishReasonToolCalls,
		},
		{
			name:     "content filter",
			reason:   "content_filter",
			expected: protocol.FinishReasonContentFilter,
		},
		{
			name:     "unknown",
			reason:   "other",
			expected: protocol.FinishReasonUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actual := mapFinishReason(tc.reason)
			if actual != tc.expected {
				t.Fatalf("expected %q, got %q", tc.expected, actual)
			}
		})
	}
}
