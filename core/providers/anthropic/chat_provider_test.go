package anthropic

import (
	"encoding/json"
	"testing"

	anthropicSDK "github.com/anthropics/anthropic-sdk-go"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
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

func TestApplyThinkingConfig_UsesAdaptiveThinking(t *testing.T) {
	params := anthropicSDK.MessageNewParams{MaxTokens: 4096}
	applyThinkingConfig(&params, "low")

	if params.Thinking.OfAdaptive == nil {
		t.Fatal("expected adaptive thinking config")
	}
	if params.OutputConfig.Effort != anthropicSDK.OutputConfigEffortLow {
		t.Fatalf("effort = %q, want low", params.OutputConfig.Effort)
	}
}

func TestApplyThinkingConfig_DisablesThinkingExplicitly(t *testing.T) {
	params := anthropicSDK.MessageNewParams{MaxTokens: 4096}
	applyThinkingConfig(&params, "disabled")

	if params.Thinking.OfDisabled == nil {
		t.Fatal("expected disabled thinking config")
	}
	if params.Thinking.OfAdaptive != nil {
		t.Fatal("did not expect adaptive thinking config")
	}
	if params.OutputConfig.Effort != "" {
		t.Fatalf("effort = %q, want empty", params.OutputConfig.Effort)
	}
}

func TestAdaptiveThinkingPreservesForcedToolChoice(t *testing.T) {
	decisionTool := &mockTool{
		name:        "record_decision",
		description: "Record the decision",
		schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
	tests := []struct {
		name           string
		choice         providers.ToolChoice
		wantType       string
		wantToolName   string
		assertInternal func(*testing.T, anthropicSDK.MessageNewParams)
	}{
		{
			name:     "required",
			choice:   providers.ToolChoice{Mode: providers.ToolChoiceRequired},
			wantType: "any",
			assertInternal: func(t *testing.T, params anthropicSDK.MessageNewParams) {
				t.Helper()
				if params.ToolChoice.OfAny == nil {
					t.Fatal("expected any tool choice")
				}
			},
		},
		{
			name: "specific",
			choice: providers.ToolChoice{
				Mode: providers.ToolChoiceSpecific,
				Name: "record_decision",
			},
			wantType:     "tool",
			wantToolName: "record_decision",
			assertInternal: func(t *testing.T, params anthropicSDK.MessageNewParams) {
				t.Helper()
				if params.ToolChoice.OfTool == nil || params.ToolChoice.OfTool.Name != "record_decision" {
					t.Fatalf("unexpected specific tool choice: %#v", params.ToolChoice.OfTool)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params, err := convertToAnthropicParams(
				[]protocol.Message{protocol.NewUserMessage("classify")},
				[]tools.Tool{decisionTool},
				tc.choice,
			)
			if err != nil {
				t.Fatalf("convert params: %v", err)
			}
			applyThinkingConfig(&params, "low")

			if params.Thinking.OfAdaptive == nil {
				t.Fatal("expected adaptive thinking config")
			}
			tc.assertInternal(t, params)

			wireJSON, err := json.Marshal(params)
			if err != nil {
				t.Fatalf("marshal params: %v", err)
			}
			var wire struct {
				Thinking struct {
					Type string `json:"type"`
				} `json:"thinking"`
				ToolChoice struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"tool_choice"`
			}
			if err := json.Unmarshal(wireJSON, &wire); err != nil {
				t.Fatalf("unmarshal params: %v", err)
			}
			if wire.Thinking.Type != "adaptive" {
				t.Fatalf("wire thinking type = %q, want adaptive", wire.Thinking.Type)
			}
			if wire.ToolChoice.Type != tc.wantType || wire.ToolChoice.Name != tc.wantToolName {
				t.Fatalf("wire tool_choice = %#v, want type=%q name=%q", wire.ToolChoice, tc.wantType, tc.wantToolName)
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
