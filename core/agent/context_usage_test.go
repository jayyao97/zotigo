package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/services"
)

func calibratedTestAgent() *Agent {
	a := &Agent{
		cfg:          config.ProfileConfig{Provider: "anthropic", Model: "test", ContextWindow: 512000},
		compressor:   services.NewCompressor(services.CompressorConfig{ContextWindowSize: 512000}),
		conversation: newConversationState(),
	}
	for range 20 {
		a.conversation.history = append(a.conversation.history,
			protocol.NewUserMessage(strings.Repeat("old question details ", 100)),
			protocol.NewAssistantMessage(strings.Repeat("old response details ", 100)))
	}
	index := len(a.conversation.history) - 1
	anchor := a.contextPromptLocked()
	anchor.HistoryDigest = services.ContextDigest(a.projectPromptHistory(a.conversation.history[:index]))
	a.conversation.history[index].Metadata = &protocol.MessageMetadata{
		Usage:        &protocol.Usage{InputTokens: 2, CacheCreationInputTokens: 835, CacheReadInputTokens: 526326},
		ContextInput: &anchor,
	}
	return a
}

func TestBuildContextUsesAPIAndInvalidatesAfterCompression(t *testing.T) {
	a := calibratedTestAgent()
	if a.compressor.NeedsCompression(a.conversation.history) {
		t.Fatal("fixture local estimate should be below threshold")
	}
	_, _, compact, err := a.buildContext()
	if err != nil || compact == nil {
		t.Fatalf("API-driven compaction: %+v %v", compact, err)
	}
	if compact.OriginalTokens < 527163 {
		t.Fatalf("before: %d", compact.OriginalTokens)
	}
	_, _, again, err := a.buildContext()
	if err != nil || again != nil {
		t.Fatalf("stale usage triggered repeated compression: %+v %v", again, err)
	}
	restored := calibratedTestAgent()
	restored.Restore(a.Snapshot())
	_, _, again, err = restored.buildContext()
	if err != nil || again != nil {
		t.Fatalf("restored compressed history: %+v %v", again, err)
	}
}

func TestContextCalibrationModelSwitchAndRestore(t *testing.T) {
	a := calibratedTestAgent()
	restored := calibratedTestAgent()
	restored.Restore(a.Snapshot())
	if got := restored.contextTokensLocked(); got < 527163 {
		t.Fatalf("restore lost valid baseline: %d", got)
	}
	restored.cfg.Model = "different-model"
	if got := restored.contextTokensLocked(); got >= 409600 {
		t.Fatalf("model switch retained baseline: %d", got)
	}
}

func TestManualCompactionIgnoresThreshold(t *testing.T) {
	a := calibratedTestAgent()
	for index := range a.conversation.history {
		a.conversation.history[index].Metadata = nil
	}
	result, err := a.ForceCompress(context.Background())
	if err != nil || !result.Compressed {
		t.Fatalf("manual compaction below threshold: %+v %v", result, err)
	}
	a.runActive = true
	if _, err := a.ForceCompress(context.Background()); err == nil {
		t.Fatal("manual compaction raced active generation")
	}
}
