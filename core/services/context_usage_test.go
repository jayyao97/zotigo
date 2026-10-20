package services

import (
	"encoding/json"
	"testing"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
)

func TestContextProfileKeyOnlyTracksRequestShape(t *testing.T) {
	profile := config.ProfileConfig{Provider: "openai", Model: "model", BaseURL: "https://example.test", ThinkingLevel: "high"}
	key := ContextProfileKey(profile)
	changed := profile
	changed.APIKey = "a different credential"
	changed.ContextWindow = 512000
	changed.Params = map[string]any{"timeout": 90, "temperature": 0.5}
	if ContextProfileKey(changed) != key {
		t.Fatal("operational settings invalidated calibration")
	}
	for _, change := range []func(*config.ProfileConfig){
		func(p *config.ProfileConfig) { p.Provider = "anthropic" },
		func(p *config.ProfileConfig) { p.Model = "another" },
		func(p *config.ProfileConfig) { p.BaseURL = "https://another.test" },
		func(p *config.ProfileConfig) { p.ThinkingLevel = "low" },
		func(p *config.ProfileConfig) { p.Params = map[string]any{"mode": "responses"} },
	} {
		changed := profile
		change(&changed)
		if ContextProfileKey(changed) == key {
			t.Fatal("request shape change retained calibration")
		}
	}
}

func TestContextTokensCalibration(t *testing.T) {
	c := NewCompressor(CompressorConfig{ContextWindowSize: 512000})
	prompt := protocol.ContextInput{PromptKey: "system-tools", ProfileKey: "model-a", PrefixTokens: 20}
	history := []protocol.Message{protocol.NewUserMessage("hello")}
	anchor := prompt
	anchor.HistoryDigest = ContextDigest(history)
	answer := protocol.NewAssistantMessage("previous assistant output")
	answer.Metadata = &protocol.MessageMetadata{
		Usage:        &protocol.Usage{InputTokens: 2, CacheCreationInputTokens: 835, CacheReadInputTokens: 526326},
		ContextInput: &anchor,
	}
	history = append(history, answer)
	tokens, valid := c.ContextTokens(history, prompt)
	if !valid || tokens != 527163+c.CountTokens(history[1:]) || !c.NeedsCompressionTokens(tokens) || c.NeedsCompression(history) {
		t.Fatalf("API overflow/local underestimate: %d %v", tokens, valid)
	}
	// Cache tokens are counted once; output is measured from retained content,
	// not usage.OutputTokens (which may include hidden reasoning).
	answer.Metadata.Usage = &protocol.Usage{InputTokens: 409580, OutputTokens: 100000}
	history[1] = answer
	before, _ := c.ContextTokens(history, prompt)
	history = append(history, protocol.NewUserMessage("additional user input that crosses the remaining budget"),
		protocol.NewToolMessage([]protocol.ToolResult{{Text: "tool result with more content to account for"}}))
	after, valid := c.ContextTokens(history, prompt)
	if c.NeedsCompressionTokens(before) || !c.NeedsCompressionTokens(after) || !valid {
		t.Fatalf("delta failed to cross threshold: %d -> %d", before, after)
	}
	encoded, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	var restored []protocol.Message
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if got, valid := c.ContextTokens(restored, prompt); got != after || !valid {
		t.Fatalf("restore: %d %v", got, valid)
	}
	for _, name := range []string{"no usage", "legacy usage", "model switch", "prompt switch", "rewritten history", "trimmed history"} {
		t.Run(name, func(t *testing.T) {
			copyHistory := append([]protocol.Message(nil), history...)
			copyPrompt := prompt
			switch name {
			case "no usage":
				copyHistory[1].Metadata = nil
			case "legacy usage":
				copyHistory[1].Metadata = &protocol.MessageMetadata{Usage: answer.Metadata.Usage}
			case "model switch":
				copyPrompt.ProfileKey = "model-b"
			case "prompt switch":
				copyPrompt.PromptKey = "changed tools"
			case "rewritten history":
				copyHistory[0] = protocol.NewUserMessage("[Previous conversation summary]")
			case "trimmed history":
				copyHistory = copyHistory[1:]
			}
			got, valid := c.ContextTokens(copyHistory, copyPrompt)
			if valid || got != copyPrompt.PrefixTokens+c.CountTokens(copyHistory) {
				t.Fatalf("invalid anchor reused: %d %v", got, valid)
			}
		})
	}
}
