package providers_test

import (
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
)

func TestMergeConsecutiveUserMessages(t *testing.T) {
	cases := []struct {
		name string
		in   []protocol.Message
		// We assert role sequence + per-message text concat to keep tests
		// readable without re-stating full ContentPart structs.
		wantRoles []protocol.Role
		wantTexts []string
	}{
		{
			name:      "empty",
			in:        nil,
			wantRoles: nil,
			wantTexts: nil,
		},
		{
			name: "single user untouched",
			in: []protocol.Message{
				userText("hi"),
			},
			wantRoles: []protocol.Role{protocol.RoleUser},
			wantTexts: []string{"hi"},
		},
		{
			name: "alternating user/assistant untouched",
			in: []protocol.Message{
				userText("hi"),
				asstText("yes"),
				userText("more"),
			},
			wantRoles: []protocol.Role{protocol.RoleUser, protocol.RoleAssistant, protocol.RoleUser},
			wantTexts: []string{"hi", "yes", "more"},
		},
		{
			name: "post-compaction shape: summary user + preserved user collapses",
			in: []protocol.Message{
				{Role: protocol.RoleSystem, Content: textParts("sys")},
				userText("[Previous conversation summary]\n<context_summary/>"),
				userText("can you continue?"),
				asstText("sure"),
			},
			wantRoles: []protocol.Role{protocol.RoleSystem, protocol.RoleUser, protocol.RoleAssistant},
			wantTexts: []string{
				"sys",
				"[Previous conversation summary]\n<context_summary/>can you continue?",
				"sure",
			},
		},
		{
			name: "three consecutive users collapse into one",
			in: []protocol.Message{
				userText("a"),
				userText("b"),
				userText("c"),
				asstText("ok"),
			},
			wantRoles: []protocol.Role{protocol.RoleUser, protocol.RoleAssistant},
			wantTexts: []string{"abc", "ok"},
		},
		{
			name: "tool messages between users are NOT merged across",
			in: []protocol.Message{
				userText("a"),
				{Role: protocol.RoleTool, Content: textParts("tool-result")},
				userText("b"),
			},
			wantRoles: []protocol.Role{protocol.RoleUser, protocol.RoleTool, protocol.RoleUser},
			wantTexts: []string{"a", "tool-result", "b"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := providers.MergeConsecutiveUserMessages(tc.in)
			if len(got) != len(tc.wantRoles) {
				t.Fatalf("len = %d, want %d (got=%v)", len(got), len(tc.wantRoles), roleSeq(got))
			}
			for i, m := range got {
				if m.Role != tc.wantRoles[i] {
					t.Errorf("msg[%d].Role = %s, want %s", i, m.Role, tc.wantRoles[i])
				}
				if text := concatText(m); text != tc.wantTexts[i] {
					t.Errorf("msg[%d] text = %q, want %q", i, text, tc.wantTexts[i])
				}
			}
		})
	}
}

func TestMergeConsecutiveUserMessages_DoesNotMutateInput(t *testing.T) {
	in := []protocol.Message{
		userText("a"),
		userText("b"),
	}
	_ = providers.MergeConsecutiveUserMessages(in)
	if len(in) != 2 || concatText(in[0]) != "a" || concatText(in[1]) != "b" {
		t.Errorf("input mutated: %+v", in)
	}
}

func userText(s string) protocol.Message {
	return protocol.Message{Role: protocol.RoleUser, Content: textParts(s)}
}

func asstText(s string) protocol.Message {
	return protocol.Message{Role: protocol.RoleAssistant, Content: textParts(s)}
}

func textParts(s string) []protocol.ContentPart {
	return []protocol.ContentPart{{Type: protocol.ContentTypeText, Text: s}}
}

func concatText(m protocol.Message) string {
	out := ""
	for _, p := range m.Content {
		if p.Type == protocol.ContentTypeText {
			out += p.Text
		}
	}
	return out
}

func roleSeq(msgs []protocol.Message) []protocol.Role {
	r := make([]protocol.Role, len(msgs))
	for i, m := range msgs {
		r[i] = m.Role
	}
	return r
}
