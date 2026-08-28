package hooks

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

type EventName string

const (
	SessionStart EventName = "SessionStart"
	SessionEnd   EventName = "SessionEnd"
	PreToolUse   EventName = "PreToolUse"
	PostToolUse  EventName = "PostToolUse"
)

func ParseEventName(value string) (EventName, bool) {
	name := EventName(strings.TrimSpace(value))
	switch name {
	case SessionStart, SessionEnd, PreToolUse, PostToolUse:
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
	Tool          *ToolPayload    `json:"tool,omitempty"`
}

type SessionPayload struct {
	Source    string `json:"source,omitempty"`
	Result    string `json:"result,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
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
