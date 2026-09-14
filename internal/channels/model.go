package channels

import (
	"context"
	"errors"
	"time"
)

const ProviderFeishu = "feishu"

const (
	ActorRoleOwner  = "owner"
	ActorRoleMember = "member"
)

const (
	ProgressModeAuto            = "auto"
	ProgressModeCOT             = "cot"
	ProgressModeInteractiveCard = "interactive_card"
)

type OverrideMode string

const (
	OverrideInherit OverrideMode = "inherit"
	OverrideReplace OverrideMode = "replace"
)

type Connection struct {
	ID                   string    `json:"id"`
	Provider             string    `json:"provider"`
	Name                 string    `json:"name"`
	AppID                string    `json:"app_id"`
	HasSecret            bool      `json:"has_secret"`
	Enabled              bool      `json:"enabled"`
	Status               string    `json:"status"`
	LastError            string    `json:"last_error,omitempty"`
	BotOpenID            string    `json:"bot_open_id,omitempty"`
	BotName              string    `json:"bot_name,omitempty"`
	AllowChatIDs         []string  `json:"allow_chat_ids"`
	OwnerSenderIDs       []string  `json:"owner_sender_ids"`
	AgentInstructions    string    `json:"agent_instructions"`
	ApprovalInstructions string    `json:"approval_instructions"`
	ReviewAllTools       bool      `json:"review_all_tools"`
	ProgressMode         string    `json:"progress_mode"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
	COTAvailable         bool      `json:"cot_available"`
}

type ConnectionInput struct {
	Provider             string    `json:"provider"`
	Name                 string    `json:"name"`
	AppID                string    `json:"app_id"`
	AppSecret            *string   `json:"app_secret,omitempty"`
	ClearSecret          bool      `json:"clear_secret,omitempty"`
	Enabled              bool      `json:"enabled"`
	AllowChatIDs         []string  `json:"allow_chat_ids"`
	OwnerSenderIDs       *[]string `json:"owner_sender_ids,omitempty"`
	AgentInstructions    string    `json:"agent_instructions"`
	ApprovalInstructions string    `json:"approval_instructions"`
	ReviewAllTools       bool      `json:"review_all_tools"`
	ProgressMode         string    `json:"progress_mode"`
}

type Sender struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
}

type Conversation struct {
	ID                       string       `json:"id"`
	ConnectionID             string       `json:"connection_id"`
	ChatID                   string       `json:"chat_id"`
	RootID                   string       `json:"root_id,omitempty"`
	ThreadID                 string       `json:"thread_id,omitempty"`
	ChatType                 string       `json:"chat_type"`
	ChatName                 string       `json:"chat_name,omitempty"`
	DisplayName              string       `json:"display_name"`
	SessionID                string       `json:"session_id,omitempty"`
	WorkspaceID              string       `json:"workspace_id,omitempty"`
	Enabled                  bool         `json:"enabled"`
	AllowedSenderIDs         []string     `json:"allowed_sender_ids"`
	ObservedSenders          []Sender     `json:"observed_senders"`
	AgentInstructionsMode    OverrideMode `json:"agent_instructions_mode"`
	AgentInstructions        string       `json:"agent_instructions"`
	ApprovalInstructionsMode OverrideMode `json:"approval_instructions_mode"`
	ApprovalInstructions     string       `json:"approval_instructions"`
	ReviewAllTools           *bool        `json:"review_all_tools,omitempty"`
	LastActivityAt           time.Time    `json:"last_activity_at"`
	CreatedAt                time.Time    `json:"created_at"`
	UpdatedAt                time.Time    `json:"updated_at"`
}

type ConversationInput struct {
	DisplayName              string       `json:"display_name"`
	SessionID                string       `json:"session_id"`
	WorkspaceID              string       `json:"workspace_id"`
	Enabled                  bool         `json:"enabled"`
	AllowedSenderIDs         []string     `json:"allowed_sender_ids"`
	AgentInstructionsMode    OverrideMode `json:"agent_instructions_mode"`
	AgentInstructions        string       `json:"agent_instructions"`
	ApprovalInstructionsMode OverrideMode `json:"approval_instructions_mode"`
	ApprovalInstructions     string       `json:"approval_instructions"`
	ReviewAllTools           *bool        `json:"review_all_tools,omitempty"`
}

// Group is a group chat currently visible to one channel connection. The
// rootless Conversation with the same chat_id owns its Workspace binding and
// policy; rooted Conversations are the Sessions created from individual
// Feishu topics.
type Group struct {
	ChatID           string   `json:"chat_id"`
	Name             string   `json:"name"`
	Avatar           string   `json:"avatar,omitempty"`
	Description      string   `json:"description,omitempty"`
	External         bool     `json:"external"`
	Available        bool     `json:"available"`
	ConversationID   string   `json:"conversation_id,omitempty"`
	WorkspaceID      string   `json:"workspace_id,omitempty"`
	Enabled          bool     `json:"enabled"`
	AllowedSenderIDs []string `json:"allowed_sender_ids"`
}

type Message struct {
	ID             string    `json:"id"`
	ConnectionID   string    `json:"connection_id"`
	ConversationID string    `json:"conversation_id"`
	ProviderID     string    `json:"provider_message_id"`
	Sender         Sender    `json:"sender"`
	Text           string    `json:"text"`
	MentionedBot   bool      `json:"mentioned_bot"`
	TriggerStatus  string    `json:"trigger_status"`
	StatusDetail   string    `json:"status_detail,omitempty"`
	ReplyMessageID string    `json:"reply_message_id,omitempty"`
	DeliveryMode   string    `json:"delivery_mode,omitempty"`
	COTID          string    `json:"cot_id,omitempty"`
	FinalMessageID string    `json:"final_message_id,omitempty"`
	ProjectedSeq   uint64    `json:"projected_sequence,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type InboundMessage struct {
	EventID            string
	MessageID          string
	ChatID             string
	ChatType           string
	ChatName           string
	RootID             string
	ThreadID           string
	ConversationName   string
	Sender             Sender
	Text               string
	MentionedBot       bool
	ConversationKey    string
	StartsConversation bool
	TriggerAllowed     bool
	CreatedAt          time.Time
}

// SessionPromptConfig is the fully resolved prompt policy captured when a
// channel conversation is bound to a Session.
type SessionPromptConfig struct {
	AgentInstructions    string
	ApprovalInstructions string
	ReviewAllTools       bool
}

type Task struct {
	ConnectionID         string
	ConversationID       string
	SessionID            string
	MessageID            string
	Text                 string
	Origin               TaskOrigin
	AgentInstructions    string
	ApprovalInstructions string
	ReviewAllTools       bool
}

type TaskOrigin struct {
	Provider             string
	ConnectionName       string
	ConversationID       string
	ConversationName     string
	ConversationType     string
	ExternalConversation string
	ExternalRootID       string
	ExternalThreadID     string
	ExternalMessageID    string
	Sender               Sender
	ActorRole            string
}

type Progress struct {
	State    string
	Text     string
	Sequence uint64
	Events   []PublicExecutionEvent
}

type PublicExecutionEvent struct {
	ID            string
	CorrelationID string
	Type          string
	Timestamp     time.Time
	Tool          *PublicToolActivity
}

type PublicToolActivity struct {
	CallID      string
	DisplayName string
	Kind        string
	Status      string
}

const (
	ExecutionRunStarted   = "run_started"
	ExecutionToolStarted  = "tool_started"
	ExecutionToolFinished = "tool_finished"
	ExecutionApproval     = "approval_waiting"
	ExecutionApprovalDone = "approval_finished"
)

type DeliveryReceipt struct {
	Mode      string
	MessageID string
	COTID     string
}

type uncertainDeliveryError struct{ err error }

func (e *uncertainDeliveryError) Error() string { return e.err.Error() }
func (e *uncertainDeliveryError) Unwrap() error { return e.err }

func UncertainDelivery(err error) error {
	if err == nil || IsUncertainDelivery(err) {
		return err
	}
	return &uncertainDeliveryError{err: err}
}

func IsUncertainDelivery(err error) bool {
	var target *uncertainDeliveryError
	return errors.As(err, &target)
}

type TaskResult struct {
	Text string
}

type TaskDispatcher interface {
	EnsureChannelSessionPrompt(context.Context, string, SessionPromptConfig) (SessionPromptConfig, error)
	DispatchChannelTask(ctx context.Context, task Task, progress func(Progress), admissionComplete func()) (TaskResult, error)
}

type SessionProvisioner interface {
	ProvisionChannelSession(ctx context.Context, workspaceID, title string, prompt SessionPromptConfig, bind func(sessionID string) error) error
}

// TaskController handles channel commands that control the bound session
// instead of starting a new agent turn.
type TaskController interface {
	StopChannelTask(ctx context.Context, task Task, progress func(Progress), admissionComplete func()) (TaskResult, error)
}
