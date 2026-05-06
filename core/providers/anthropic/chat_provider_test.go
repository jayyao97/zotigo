package anthropic

import (
	"testing"

	anthropicSDK "github.com/anthropics/anthropic-sdk-go"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
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

func TestApplyThinkingConfig_SuppressesThinkingForForcedToolChoice(t *testing.T) {
	tests := []struct {
		name         string
		toolChoice   providers.ToolChoice
		wantThinking bool
	}{
		{
			name:         "auto allows thinking",
			toolChoice:   providers.ToolChoice{},
			wantThinking: true,
		},
		{
			name: "required suppresses thinking",
			toolChoice: providers.ToolChoice{
				Mode: providers.ToolChoiceRequired,
			},
		},
		{
			name: "specific suppresses thinking",
			toolChoice: providers.ToolChoice{
				Mode: providers.ToolChoiceSpecific,
				Name: "record_decision",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params := anthropicSDK.MessageNewParams{MaxTokens: 4096}
			applyThinkingConfig(&params, "low", tc.toolChoice)

			gotThinking := params.Thinking.OfAdaptive != nil
			if gotThinking != tc.wantThinking {
				t.Fatalf("thinking enabled = %t, want %t", gotThinking, tc.wantThinking)
			}
			if tc.wantThinking && params.OutputConfig.Effort != anthropicSDK.OutputConfigEffortLow {
				t.Fatalf("effort = %q, want low", params.OutputConfig.Effort)
			}
			if !tc.wantThinking && params.OutputConfig.Effort != "" {
				t.Fatalf("effort = %q, want empty", params.OutputConfig.Effort)
			}
		})
	}
}

func TestUpdateUsage_UsesLatestCumulativeCounts(t *testing.T) {
	var usage protocol.Usage
	updateUsage(&usage, 10, 20, 30, 0)
	updateUsage(&usage, 11, 21, 31, 5)

	want := protocol.Usage{
		InputTokens:              11,
		OutputTokens:             5,
		CacheCreationInputTokens: 21,
		CacheReadInputTokens:     31,
	}
	if usage != want {
		t.Fatalf("usage = %+v, want %+v", usage, want)
	}
}
