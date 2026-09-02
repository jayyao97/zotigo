package hooks

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

type EventName string

const (
	SessionStart     EventName = "SessionStart"
	SessionEnd       EventName = "SessionEnd"
	TurnStart        EventName = "TurnStart"
	UserPromptSubmit EventName = "UserPromptSubmit"
	TurnEnd          EventName = "TurnEnd"
	PreToolUse       EventName = "PreToolUse"
	PostToolUse      EventName = "PostToolUse"
)

func ParseEventName(value string) (EventName, bool) {
	name := EventName(strings.TrimSpace(value))
	switch name {
	case SessionStart, SessionEnd, TurnStart, UserPromptSubmit, TurnEnd, PreToolUse, PostToolUse:
		return name, true
	default:
		return "", false
	}
}

type Event struct {
	SchemaVersion int             `json:"schema_version"`
	EventID       string          `json:"event_id"`
	EventName     EventName       `json:"event_name"`
	OccurredAt    time.Time       `json:"occurred_at"`
	SessionID     string          `json:"session_id"`
	TurnID        string          `json:"turn_id,omitempty"`
	Agent         string          `json:"agent"`
	CWD           string          `json:"cwd"`
	Session       *SessionPayload `json:"session,omitempty"`
	Turn          *TurnPayload    `json:"turn,omitempty"`
	Prompt        *PromptPayload  `json:"prompt,omitempty"`
	Tool          *ToolPayload    `json:"tool,omitempty"`
}

type SessionPayload struct {
	Source    string        `json:"source,omitempty"`
	Result    string        `json:"result,omitempty"`
	ErrorCode string        `json:"error_code,omitempty"`
	Model     string        `json:"model,omitempty"`
	Usage     *UsagePayload `json:"usage,omitempty"`
}

type UsagePayload struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	TotalTokens              int `json:"total_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

type TurnPayload struct {
	Status string `json:"status,omitempty"`
	// Model is the model selected when the turn started.
	Model string        `json:"model,omitempty"`
	Usage *UsagePayload `json:"usage,omitempty"`
}

type PromptPayload struct {
	Text string `json:"text"`
}

type ToolPayload struct {
	CallID     string             `json:"call_id"`
	Name       string             `json:"name"`
	NativeName string             `json:"native_name"`
	Input      any                `json:"input"`
	Result     *ToolResultPayload `json:"result,omitempty"`
}

type ToolResultPayload struct {
	Status  string `json:"status"`
	Summary string `json:"summary,omitempty"`
}

func NewEvent(name EventName, sessionID string, agentName string, cwd string) Event {
	return Event{
		SchemaVersion: ConfigVersion,
		EventID:       "hookevt_" + uuid.NewString(),
		EventName:     name,
		OccurredAt:    time.Now().UTC(),
		SessionID:     sessionID,
		Agent:         agentName,
		CWD:           cwd,
	}
}

func (e Event) matcherValue() (string, bool) {
	switch e.EventName {
	case PreToolUse, PostToolUse:
		if e.Tool != nil {
			return e.Tool.Name, true
		}
	case SessionStart:
		if e.Session != nil {
			return e.Session.Source, true
		}
	case SessionEnd:
		if e.Session != nil {
			return e.Session.Result, true
		}
	case TurnStart, TurnEnd:
		if e.Turn != nil {
			return e.Turn.Status, true
		}
	}
	return "", false
}

func DecodeToolInput(arguments string) any {
	arguments = strings.TrimSpace(arguments)
	if arguments == "" {
		return map[string]any{}
	}
	var input any
	if err := json.Unmarshal([]byte(arguments), &input); err == nil {
		return input
	}
	return arguments
}
