package zotigod

import (
	"crypto/sha256"
	"encoding/json"

	"github.com/jayyao97/zotigo/core/protocol"
)

type contextUsageCacheEntry struct {
	digest     [32]byte
	tokens     int
	calibrated bool
}

// Snapshot timestamps are not a content revision: compaction and usage updates
// must invalidate the estimate even when the displayed updated_at is unchanged.
// Hashing avoids repeated BPE tokenization on sidebar/detail polling.
func (h *handler) nativeContextTokens(id string, history []protocol.Message, prompt protocol.ContextInput) (int, bool) {
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	if encoder.Encode(history) != nil || encoder.Encode(prompt) != nil {
		return nativeContextEstimator().ContextTokens(history, prompt)
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	h.contextUsageMu.Lock()
	cached, found := h.contextUsageCache[id]
	h.contextUsageMu.Unlock()
	if found && cached.digest == digest {
		return cached.tokens, cached.calibrated
	}
	tokens, calibrated := nativeContextEstimator().ContextTokens(history, prompt)
	h.contextUsageMu.Lock()
	if h.contextUsageCache == nil || len(h.contextUsageCache) >= 128 {
		h.contextUsageCache = make(map[string]contextUsageCacheEntry)
	}
	h.contextUsageCache[id] = contextUsageCacheEntry{digest, tokens, calibrated}
	h.contextUsageMu.Unlock()
	return tokens, calibrated
}
