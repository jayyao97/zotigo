package openai

import (
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
)

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
