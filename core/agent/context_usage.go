package agent

import (
	"encoding/json"
	"sort"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/services"
)

// contextPromptLocked describes static request input, including tool schemas
// which are outside the messages array but consume provider context.
func (a *Agent) contextPromptLocked() protocol.ContextInput {
	return a.contextPromptFor(a.systemPromptMessages())
}

func (a *Agent) contextPromptFor(system []protocol.Message) protocol.ContextInput {
	prefix := append([]protocol.Message(nil), system...)
	names := make([]string, 0, len(a.tools))
	for name := range a.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		tool := a.tools[name]
		definition, _ := json.Marshal(struct {
			Name        string
			Description string
			Schema      any
		}{tool.Name(), tool.Description(), tool.Schema()})
		prefix = append(prefix, protocol.NewSystemMessage(string(definition)))
	}
	prompt := protocol.ContextInput{
		PromptKey:  services.ContextDigest(prefix),
		ProfileKey: services.ContextProfileKey(a.cfg),
	}
	if a.compressor != nil {
		prompt.PrefixTokens = a.compressor.CountTokens(prefix)
	}
	return prompt
}

func (a *Agent) contextTokensLocked() int {
	tokens, _ := a.compressor.ContextTokens(a.conversation.history, a.contextPromptLocked())
	return tokens
}

// Uses the same calibrated estimate as compression, including assistant output
// and tool results since the last request. Caller holds a.mu.
func (a *Agent) contextUsageLocked(input protocol.ContextInput) *protocol.ContextUsage {
	if a.compressor == nil {
		return nil
	}
	tokens, calibrated := a.compressor.ContextTokens(a.conversation.history, input)
	window := a.cfg.ContextWindow
	if window <= 0 {
		window = config.DefaultContextWindow
	}
	source := "estimate"
	if calibrated {
		source = "provider_estimate"
	}
	return &protocol.ContextUsage{Tokens: tokens, Window: window, Source: source, ProfileKey: input.ProfileKey}
}
