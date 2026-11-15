package wire

import (
	"encoding/json"

	"github.com/jayyao97/zotigo/core/protocol"
)

// MessageType is the kind of a WebSocket frame exchanged between the daemon
// and a worker. It selects which payload field of Message is set.
type MessageType string

const (
	MessageCommand                 MessageType = "command"
	MessageWorking                 MessageType = "working"
	MessageDelta                   MessageType = "display_delta"
	MessageDisplayWake             MessageType = "display_wake"
	MessageDisplayBarrier          MessageType = "display_barrier"
	MessageDisplayBarrierOK        MessageType = "display_barrier_ack"
	MessageApprovalRequest         MessageType = "approval_request"
	MessageApprovalDecision        MessageType = "approval_decision"
	MessageApprovalResult          MessageType = "approval_result"
	MessageInteractionRequest      MessageType = "interaction_request"
	MessageInteractionResponse     MessageType = "interaction_response"
	MessageInteractionResult       MessageType = "interaction_result"
	MessageConversationBound       MessageType = "conversation_bound"
	MessageConversationBoundResult MessageType = "conversation_bound_result"
	MessageInputRequest            MessageType = "input_request"
	MessageInputResult             MessageType = "input_result"
	MessageInterruptTurn           MessageType = "interrupt_turn"
	MessageIdle                    MessageType = "idle"
	MessageRuntimeToolRequest      MessageType = "runtime_tool_request"
	MessageRuntimeToolResult       MessageType = "runtime_tool_result"
)

// Message is the single frame type of the daemon↔worker WebSocket. Type names
// the kind; at most the payload fields that belong to that kind are set.
type Message struct {
	Type                    MessageType              `json:"type"`
	Command                 *Command                 `json:"command,omitempty"`
	PreviewRetirement       *PreviewRetirement       `json:"preview_retirement,omitempty"`
	Delta                   *DisplayDelta            `json:"delta,omitempty"`
	DisplayBarrier          *DisplayBarrier          `json:"display_barrier,omitempty"`
	ApprovalRequest         *ApprovalRequest         `json:"approval_request,omitempty"`
	ApprovalDecision        *ApprovalDecision        `json:"approval_decision,omitempty"`
	ApprovalResult          *ApprovalResult          `json:"approval_result,omitempty"`
	InteractionRequest      *InteractionRequest      `json:"interaction_request,omitempty"`
	InteractionResponse     *InteractionResponse     `json:"interaction_response,omitempty"`
	InteractionResult       *InteractionResult       `json:"interaction_result,omitempty"`
	ConversationBound       *ConversationBound       `json:"conversation_bound,omitempty"`
	ConversationBoundResult *ConversationBoundResult `json:"conversation_bound_result,omitempty"`
	InputRequest            *InputRequest            `json:"input_request,omitempty"`
	InputResult             *InputResult             `json:"input_result,omitempty"`
	InterruptTurn           *InterruptTurn           `json:"interrupt_turn,omitempty"`
	Idle                    *Idle                    `json:"idle,omitempty"`
	RuntimeToolRequest      *RuntimeToolRequest      `json:"runtime_tool_request,omitempty"`
	RuntimeToolResult       *RuntimeToolResult       `json:"runtime_tool_result,omitempty"`
}

type Idle struct {
	CommandSequence uint64 `json:"command_sequence"`
}

type InterruptTurn struct {
	ThreadID string `json:"thread_id"`
	TurnID   string `json:"turn_id"`
}

type DisplayBarrier struct {
	ID string `json:"id"`
}

type ConversationBound struct {
	ConversationID string `json:"conversation_id"`
}

type ConversationBoundResult struct {
	ConversationID string `json:"conversation_id"`
	ErrorCode      string `json:"error_code,omitempty"`
	Error          string `json:"error,omitempty"`
}

type RuntimeToolRequest struct {
	RequestID      string                   `json:"request_id"`
	Namespace      string                   `json:"namespace,omitempty"`
	TurnID         string                   `json:"turn_id,omitempty"`
	CallID         string                   `json:"call_id,omitempty"`
	Name           string                   `json:"name"`
	Arguments      json.RawMessage          `json:"arguments,omitempty"`
	RequestContext *protocol.RequestContext `json:"request_context,omitempty"`
}

type RuntimeToolResult struct {
	RequestID string `json:"request_id"`
	Text      string `json:"text,omitempty"`
	Error     string `json:"error,omitempty"`
}
