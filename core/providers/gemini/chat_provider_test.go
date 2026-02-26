package gemini

import (
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
	"google.golang.org/genai"
)

func TestMapFinishReason(t *testing.T) {
	tests := []struct {
		name     string
		reason   genai.FinishReason
		expected protocol.FinishReason
	}{
		{
			name:     "stop",
			reason:   genai.FinishReasonStop,
			expected: protocol.FinishReasonStop,
		},
		{
			name:     "max tokens",
			reason:   genai.FinishReasonMaxTokens,
			expected: protocol.FinishReasonLength,
		},
		{
			name:     "safety maps to content filter",
			reason:   genai.FinishReasonSafety,
			expected: protocol.FinishReasonContentFilter,
		},
		{
			name:     "recitation maps to content filter",
			reason:   genai.FinishReasonRecitation,
			expected: protocol.FinishReasonContentFilter,
		},
		{
			name:     "unknown",
			reason:   genai.FinishReason("other"),
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
