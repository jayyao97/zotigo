package wire

import (
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
)

// Kinds of Command, carried in Command.Type.
const (
	CommandMessage        = "message"
	CommandPause          = "pause"
	CommandSteering       = "steering"
	CommandProfile        = "profile"
	CommandApprovalPolicy = "approval_policy"
)

const (
	// UserPauseReason is the PauseCommandPayload.Reason of a pause the user asked for.
	UserPauseReason = "user_pause"
	// MaxCommandsLimit is the largest page of commands a worker may request
	// and the daemon will return.
	MaxCommandsLimit = 200
)

type CommandsPage struct {
	Commands   []Command `json:"commands"`
	NextCursor string    `json:"next_cursor"`
	NextOffset int64     `json:"next_offset,omitempty"`
}

type Command struct {
	ID             string                        `json:"id"`
	Sequence       uint64                        `json:"sequence"`
	Type           string                        `json:"type"`
	CreatedAt      time.Time                     `json:"created_at"`
	Message        *MessageCommandPayload        `json:"message,omitempty"`
	Steering       *SteeringCommandPayload       `json:"steering,omitempty"`
	Pause          *PauseCommandPayload          `json:"pause,omitempty"`
	Profile        *ProfileCommandPayload        `json:"profile,omitempty"`
	ApprovalPolicy *ApprovalPolicyCommandPayload `json:"approval_policy,omitempty"`
}

type MessageCommandPayload struct {
	Text           string                   `json:"text"`
	Images         []CommandImageData       `json:"images,omitempty"`
	Skills         []string                 `json:"skills,omitempty"`
	RequestContext *protocol.RequestContext `json:"request_context,omitempty"`
}

type SteeringCommandPayload struct {
	Text           string                   `json:"text"`
	Images         []CommandImageData       `json:"images,omitempty"`
	Skills         []string                 `json:"skills,omitempty"`
	TurnID         string                   `json:"turn_id,omitempty"`
	RequestContext *protocol.RequestContext `json:"request_context,omitempty"`
}

type PauseCommandPayload struct {
	TurnID string `json:"turn_id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type ProfileCommandPayload struct {
	RequireIdle bool   `json:"require_idle,omitempty"`
	Name        string `json:"name"`
}

type ApprovalPolicyCommandPayload struct {
	Policy agent.ApprovalPolicy `json:"policy"`
}

type CommandImageData struct {
	MimeType   string `json:"mime_type,omitempty"`
	SizeBytes  int    `json:"size_bytes,omitempty"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	DataBase64 string `json:"data_base64,omitempty"`
	BlobPath   string `json:"blob_path,omitempty"`
}
