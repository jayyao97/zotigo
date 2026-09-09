package protocol

// EventType discriminates the type of event in the stream.
type EventType string

const (
	// Content Stream
	EventTypeContentDelta EventType = "content_delta"
	EventTypeContentEnd   EventType = "content_end"

	// Tool Call Stream
	EventTypeToolCallDelta  EventType = "tool_call_delta"
	EventTypeToolCallEnd    EventType = "tool_call_end"
	EventTypeToolProgress   EventType = "tool_progress"
	EventTypeToolResultDone EventType = "tool_result_done"
	EventTypeSubagent       EventType = "subagent"

	// Control
	EventTypeContextCompacted EventType = "context_compacted"
	EventTypeSteeringApplied  EventType = "steering_applied"
	EventTypeFinish           EventType = "finish"
	EventTypeError            EventType = "error"
)

type Event struct {
	Type  EventType `json:"type"`
	Index int       `json:"index"`

	ContentPartDelta *ContentPartDelta `json:"content_part_delta,omitempty"`
	ToolCallDelta    *ToolCallDelta    `json:"tool_call_delta,omitempty"`

	ContentPart       *ContentPart       `json:"content_part,omitempty"`
	ToolCall          *ToolCall          `json:"tool_call,omitempty"`
	ToolResult        *ToolResult        `json:"tool_result,omitempty"`
	Subagent          *SubagentEvent     `json:"subagent,omitempty"`
	ContextCompaction *ContextCompaction `json:"context_compaction,omitempty"`
	SteeringIDs       []string           `json:"steering_ids,omitempty"`
	// SteeringAck keeps steering out of Agent history until the host has
	// durably established its display boundary.
	SteeringAck chan error `json:"-"`

	FinishReason FinishReason `json:"finish_reason,omitempty"`
	Error        error        `json:"error,omitempty"`
	Usage        *Usage       `json:"usage,omitempty"`
}

// SubagentEvent associates one child-agent event with its parent spawn call.
// Child agents cannot spawn, so nested Subagent events are not supported.
type SubagentEvent struct {
	ToolCallID  string `json:"tool_call_id"`
	Name        string `json:"name,omitempty"`
	AgentType   string `json:"agent_type,omitempty"`
	WorkDir     string `json:"workdir,omitempty"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status,omitempty"`
	Event       *Event `json:"event,omitempty"`
}

type ContextCompaction struct {
	OriginalTokens   int `json:"original_tokens"`
	CompressedTokens int `json:"compressed_tokens"`
	MessagesBefore   int `json:"messages_before"`
	MessagesAfter    int `json:"messages_after"`
}

type ContentPartDelta struct {
	Type ContentType `json:"type,omitempty"` // empty defaults to text; set to ContentTypeReasoning for thinking blocks
	Text string      `json:"text,omitempty"`
}

type ToolCallDeltaType string

const (
	ToolCallDeltaTypeID   ToolCallDeltaType = "id"
	ToolCallDeltaTypeName ToolCallDeltaType = "name"
	ToolCallDeltaTypeArgs ToolCallDeltaType = "arguments"
)

type ToolCallDelta struct {
	Type      ToolCallDeltaType `json:"type"`
	ID        string            `json:"id,omitempty"`        // populated if Type == id
	Name      string            `json:"name,omitempty"`      // populated if Type == name
	Arguments string            `json:"arguments,omitempty"` // populated if Type == arguments
}

// Helpers

func NewErrorEvent(err error) Event {
	return Event{Type: EventTypeError, Error: err}
}

func NewTextDeltaEvent(text string) Event {
	return Event{
		Type:             EventTypeContentDelta,
		ContentPartDelta: &ContentPartDelta{Text: text},
	}
}

func NewReasoningDeltaEvent(text string) Event {
	return Event{
		Type:             EventTypeContentDelta,
		ContentPartDelta: &ContentPartDelta{Type: ContentTypeReasoning, Text: text},
	}
}

func NewFinishEvent(reason FinishReason) Event {
	return Event{Type: EventTypeFinish, FinishReason: reason}
}
