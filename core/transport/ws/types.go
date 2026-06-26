package ws

import (
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/transport"
)

type MessageType string

const (
	MessageTypeEvent           MessageType = "event"
	MessageTypeInput           MessageType = "input"
	MessageTypeApprovalRequest MessageType = "approval_request"
	MessageTypeApprovalResult  MessageType = "approval_result"
	MessageTypeClose           MessageType = "close"
)

type Message struct {
	Type MessageType `json:"type"`
	ID   string      `json:"id,omitempty"`

	Event     *protocol.Event             `json:"event,omitempty"`
	Input     *transport.UserInput        `json:"input,omitempty"`
	Pending   []transport.PendingToolCall `json:"pending,omitempty"`
	Approvals []transport.ApprovalResult  `json:"approvals,omitempty"`
	Error     string                      `json:"error,omitempty"`
}
