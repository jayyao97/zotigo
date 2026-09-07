package zotigod

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
)

const (
	interactionKindUserInput  = "user_input"
	interactionStatusPending  = "pending"
	interactionStatusResolved = "resolved"
	interactionStatusExpired  = "expired"
)

var errInvalidInteraction = errors.New("invalid interaction")

type interactionRequester struct {
	Agent    string `json:"agent,omitempty"`
	ThreadID string `json:"thread_id,omitempty"`
	Name     string `json:"name,omitempty"`
}

type interactionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type interactionQuestion struct {
	ID       string              `json:"id"`
	Header   string              `json:"header,omitempty"`
	Question string              `json:"question"`
	IsOther  bool                `json:"is_other,omitempty"`
	IsSecret bool                `json:"is_secret,omitempty"`
	Options  []interactionOption `json:"options,omitempty"`
}

type interactionRequest struct {
	ID            string                `json:"id"`
	SessionID     string                `json:"session_id"`
	TurnID        string                `json:"turn_id"`
	ItemID        string                `json:"item_id,omitempty"`
	Kind          string                `json:"kind"`
	Status        string                `json:"status"`
	Requester     interactionRequester  `json:"requester"`
	Questions     []interactionQuestion `json:"questions,omitempty"`
	Answers       map[string][]string   `json:"answers,omitempty"`
	IsBlocking    bool                  `json:"is_blocking,omitempty"`
	AutoResolveMS *uint64               `json:"auto_resolve_ms,omitempty"`
	CreatedAt     time.Time             `json:"created_at"`
	ResolvedAt    *time.Time            `json:"resolved_at,omitempty"`
}

func newUserInputInteraction(sessionID, turnID, itemID string, requester interactionRequester, questions []interactionQuestion, blocking bool, autoResolveMS *uint64) (interactionRequest, error) {
	req := interactionRequest{
		ID: newZotigodID("interaction"), SessionID: strings.TrimSpace(sessionID),
		TurnID: strings.TrimSpace(turnID), ItemID: strings.TrimSpace(itemID),
		Kind: interactionKindUserInput, Status: interactionStatusPending,
		Requester: requester, Questions: questions, IsBlocking: blocking,
		AutoResolveMS: autoResolveMS, CreatedAt: time.Now().UTC(),
	}
	if err := validateInteraction(req); err != nil {
		return interactionRequest{}, err
	}
	return req, nil
}

func validateInteraction(req interactionRequest) error {
	if req.SessionID == "" || req.TurnID == "" || req.ID == "" {
		return fmt.Errorf("%w: id, session_id, and turn_id are required", errInvalidInteraction)
	}
	if req.Kind != interactionKindUserInput || len(req.Questions) == 0 {
		return fmt.Errorf("%w: user input questions are required", errInvalidInteraction)
	}
	seen := make(map[string]struct{}, len(req.Questions))
	for _, question := range req.Questions {
		if strings.TrimSpace(question.ID) == "" || strings.TrimSpace(question.Question) == "" {
			return fmt.Errorf("%w: question id and text are required", errInvalidInteraction)
		}
		if _, ok := seen[question.ID]; ok {
			return fmt.Errorf("%w: duplicate question id %q", errInvalidInteraction, question.ID)
		}
		seen[question.ID] = struct{}{}
	}
	return nil
}

func validateInteractionAnswers(req interactionRequest, answers map[string][]string) error {
	if len(answers) != len(req.Questions) {
		return fmt.Errorf("%w: every question requires an answer", errInvalidInteraction)
	}
	for _, question := range req.Questions {
		values, ok := answers[question.ID]
		if !ok {
			return fmt.Errorf("%w: question %q requires an answer", errInvalidInteraction, question.ID)
		}
		if len(values) == 0 {
			continue
		}
		if len(values) == 2 && validInteractionUserNote(values[1]) {
			if hasInteractionOption(question.Options, values[0]) || (question.IsOther && values[0] == "None of the above") {
				continue
			}
		}
		if len(values) != 1 {
			return fmt.Errorf("%w: question %q contains invalid answers", errInvalidInteraction, question.ID)
		}
		value := values[0]
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: question %q contains an empty answer", errInvalidInteraction, question.ID)
		}
		if len(question.Options) == 0 {
			if !validInteractionUserNote(value) {
				return fmt.Errorf("%w: question %q requires a user note", errInvalidInteraction, question.ID)
			}
			continue
		}
		if hasInteractionOption(question.Options, value) {
			continue
		}
		if !question.IsOther || !validInteractionUserNote(value) {
			return fmt.Errorf("%w: question %q contains an unknown option", errInvalidInteraction, question.ID)
		}
	}
	return nil
}

func validInteractionUserNote(answer string) bool {
	const prefix = "user_note:"
	return strings.HasPrefix(answer, prefix) && strings.TrimSpace(strings.TrimPrefix(answer, prefix)) != ""
}

func hasInteractionOption(options []interactionOption, answer string) bool {
	for _, option := range options {
		if option.Label == answer {
			return true
		}
	}
	return false
}

func interactionDisplayItem(req interactionRequest, resolved bool) zotigosession.DisplayItem {
	itemType := zotigosession.DisplayItemInteractionRequest
	if resolved {
		itemType = zotigosession.DisplayItemInteractionResponse
	}
	questions := make([]zotigosession.DisplayInteractionQuestion, 0, len(req.Questions))
	for _, question := range req.Questions {
		options := make([]zotigosession.DisplayInteractionOption, 0, len(question.Options))
		for _, option := range question.Options {
			options = append(options, zotigosession.DisplayInteractionOption{Label: option.Label, Description: option.Description})
		}
		questions = append(questions, zotigosession.DisplayInteractionQuestion{
			ID: question.ID, Header: question.Header, Question: question.Question,
			IsOther: question.IsOther, IsSecret: question.IsSecret, Options: options,
		})
	}
	return zotigosession.DisplayItem{
		Type: itemType,
		Turn: &zotigosession.DisplayTurn{ID: req.TurnID},
		Interaction: &zotigosession.DisplayInteraction{
			ID: req.ID, Kind: req.Kind, Status: req.Status, TurnID: req.TurnID, ItemID: req.ItemID,
			Requester: &zotigosession.DisplayInteractionRequester{Agent: req.Requester.Agent, ThreadID: req.Requester.ThreadID, Name: req.Requester.Name},
			Questions: questions, Answers: displayInteractionAnswers(req), IsBlocking: req.IsBlocking, AutoResolveMS: req.AutoResolveMS,
		},
	}
}

func displayInteractionAnswers(req interactionRequest) map[string][]string {
	if len(req.Answers) == 0 {
		return nil
	}
	answers := make(map[string][]string, len(req.Answers))
	secret := make(map[string]bool, len(req.Questions))
	for _, question := range req.Questions {
		secret[question.ID] = question.IsSecret
	}
	for questionID, values := range req.Answers {
		if secret[questionID] {
			answers[questionID] = []string{"[redacted]"}
			continue
		}
		answers[questionID] = append([]string(nil), values...)
	}
	return answers
}

func interactionFromDisplayItems(sessionID, interactionID string, items []zotigosession.DisplayItem) (interactionRequest, bool) {
	var req interactionRequest
	for _, item := range items {
		value := item.Interaction
		if value == nil || value.ID != interactionID {
			continue
		}
		if item.Type == zotigosession.DisplayItemInteractionRequest {
			req = interactionRequestFromDisplay(sessionID, item)
		}
		if item.Type == zotigosession.DisplayItemInteractionResponse && req.ID != "" {
			req.Status = value.Status
			if req.Status == "" {
				req.Status = interactionStatusResolved
			}
			req.Answers = value.Answers
			resolvedAt := item.CreatedAt
			req.ResolvedAt = &resolvedAt
		}
	}
	return req, req.ID != ""
}

func interactionRequestFromDisplay(sessionID string, item zotigosession.DisplayItem) interactionRequest {
	value := item.Interaction
	questions := make([]interactionQuestion, 0, len(value.Questions))
	for _, question := range value.Questions {
		options := make([]interactionOption, 0, len(question.Options))
		for _, option := range question.Options {
			options = append(options, interactionOption{Label: option.Label, Description: option.Description})
		}
		questions = append(questions, interactionQuestion{ID: question.ID, Header: question.Header, Question: question.Question, IsOther: question.IsOther, IsSecret: question.IsSecret, Options: options})
	}
	requester := interactionRequester{}
	if value.Requester != nil {
		requester = interactionRequester{Agent: value.Requester.Agent, ThreadID: value.Requester.ThreadID, Name: value.Requester.Name}
	}
	return interactionRequest{ID: value.ID, SessionID: sessionID, TurnID: value.TurnID, ItemID: value.ItemID, Kind: value.Kind, Status: interactionStatusPending, Requester: requester, Questions: questions, IsBlocking: value.IsBlocking, AutoResolveMS: value.AutoResolveMS, CreatedAt: item.CreatedAt}
}

func (h *handler) expireDisconnectedCodexRequests(ctx context.Context, sessionID string) error {
	items, exists, err := h.items.LoadItems(ctx, sessionID)
	if err != nil || !exists {
		return err
	}
	pendingApprovals := make(map[string]approvalRequest)
	pendingInteractions := make(map[string]interactionRequest)
	for _, item := range items {
		if item.Approval != nil && item.Approval.ID != "" {
			switch item.Type {
			case zotigosession.DisplayItemApprovalRequest:
				req, ok := approvalFromDisplayItems(sessionID, item.Approval.ID, []zotigosession.DisplayItem{item})
				if ok && isCodexApproval(req) {
					pendingApprovals[req.ID] = req
				}
			case zotigosession.DisplayItemApprovalDecision:
				delete(pendingApprovals, item.Approval.ID)
			}
		}
		if item.Interaction != nil && item.Interaction.ID != "" {
			switch item.Type {
			case zotigosession.DisplayItemInteractionRequest:
				req := interactionRequestFromDisplay(sessionID, item)
				if req.Requester.Agent == "codex" {
					pendingInteractions[req.ID] = req
				}
			case zotigosession.DisplayItemInteractionResponse:
				delete(pendingInteractions, item.Interaction.ID)
			}
		}
	}
	now := time.Now().UTC()
	for _, req := range pendingApprovals {
		decisions := make([]zotigosession.DisplayApprovalDecision, 0, len(req.Pending))
		for _, pending := range req.Pending {
			decisions = append(decisions, zotigosession.DisplayApprovalDecision{
				ToolCallID: pending.ToolCallID, Approved: false, Reason: "Codex request expired after worker disconnect",
			})
		}
		if _, err := h.items.AppendItem(ctx, sessionID, zotigosession.DisplayItem{
			Type: zotigosession.DisplayItemApprovalDecision, Turn: &zotigosession.DisplayTurn{ID: req.TurnID},
			Approval: &zotigosession.DisplayApproval{ID: req.ID, TurnID: req.TurnID, Decisions: decisions},
		}); err != nil {
			return err
		}
	}
	for _, req := range pendingInteractions {
		req.Status = interactionStatusExpired
		req.ResolvedAt = &now
		if _, err := h.items.AppendItem(ctx, sessionID, interactionDisplayItem(req, true)); err != nil {
			return err
		}
	}
	return nil
}

func isCodexApproval(req approvalRequest) bool {
	if len(req.Pending) == 0 {
		return false
	}
	for _, pending := range req.Pending {
		if !strings.HasPrefix(pending.Source, "codex") {
			return false
		}
	}
	return true
}
