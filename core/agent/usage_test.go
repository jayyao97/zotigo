package agent

import (
	"encoding/json"
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
)

func TestUsageFromToolResultMetadataAcceptsJSONNumbers(t *testing.T) {
	metadata := map[string]any{"usage": map[string]any{
		"input_tokens":  json.Number("11"),
		"output_tokens": json.Number("7"),
		"total_tokens":  json.Number("18"),
	}}
	got := usageFromToolResultMetadata(metadata)
	want := (protocol.Usage{InputTokens: 11, OutputTokens: 7, TotalTokens: 18}).Normalized()
	if got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}
