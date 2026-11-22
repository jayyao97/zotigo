package openai

import (
	"encoding/json"

	"github.com/jayyao97/zotigo/core/protocol"
)

// openAIUsage maps an OpenAI-shaped usage record onto protocol.Usage.
// Prompt counts include cached reads and, where reported, cache writes;
// both are subtracted so InputTokens is the uncached remainder.
//
// The official API reports no cache writes, but compatible services that
// front Anthropic-style caching add cache_write_tokens (some relays also
// spell it cached_creation_tokens) to the token details. The SDK does not
// model the field, so it is read from the raw details JSON.
func openAIUsage(prompt, cached, output, total int64, detailsJSON string) *protocol.Usage {
	written := cacheWriteTokens(detailsJSON)
	return &protocol.Usage{
		InputTokens:              max(int(prompt-cached)-written, 0),
		OutputTokens:             int(output),
		TotalTokens:              int(total),
		CacheCreationInputTokens: written,
		CacheReadInputTokens:     int(cached),
	}
}

func cacheWriteTokens(detailsJSON string) int {
	if detailsJSON == "" {
		return 0
	}
	var details struct {
		CacheWrite    *int `json:"cache_write_tokens"`
		CacheCreation *int `json:"cached_creation_tokens"`
	}
	if json.Unmarshal([]byte(detailsJSON), &details) != nil {
		return 0
	}
	for _, value := range []*int{details.CacheWrite, details.CacheCreation} {
		if value != nil && *value > 0 {
			return *value
		}
	}
	return 0
}
