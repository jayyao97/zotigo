package services

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jayyao97/zotigo/core/protocol"
)

func TestToolProjectionIsStableAcrossRepeatedUseAndRestore(t *testing.T) {
	c := NewCompressor(DefaultCompressorConfig())
	for name, text := range map[string]string{
		"english":        strings.Repeat("word ", 20000),
		"chinese":        strings.Repeat("中文报告", 2000),
		"lines":          strings.Repeat("中文日志\n", 2000),
		"malformed utf8": strings.Repeat("hello\xff\xfe", 2000),
	} {
		t.Run(name, func(t *testing.T) {
			h := []protocol.Message{protocol.NewToolMessage([]protocol.ToolResult{{ToolName: "shell", Type: protocol.ToolResultTypeText, Text: text}})}
			once := c.ProjectPrompt(h, 2000)
			projected := once[0].Content[0].ToolResult.Text
			if !utf8.ValidString(projected) || c.counter(projected) > 2000 {
				t.Fatalf("projection not bounded valid text: %d tokens", c.counter(projected))
			}
			if ContextDigest(c.ProjectPrompt(once, 2000)) != ContextDigest(once) {
				t.Fatal("second projection changed history")
			}
			encoded, _ := json.Marshal(h)
			var restored []protocol.Message
			if err := json.Unmarshal(encoded, &restored); err != nil {
				t.Fatal(err)
			}
			if ContextDigest(c.ProjectPrompt(restored, 2000)) != ContextDigest(once) {
				t.Fatal("restoring raw history changed projection")
			}
			prompt := protocol.ContextInput{PromptKey: "prompt", ProfileKey: "profile", PrefixTokens: 100}
			anchor := prompt
			anchor.HistoryDigest = ContextDigest(once)
			answer := protocol.NewAssistantMessage("answer")
			answer.Metadata = &protocol.MessageMetadata{ContextInput: &anchor, Usage: &protocol.Usage{InputTokens: 576000}}
			h = append(h, answer)
			for _, input := range [][]protocol.Message{h, c.ProjectPrompt(h, 2000)} {
				tokens, valid := c.ContextTokens(input, prompt)
				if !valid || tokens < 576000 || !c.NeedsCompressionTokens(tokens) {
					t.Fatalf("lost calibrated overflow: %d %v", tokens, valid)
				}
			}
		})
	}
}
