package protocol

import (
	"encoding/json"
	"fmt"
	"time"
)

// Message is the generic interface for all message types.
type Message struct {
	ID         string           `json:"id,omitempty"`
	Role       Role             `json:"role"`
	Content    []ContentPart    `json:"content"`
	Metadata   *MessageMetadata `json:"metadata,omitempty"`
	CreatedAt  time.Time        `json:"created_at"`
	FinishedAt *time.Time       `json:"finished_at,omitempty"`
}

type MessageMetadata struct {
	Provider string         `json:"provider,omitempty"`
	Model    string         `json:"model,omitempty"`
	Usage    *Usage         `json:"usage,omitempty"`
	Raw      map[string]any `json:"raw,omitempty"`
}

// Usage normalizes token-accounting fields across providers.
//
// Convention: InputTokens counts ONLY new (uncached) prompt tokens.
// CacheCreationInputTokens and CacheReadInputTokens are reported
// separately, never folded back into InputTokens. Total prompt size is
// therefore InputTokens + CacheCreationInputTokens + CacheReadInputTokens
// (see TotalInput). This matches Anthropic's wire format directly; the
// OpenAI and Gemini adapters subtract cached counts from their
// "prompt_tokens" / "prompt_token_count" before assigning InputTokens
// so the same arithmetic holds for every provider.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	TotalTokens              int `json:"total_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// TotalInput returns the full prompt size (uncached + cache-create + cache-read).
func (u Usage) TotalInput() int {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// Add returns u + o component-wise. Useful for session-level rollups.
func (u Usage) Add(o Usage) Usage {
	return Usage{
		InputTokens:              u.InputTokens + o.InputTokens,
		OutputTokens:             u.OutputTokens + o.OutputTokens,
		TotalTokens:              u.TotalTokens + o.TotalTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens + o.CacheCreationInputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens + o.CacheReadInputTokens,
	}
}

// Normalized fills TotalTokens from the components when the provider
// did not supply it (e.g. Anthropic only emits per-component counts).
func (u Usage) Normalized() Usage {
	if u.TotalTokens == 0 {
		u.TotalTokens = u.TotalInput() + u.OutputTokens
	}
	return u
}

// SessionUsage walks the assistant messages in history and returns the
// component-wise sum of every recorded Usage. Each per-message usage
// is normalized first so turns persisted before the agent started
// filling TotalTokens (older Anthropic sessions on disk) still
// contribute a non-zero total. Without this, a resumed legacy session
// would have TotalTokens=0 and any caller gating on it would hide its
// readout despite Input/Output/cache being populated.
func SessionUsage(history []Message) Usage {
	var total Usage
	for _, msg := range history {
		if msg.Role != RoleAssistant {
			continue
		}
		if msg.Metadata == nil || msg.Metadata.Usage == nil {
			continue
		}
		total = total.Add(msg.Metadata.Usage.Normalized())
	}
	return total
}

// LastTurnUsage returns the most recent assistant turn's recorded
// usage (or false when history has none). The last turn's TotalInput
// is the closest proxy for "current context occupancy" — exactly what
// the next request will pay for before any new user content is added.
func LastTurnUsage(history []Message) (Usage, bool) {
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		if m.Role != RoleAssistant {
			continue
		}
		if m.Metadata == nil || m.Metadata.Usage == nil {
			continue
		}
		return *m.Metadata.Usage, true
	}
	return Usage{}, false
}

// CountAssistantTurns returns how many assistant messages are in
// history. Centralized so /cost and the TUI summary can't drift on
// what counts as a turn.
func CountAssistantTurns(history []Message) int {
	n := 0
	for _, msg := range history {
		if msg.Role == RoleAssistant {
			n++
		}
	}
	return n
}

type ContentPart struct {
	Type      ContentType `json:"type"`
	Text      string      `json:"text,omitempty"`
	Signature string      `json:"signature,omitempty"` // Anthropic thinking block signature
	// ReasoningID + EncryptedContent are populated on reasoning content
	// parts produced by the OpenAI Responses API when the request
	// includes "reasoning.encrypted_content". Both fields must be
	// echoed back verbatim on the next turn (via a reasoning input
	// item) to preserve chain-of-thought across a stateless call.
	ReasoningID      string      `json:"reasoning_id,omitempty"`
	EncryptedContent string      `json:"encrypted_content,omitempty"`
	Image            *MediaPart  `json:"image,omitempty"`
	Audio            *MediaPart  `json:"audio,omitempty"`
	Video            *MediaPart  `json:"video,omitempty"`
	File             *FilePart   `json:"file,omitempty"`
	ToolCall         *ToolCall   `json:"tool_call,omitempty"`
	ToolResult       *ToolResult `json:"tool_result,omitempty"`
}

type MediaPart struct {
	Data      []byte `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
	FileID    string `json:"file_id,omitempty"` // Cloud provider file ID
	MediaType string `json:"media_type,omitempty"`
}

type FilePart struct {
	Data      []byte `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
	FileID    string `json:"file_id,omitempty"` // Cloud provider file ID
	Name      string `json:"name,omitempty"`
	MediaType string `json:"media_type,omitempty"`
}

type ToolCall struct {
	Index     int    `json:"index,omitempty"` // For streaming ordering
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolResult follows the Vercel AI SDK V3 spec structure (flattened for Go).
type ToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name,omitempty"`

	Type ToolResultType `json:"type"`

	// Mutually exclusive fields based on Type
	Text    string                  `json:"text,omitempty"`    // For text, error-text
	JSON    any                     `json:"json,omitempty"`    // For json, error-json
	Reason  string                  `json:"reason,omitempty"`  // For execution-denied
	Content []ToolResultContentPart `json:"content,omitempty"` // For content (multi-modal)

	IsError  bool           `json:"is_error,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// ToolResultContentPart is a piece of content within a ToolResultTypeContent result.
type ToolResultContentPart struct {
	Type  ContentType `json:"type"` // text, image
	Text  string      `json:"text,omitempty"`
	Image *MediaPart  `json:"image,omitempty"`
}

// Helper constructors

func NewSystemMessage(text string) Message {
	return Message{
		Role: RoleSystem,
		Content: []ContentPart{
			{Type: ContentTypeText, Text: text},
		},
		CreatedAt: time.Now(),
	}
}

func NewUserMessage(text string) Message {
	return Message{
		Role: RoleUser,
		Content: []ContentPart{
			{Type: ContentTypeText, Text: text},
		},
		CreatedAt: time.Now(),
	}
}

func NewAssistantMessage(text string) Message {
	msg := Message{
		Role:      RoleAssistant,
		Content:   []ContentPart{},
		CreatedAt: time.Now(),
	}
	if text != "" {
		msg.Content = append(msg.Content, ContentPart{Type: ContentTypeText, Text: text})
	}
	return msg
}

func (m *Message) AddToolCall(tc ToolCall) {
	m.Content = append(m.Content, ContentPart{
		Type:     ContentTypeToolCall,
		ToolCall: &tc,
	})
}

func NewToolMessage(results []ToolResult) Message {
	msg := Message{
		Role:      RoleTool,
		Content:   make([]ContentPart, len(results)),
		CreatedAt: time.Now(),
	}
	for i, res := range results {
		r := res
		msg.Content[i] = ContentPart{
			Type:       ContentTypeToolResult,
			ToolResult: &r,
		}
	}
	return msg
}

func NewTextToolResult(callID string, text string, isError bool) ToolResult {
	t := ToolResultTypeText
	if isError {
		t = ToolResultTypeErrorText
	}
	return ToolResult{
		ToolCallID: callID,
		Type:       t,
		Text:       text,
		IsError:    isError,
	}
}

func (m Message) String() string {
	var s string
	for _, p := range m.Content {
		switch p.Type {
		case ContentTypeText, ContentTypeReasoning:
			s += p.Text
		case ContentTypeImage:
			if s != "" {
				s += "\n"
			}
			s += "[🖼️ Image]"
		case ContentTypeToolCall:
			if p.ToolCall != nil {
				if s != "" {
					s += "\n"
				}
				s += fmt.Sprintf("[Call Tool: %s]", p.ToolCall.Name)
			}
		case ContentTypeToolResult:
			if p.ToolResult != nil {
				if s != "" {
					s += "\n"
				}
				res := p.ToolResult.Text
				if len(res) > 100 {
					res = res[:97] + "..."
				}
				s += fmt.Sprintf("[Result: %s]", res)
			}
		}
	}
	return s
}

func (m Message) JSONString() string {
	b, _ := json.Marshal(m)
	return string(b)
}
