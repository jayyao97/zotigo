package anthropic

import (
	"testing"

	anthropicSDK "github.com/anthropics/anthropic-sdk-go"
	"github.com/jayyao97/zotigo/core/protocol"
)

func TestMapStopReason(t *testing.T) {
	tests := []struct {
		name     string
		reason   anthropicSDK.StopReason
		expected protocol.FinishReason
	}{
		{
			name:     "end turn maps to stop",
			reason:   anthropicSDK.StopReasonEndTurn,
			expected: protocol.FinishReasonStop,
		},
		{
			name:     "max tokens maps to length",
			reason:   anthropicSDK.StopReasonMaxTokens,
			expected: protocol.FinishReasonLength,
		},
		{
			name:     "tool use maps to tool calls",
			reason:   anthropicSDK.StopReasonToolUse,
			expected: protocol.FinishReasonToolCalls,
		},
		{
			name:     "stop sequence maps to stop",
			reason:   anthropicSDK.StopReasonStopSequence,
			expected: protocol.FinishReasonStop,
		},
		{
			name:     "unknown maps to unknown",
			reason:   anthropicSDK.StopReason("other"),
			expected: protocol.FinishReasonUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actual := mapStopReason(tc.reason)
			if actual != tc.expected {
				t.Fatalf("expected %q, got %q", tc.expected, actual)
			}
		})
	}
}
