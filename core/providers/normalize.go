package providers

import "github.com/jayyao97/zotigo/core/protocol"

// MergeConsecutiveUserMessages folds adjacent user-role messages into a
// single message by concatenating their content blocks. Run by every
// provider converter as the last protocol-level step before mapping to
// SDK-specific shapes.
//
// Why this exists:
//   - Anthropic 1P API tolerates consecutive user messages (it merges
//     them server-side), but AWS Bedrock — which exposes the same Claude
//     models — does not, and rejects with "messages: roles must
//     alternate".
//   - Some Gemini deployments enforce strict user/model alternation.
//   - The compactor leaves us in this state by design: it injects a
//     summary message right before a preserved tail that always begins
//     with a user message (the partition snaps to a user boundary so
//     tool-call chains aren't split). Without this normalize step,
//     resumed sessions on Bedrock would 400 immediately after compact.
//
// Mirrors claude-code's normalizeMessagesForAPI behaviour. Operates on
// a copy; the input slice is not mutated.
//
// Tool messages (protocol.RoleTool) are left alone — providers that
// represent tool results as "user with tool_result content" handle that
// adjacency themselves at conversion time.
func MergeConsecutiveUserMessages(msgs []protocol.Message) []protocol.Message {
	if len(msgs) < 2 {
		return msgs
	}
	out := make([]protocol.Message, 0, len(msgs))
	for _, m := range msgs {
		if len(out) > 0 &&
			out[len(out)-1].Role == protocol.RoleUser &&
			m.Role == protocol.RoleUser {
			prev := out[len(out)-1]
			merged := make([]protocol.ContentPart, 0, len(prev.Content)+len(m.Content))
			merged = append(merged, prev.Content...)
			merged = append(merged, m.Content...)
			prev.Content = merged
			out[len(out)-1] = prev
			continue
		}
		out = append(out, m)
	}
	return out
}
