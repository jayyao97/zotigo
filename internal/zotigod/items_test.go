package zotigod

import (
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

func TestPublicDisplayTurnIncludesUsage(t *testing.T) {
	usage := protocol.Usage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10, CacheReadInputTokens: 4}
	got := publicDisplayTurn(&zotigosession.DisplayTurn{ID: "turn-1", DurationMS: 1250, Usage: &usage})
	if got == nil || got.Usage == nil || *got.Usage != usage {
		t.Fatalf("public turn usage = %#v, want %#v", got, usage)
	}
}
