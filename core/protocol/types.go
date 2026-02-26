package protocol

// Role represents the role of the message sender.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ContentType discriminates the type of content in a message part.
type ContentType string

const (
	ContentTypeText       ContentType = "text"
	ContentTypeReasoning  ContentType = "reasoning" // For CoT/Thinking models
	ContentTypeImage      ContentType = "image"
	ContentTypeAudio      ContentType = "audio"
	ContentTypeVideo      ContentType = "video"
	ContentTypeFile       ContentType = "file"        // Generic file (PDF, etc.)
	ContentTypeToolCall   ContentType = "tool_call"   // Request to call a tool
	ContentTypeToolResult ContentType = "tool_result" // Result of a tool call
)

// ToolResultType discriminates the structure of the tool result.
type ToolResultType string

const (
	ToolResultTypeText            ToolResultType = "text"
	ToolResultTypeJSON            ToolResultType = "json"
	ToolResultTypeErrorText       ToolResultType = "error-text"
	ToolResultTypeErrorJSON       ToolResultType = "error-json"
	ToolResultTypeExecutionDenied ToolResultType = "execution-denied"
	ToolResultTypeContent         ToolResultType = "content" // Complex multi-modal
)

// FinishReason indicates why the generation stopped.
type FinishReason string

const (
	FinishReasonStop          FinishReason = "stop"
	FinishReasonLength        FinishReason = "length"
	FinishReasonToolCalls     FinishReason = "tool_calls"
	FinishReasonContentFilter FinishReason = "content_filter"
	FinishReasonError         FinishReason = "error"
	FinishReasonUnknown       FinishReason = "unknown"
)
