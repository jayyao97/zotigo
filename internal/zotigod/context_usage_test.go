package zotigod

import (
	"context"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/services"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

func TestContextUsageCacheInvalidatesOnContentAndCalibration(t *testing.T) {
	h := &handler{}
	history := []protocol.Message{protocol.NewUserMessage("hello")}
	prompt := protocol.ContextInput{PromptKey: "prompt", ProfileKey: "profile", PrefixTokens: 10}
	first, _ := h.nativeContextTokens("session", history, prompt)
	anchor := prompt
	anchor.HistoryDigest = services.ContextDigest(history)
	answer := protocol.NewAssistantMessage("answer")
	answer.Metadata = &protocol.MessageMetadata{ContextInput: &anchor, Usage: &protocol.Usage{InputTokens: 1000}}
	history = append(history, answer)
	measured, valid := h.nativeContextTokens("session", history, prompt)
	if !valid || measured <= first {
		t.Fatalf("new usage not reflected: %d %v", measured, valid)
	}
	answer.Metadata.Usage.InputTokens = 2000
	updated, _ := h.nativeContextTokens("session", history, prompt)
	if updated != measured+1000 {
		t.Fatal("usage-only update not reflected")
	}
	prompt.ProfileKey = "another model"
	fallback, valid := h.nativeContextTokens("session", history, prompt)
	if valid || fallback >= updated {
		t.Fatal("profile switch retained calibration")
	}
	trimmed, _ := h.nativeContextTokens("session", history[:1], prompt)
	if trimmed != first {
		t.Fatal("history trim not reflected")
	}
}

func TestNativeContextUsageUsesValidatedBaselineAndDelta(t *testing.T) {
	workDir := t.TempDir()
	writeTestProfileConfig(t, workDir)
	cfg, err := config.NewManager().LoadForDir(workDir)
	if err != nil {
		t.Fatal(err)
	}
	_, profile, err := cfg.ResolveProfile("test")
	if err != nil {
		t.Fatal(err)
	}
	c := services.NewCompressor(services.CompressorConfig{ContextWindowSize: profile.ContextWindow})
	history := []protocol.Message{protocol.NewUserMessage("question")}
	anchor := protocol.ContextInput{
		HistoryDigest: services.ContextDigest(history), PromptKey: "system-tools",
		ProfileKey: services.ContextProfileKey(profile), PrefixTokens: 40,
	}
	answer := protocol.NewAssistantMessage("answer")
	answer.Metadata = &protocol.MessageMetadata{
		Usage:        &protocol.Usage{InputTokens: 2, CacheCreationInputTokens: 835, CacheReadInputTokens: 526326},
		ContextInput: &anchor,
	}
	history = append(history, answer, protocol.NewUserMessage("new user input"))
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now()
	sess := &zotigosession.Session{
		Metadata:      zotigosession.Metadata{ID: "context-baseline", Agent: "zotigo", ProfileName: "test", WorkingDirectory: workDir, CreatedAt: now, UpdatedAt: now},
		AgentSnapshot: agent.Snapshot{History: history},
	}
	if err := store.Put(ctx, sess); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	h := &handler{store: store, items: storedDisplayItemSource{store: store}}
	get := func() *SessionContextUsage {
		return h.decorateSession(ctx, sessionFromMetadata(loaded.Metadata, SessionStateOffline, false), loaded).ContextUsage
	}
	want := 527163 + c.CountTokens(history[1:])
	if got := get(); got.Tokens != want || got.Source != "provider_estimate" {
		t.Fatalf("usage: %+v want %d", got, want)
	}
	loaded.AgentSnapshot.History[0] = protocol.NewUserMessage("summary after compression")
	if got := get(); got.Source != "estimate" || got.Tokens != 40+c.CountTokens(loaded.AgentSnapshot.History) {
		t.Fatalf("compacted usage: %+v", got)
	}
	loaded.AgentSnapshot.History = history
	// A different model configuration cannot reuse the old request's input.
	loaded.AgentSnapshot.History[1].Metadata.ContextInput.ProfileKey = "previous-model"
	if got := get(); got.Source != "estimate" || got.Tokens >= 527163 {
		t.Fatalf("model switch: %+v", got)
	}
}
