package agent

import (
	"context"
	"encoding/json"
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
	_, _, compact, err := a.buildContext(context.Background())
	if err != nil || compact == nil {
		t.Fatalf("API-driven compaction: %+v %v", compact, err)
	}
	if compact.OriginalTokens < 527163 {
		t.Fatalf("before: %d", compact.OriginalTokens)
	}
	_, _, again, err := a.buildContext(context.Background())
	if err != nil || again != nil {
		t.Fatalf("stale usage triggered repeated compression: %+v %v", again, err)
	}
	restored := calibratedTestAgent()
	restored.Restore(a.Snapshot())
	_, _, again, err = restored.buildContext(context.Background())
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

func TestBuildContextCompressesLargePrefixWithoutRemovingMessages(t *testing.T) {
	a := calibratedTestAgent()
	a.conversation.history = []protocol.Message{
		protocol.NewUserMessage(strings.Repeat("old task details ", 10000)),
		protocol.NewUserMessage("Continue with the latest task"),
	}
	a.compressor = services.NewCompressor(services.CompressorConfig{ContextWindowSize: 10000})
	_, _, compact, err := a.buildContext(context.Background())
	if err != nil || compact == nil {
		t.Fatalf("large prefix not compacted: %+v %v", compact, err)
	}
	if compact.MessagesAfter != compact.MessagesBefore || compact.CompressedTokens >= compact.OriginalTokens {
		t.Fatalf("expected token reduction with unchanged message count: %+v", compact)
	}
	if last := a.conversation.history[len(a.conversation.history)-1]; last.Content[0].Text != "Continue with the latest task" {
		t.Fatal("compaction removed the latest request")
	}
	if _, _, again, err := a.buildContext(context.Background()); err != nil || again != nil {
		t.Fatalf("small summary must not compact repeatedly: %+v %v", again, err)
	}
}

func TestAutomaticCompactionPreservesCalibrationWithLargeToolOutput(t *testing.T) {
	for _, restore := range []bool{false, true} {
		a := calibratedTestAgent()
		index := len(a.conversation.history) - 1
		a.conversation.history = append(a.conversation.history[:index],
			protocol.NewToolMessage([]protocol.ToolResult{{ToolName: "shell", Type: protocol.ToolResultTypeText, Text: strings.Repeat("中文日志\n", 2000)}}),
			protocol.NewAssistantMessage("latest answer"))
		index = len(a.conversation.history) - 1
		anchor := a.contextPromptLocked()
		anchor.HistoryDigest = services.ContextDigest(a.projectPromptHistory(a.conversation.history[:index]))
		a.conversation.history[index].Metadata = &protocol.MessageMetadata{Usage: &protocol.Usage{InputTokens: 576000}, ContextInput: &anchor}
		if restore {
			snapshot := a.Snapshot()
			encoded, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &snapshot); err != nil {
				t.Fatal(err)
			}
			a.Restore(snapshot)
		}
		before := a.contextTokensLocked()
		if before < 576000 {
			t.Fatalf("restore=%v: lost measured input: %d", restore, before)
		}
		provider := &compactionProvider{}
		a.compressor.SetSummarizer(services.NewProviderSummarizer(provider))
		_, _, compact, err := a.buildContext(context.Background())
		if err != nil || compact == nil {
			t.Fatalf("restore=%v: no auto compaction: %+v %v", restore, compact, err)
		}
		if compact.OriginalTokens != before || provider.calls != 1 {
			t.Fatalf("usage mismatch or no LLM summary: before=%d compact=%+v calls=%d", before, compact, provider.calls)
		}
	}
}
