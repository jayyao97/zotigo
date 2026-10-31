package services

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
)

// ContextDigest hashes model-visible content only. Metadata, timestamps and
// display IDs can change without changing a provider request.
func ContextDigest(messages []protocol.Message) string {
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	for _, message := range messages {
		_ = encoder.Encode(struct {
			Role    protocol.Role
			Content []protocol.ContentPart
		}{message.Role, message.Content})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// ContextProfileKey also invalidates calibration on endpoint/thinking changes.
// Credentials, timeouts and approval settings do not describe model input.
func ContextProfileKey(profile config.ProfileConfig) string {
	data, _ := json.Marshal(struct {
		Provider      string
		Model         string
		BaseURL       string
		ThinkingLevel string
		Mode          any
	}{profile.Provider, profile.Model, profile.BaseURL, profile.ThinkingLevel, profile.Params["mode"]})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ContextTokens returns next-request occupancy, using a measured request only
// while the measured history and model still match. Prompt/tool changes are
// estimated as a delta rather than discarding the provider measurement.
// The assistant carrying usage was OUTPUT of that request: include it, and all
// subsequent user/tool messages, in the locally estimated delta.
func (c *Compressor) ContextTokens(history []protocol.Message, prompt protocol.ContextInput) (tokens int, calibrated bool) {
	history = c.ProjectPrompt(history, 2000)
	for index := len(history) - 1; index >= 0; index-- {
		message := history[index]
		if message.Role != protocol.RoleAssistant || message.Metadata == nil || message.Metadata.Usage == nil {
			continue
		}
		meta := message.Metadata
		anchor := meta.ContextInput
		// Never fall back to an older measurement across an uncalibrated call.
		if anchor == nil || anchor.PromptKey == "" || anchor.ProfileKey == "" ||
			anchor.ProfileKey != prompt.ProfileKey ||
			meta.Usage.InputTokens < 0 || meta.Usage.CacheCreationInputTokens < 0 || meta.Usage.CacheReadInputTokens < 0 ||
			meta.Usage.TotalInput() <= 0 || anchor.HistoryDigest != ContextDigest(history[:index]) {
			break
		}
		// Local tokenization can differ significantly from the provider. Account
		// for prompt growth, but don't credit estimated savings until the next
		// API measurement confirms them. This also tolerates estimator changes.
		prefixGrowth := max(0, prompt.PrefixTokens-anchor.PrefixTokens)
		return meta.Usage.TotalInput() + prefixGrowth + c.CountTokens(history[index:]), true
	}
	return prompt.PrefixTokens + c.CountTokens(history), false
}

func (c *Compressor) NeedsCompressionTokens(tokens int) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return tokens > int(float64(c.config.ContextWindowSize)*c.config.TriggerRatio)
}
