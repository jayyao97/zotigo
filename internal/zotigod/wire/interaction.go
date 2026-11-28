package wire

import "time"

// Interaction payloads: questions a worker asks the user through the daemon,
// the answers relayed back, and the worker's acknowledgement.

type InteractionRequester struct {
	Agent    string `json:"agent,omitempty"`
	ThreadID string `json:"thread_id,omitempty"`
	Name     string `json:"name,omitempty"`
}

type InteractionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type InteractionQuestion struct {
	ID       string              `json:"id"`
	Header   string              `json:"header,omitempty"`
	Question string              `json:"question"`
	IsOther  bool                `json:"is_other,omitempty"`
	IsSecret bool                `json:"is_secret,omitempty"`
	Options  []InteractionOption `json:"options,omitempty"`
}

type InteractionRequest struct {
	ReplyAsMessage bool                  `json:"reply_as_message,omitempty"`
	ID             string                `json:"id"`
	SessionID      string                `json:"session_id"`
	TurnID         string                `json:"turn_id"`
	ItemID         string                `json:"item_id,omitempty"`
	Kind           string                `json:"kind"`
	Status         string                `json:"status"`
	Requester      InteractionRequester  `json:"requester"`
	Questions      []InteractionQuestion `json:"questions,omitempty"`
	Answers        map[string][]string   `json:"answers,omitempty"`
	IsBlocking     bool                  `json:"is_blocking,omitempty"`
	AutoResolveMS  *uint64               `json:"auto_resolve_ms,omitempty"`
	CreatedAt      time.Time             `json:"created_at"`
	ResolvedAt     *time.Time            `json:"resolved_at,omitempty"`
}

type InteractionResponse struct {
	RequestID     string              `json:"request_id"`
	InteractionID string              `json:"interaction_id"`
	Answers       map[string][]string `json:"answers"`
}

type InteractionResult struct {
	RequestID   string              `json:"request_id"`
	Interaction *InteractionRequest `json:"interaction,omitempty"`
	Error       string              `json:"error,omitempty"`
}
