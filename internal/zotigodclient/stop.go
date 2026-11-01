package zotigodclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/jayyao97/zotigo/core/session"
)

// TurnControl is the part of the service API needed to stop a turn.
type TurnControl interface {
	Items(ctx context.Context, id string, after uint64, before uint64, limit int) (ItemsPage, error)
	Pause(ctx context.Context, id string, turnID string) (Command, error)
	DecideApproval(ctx context.Context, id string, approvalID string, decisions []ApprovalDecision, stopTurn bool) error
}

// StopTarget identifies the turn a client wants to stop. A client's view of
// the session lags the service: input may still be in flight, or its turn
// may have started without the client having seen turn_started yet.
type StopTarget struct {
	// TurnID is the turn the client saw running, if any.
	TurnID string
	// InputIDs are client message ids this client submitted whose turn
	// should stop once it is known.
	InputIDs []string
	// Submissions reports the outcome of each input's submission. Nil
	// means every input was accepted.
	Submissions *Submissions
	// AnyOpenTurn stops whatever turn is open when no other target is
	// known (used by an embedded service that is about to exit).
	AnyOpenTurn bool
}

// SubmissionState is what a client knows about one submitted input.
type SubmissionState int

const (
	// SubmissionAccepted means the service may have recorded the input:
	// it returned success, or the outcome is unknown (a transport error
	// or 5xx after the request was sent).
	SubmissionAccepted SubmissionState = iota
	// SubmissionInFlight means the request has not returned yet.
	SubmissionInFlight
	// SubmissionRejected means the service refused the input with a 4xx,
	// so it never started or joined a turn.
	SubmissionRejected
)

// Submissions tracks message submissions so a stop can tell an input that
// was never accepted from one the client has not seen recorded yet.
type Submissions struct {
	mu     sync.Mutex
	states map[string]SubmissionState
}

// NewSubmissions returns an empty tracker.
func NewSubmissions() *Submissions {
	return &Submissions{states: make(map[string]SubmissionState)}
}

// Begin marks id as in flight. Call it before sending the request.
func (s *Submissions) Begin(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[id] = SubmissionInFlight
}

// Finish records the request's result. Only a 4xx proves rejection; any
// other error leaves the input possibly accepted.
func (s *Submissions) Finish(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if SubmissionRejectedErr(err) {
		s.states[id] = SubmissionRejected
		return
	}
	s.states[id] = SubmissionAccepted
}

// SubmissionRejectedErr reports whether a submission error proves the
// service refused the input. Transport errors and 5xx responses do not:
// the input may have been recorded before the response was lost.
func SubmissionRejectedErr(err error) bool {
	if errors.Is(err, ErrSubmissionUnconfirmed) {
		return false
	}
	var apiErr *APIError
	return err != nil && errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500
}

// MessageSubmitter is the part of the service API that submits input.
type MessageSubmitter interface {
	SubmitMessage(ctx context.Context, id string, req MessageRequest) (Command, error)
}

// submitRetryDelays bounds the resubmissions of an unconfirmed input to a
// few seconds: enough for a dropped connection, short enough that the
// caller can report the uncertainty while the input may be running.
var submitRetryDelays = []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second}

// ErrSubmissionUnconfirmed wraps a submission error whose outcome is
// unknown: the service may have recorded the input and started its turn.
var ErrSubmissionUnconfirmed = errors.New("could not confirm whether the message was accepted")

// SubmitTracked submits req and records its outcome in subs. Only a 4xx
// on the first attempt proves rejection. When the outcome is unknown it
// resubmits with the same client message id, which the service
// deduplicates, until one attempt succeeds or the retries run out. A retry
// error does not prove rejection (the first request may still be pending
// in the service), so the result then stays unconfirmed: the returned
// error wraps ErrSubmissionUnconfirmed and callers must keep the input as
// a stop target.
func SubmitTracked(ctx context.Context, c MessageSubmitter, sessionID string, req MessageRequest, subs *Submissions) (Command, error) {
	if req.ClientMessageID == "" {
		return Command{}, errors.New("tracked submission requires a client message id")
	}
	subs.Begin(req.ClientMessageID)
	command, err := c.SubmitMessage(ctx, sessionID, req)
	if err == nil || SubmissionRejectedErr(err) {
		subs.Finish(req.ClientMessageID, err)
		return command, err
	}
retry:
	for _, delay := range submitRetryDelays {
		select {
		case <-ctx.Done():
			break retry
		case <-time.After(delay):
		}
		var retryErr error
		command, retryErr = c.SubmitMessage(ctx, sessionID, req)
		switch {
		case retryErr == nil:
			subs.Finish(req.ClientMessageID, nil)
			return command, nil
		case IsCode(retryErr, "command_pending") || !SubmissionRejectedErr(retryErr):
			// Possibly our own first request still being admitted, or
			// the service is still unreachable.
			err = retryErr
		default:
			err = retryErr
			break retry
		}
	}
	unconfirmed := fmt.Errorf("%w: %w", ErrSubmissionUnconfirmed, err)
	subs.Finish(req.ClientMessageID, unconfirmed)
	return Command{}, unconfirmed
}

// Forget drops id once its turn is known.
func (s *Submissions) Forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.states, id)
}

// State reports what is known about id; unknown ids count as accepted.
func (s *Submissions) State(id string) SubmissionState {
	if s == nil {
		return SubmissionAccepted
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if state, ok := s.states[id]; ok {
		return state
	}
	return SubmissionAccepted
}

// StopResult says what StopTurn achieved.
type StopResult int

const (
	// StopNothing means no targeted turn was running or about to run.
	StopNothing StopResult = iota
	// StopRequested means the service accepted a pause, or a denial with
	// stop_turn, for the targeted turn.
	StopRequested
	// StopAlreadyEnded means the targeted turn finished on its own.
	StopAlreadyEnded
)

const (
	stopPollInterval = 150 * time.Millisecond
	stopItemWindow   = 200
)

// StopTurn stops the targeted turn and returns only once the service has
// accepted the stop or the turn is known to have ended; ctx bounds the
// wait, and an unconfirmed stop is an error.
//
// Inputs are resolved before deciding: submission states are read before
// the history, so an input accepted by then is in that history (the service
// answers a submission only after recording it). An input still in flight
// is waited for, a rejected one is ignored, and an accepted one missing
// from the recent window is looked up in older history; one that cannot be
// found keeps the stop waiting rather than reporting that nothing ran.
//
// A lost approval race (409) is not success: the approval may have been
// granted elsewhere, so the session is re-read and the turn paused. Only
// the targeted turn is stopped, never a newer one.
func StopTurn(ctx context.Context, c TurnControl, sessionID string, target StopTarget) (StopResult, error) {
	// Every turn the client saw or its inputs started or joined is stopped
	// as soon as it is known, even while other inputs are still being
	// located; pausing names the turn, so a newer turn is never hit.
	settled := make(map[string]StopResult)
	for {
		inputs := liveInputs(target)
		items, err := stopHistory(ctx, c, sessionID, inputs)
		if err != nil {
			return combineStops(settled), fmt.Errorf("read session: %w", err)
		}
		view := scanStopView(items, inputs)
		candidates := append([]string(nil), view.inputTurns...)
		if target.TurnID != "" {
			candidates = append(candidates, target.TurnID)
		}
		if target.AnyOpenTurn && view.openTurn != "" {
			candidates = append(candidates, view.openTurn)
		}
		pending := ""
		for _, turnID := range candidates {
			if _, ok := settled[turnID]; ok {
				continue
			}
			result, done, err := stopKnownTurn(ctx, c, sessionID, turnID, items, view.openTurn)
			if err != nil {
				return combineStops(settled), err
			}
			if done {
				settled[turnID] = result
			} else {
				pending = turnID
			}
		}
		// An accepted input that is not in the full history cannot be
		// placed yet; never conclude that nothing is running.
		waiting := view.awaitingStart || view.missingInputs
		if !waiting && pending == "" {
			return combineStops(settled), nil
		}
		select {
		case <-ctx.Done():
			if pending != "" {
				return combineStops(settled), fmt.Errorf("timed out stopping turn %s: %w", pending, ctx.Err())
			}
			return combineStops(settled), fmt.Errorf("timed out locating the turn of a submitted input: %w", ctx.Err())
		case <-time.After(stopPollInterval):
		}
	}
}

func combineStops(settled map[string]StopResult) StopResult {
	combined := StopNothing
	for _, result := range settled {
		if result == StopRequested {
			return StopRequested
		}
		if result == StopAlreadyEnded {
			combined = StopAlreadyEnded
		}
	}
	return combined
}

// liveInputs returns the inputs that may have started or joined a turn:
// all but those the service rejected. States are read before the history
// snapshot, so an input missing from it is waited for, never dismissed.
func liveInputs(target StopTarget) map[string]bool {
	inputs := make(map[string]bool, len(target.InputIDs))
	for _, id := range target.InputIDs {
		if target.Submissions.State(id) != SubmissionRejected {
			inputs[id] = true
		}
	}
	return inputs
}

// stopHistory reads the recent window and, while an accepted input is not
// in it, older pages as well.
func stopHistory(ctx context.Context, c TurnControl, sessionID string, inputs map[string]bool) ([]session.DisplayItem, error) {
	page, err := c.Items(ctx, sessionID, 0, 0, stopItemWindow)
	if err != nil {
		return nil, err
	}
	pages := [][]session.DisplayItem{page.Items}
	found := make(map[string]bool, len(inputs))
	markInputs(page.Items, inputs, found)
	for len(found) < len(inputs) && page.PrevCursor != "" && len(page.Items) > 0 {
		page, err = c.Items(ctx, sessionID, 0, page.Items[0].Sequence, stopItemWindow)
		if err != nil {
			return nil, err
		}
		pages = append(pages, page.Items)
		markInputs(page.Items, inputs, found)
	}
	var items []session.DisplayItem
	for index := len(pages) - 1; index >= 0; index-- {
		items = append(items, pages[index]...)
	}
	return items, nil
}

func markInputs(items []session.DisplayItem, inputs map[string]bool, found map[string]bool) {
	for _, item := range items {
		if inputs[item.ID] {
			found[item.ID] = true
		}
	}
}

// stopKnownTurn makes one attempt; done=false means re-read and retry.
func stopKnownTurn(ctx context.Context, c TurnControl, sessionID string, turnID string, items []session.DisplayItem, openTurn string) (StopResult, bool, error) {
	if turnEnded(items, turnID) {
		return StopAlreadyEnded, true, nil
	}
	if approval := pendingApprovalFor(items, turnID); approval != nil {
		decisions := make([]ApprovalDecision, 0, len(approval.Pending))
		for _, pending := range approval.Pending {
			decisions = append(decisions, ApprovalDecision{ToolCallID: pending.ToolCallID, Reason: "User stopped the turn"})
		}
		err := c.DecideApproval(ctx, sessionID, approval.ID, decisions, true)
		switch {
		case err == nil:
			return StopRequested, true, nil
		case IsStatus(err, http.StatusConflict):
			// Resolved elsewhere, possibly approved: the turn may be
			// running again. Re-read and pause it.
			return StopNothing, false, nil
		default:
			return StopNothing, true, fmt.Errorf("deny pending approval: %w", err)
		}
	}
	_, err := c.Pause(ctx, sessionID, turnID)
	switch {
	case err == nil, IsCode(err, "turn_stopping"):
		return StopRequested, true, nil
	case IsCode(err, "turn_mismatch"):
		// Another turn is open, so the targeted one has ended.
		return StopAlreadyEnded, true, nil
	case IsCode(err, "no_active_turn") && openTurn == "":
		// The targeted turn has started (it is known), and nothing is open.
		return StopAlreadyEnded, true, nil
	case IsCode(err, "no_active_turn"), IsCode(err, "approval_pending"):
		// The turn is finishing or an approval became durable meanwhile.
		return StopNothing, false, nil
	default:
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusServiceUnavailable {
			return StopNothing, false, nil
		}
		return StopNothing, true, fmt.Errorf("pause turn %s: %w", turnID, err)
	}
}

type stopView struct {
	openTurn      string
	inputTurns    []string // turns started or joined by the inputs
	awaitingStart bool     // an input is recorded but its turn has not started
	missingInputs bool     // some inputs are not recorded yet
}

// scanStopView places the inputs in the history. A user_message input means
// no turn was open when it was admitted, so its turn is the next
// turn_started; steering names the turn it joined.
func scanStopView(items []session.DisplayItem, inputs map[string]bool) stopView {
	var view stopView
	seen := make(map[string]bool, len(inputs))
	addTurn := func(turnID string) {
		for _, existing := range view.inputTurns {
			if existing == turnID {
				return
			}
		}
		view.inputTurns = append(view.inputTurns, turnID)
	}
	for _, item := range items {
		switch item.Type {
		case session.DisplayItemUserMessage:
			if inputs[item.ID] {
				seen[item.ID] = true
				view.awaitingStart = true
			}
		case session.DisplayItemSessionCommand, session.DisplayItemSteeringMessage:
			if inputs[item.ID] {
				seen[item.ID] = true
				joined := view.openTurn
				if item.Command != nil && item.Command.TurnID != "" {
					joined = item.Command.TurnID
				}
				if joined != "" {
					addTurn(joined)
				}
			}
		case session.DisplayItemTurnStarted:
			if item.Turn != nil {
				view.openTurn = item.Turn.ID
				if view.awaitingStart {
					addTurn(item.Turn.ID)
					view.awaitingStart = false
				}
			}
		case session.DisplayItemTurnCompleted, session.DisplayItemTurnFailed, session.DisplayItemTurnInterrupted:
			if item.Turn != nil && item.Turn.ID == view.openTurn {
				view.openTurn = ""
			}
		}
	}
	for id := range inputs {
		if !seen[id] {
			view.missingInputs = true
		}
	}
	return view
}

func turnEnded(items []session.DisplayItem, turnID string) bool {
	for _, item := range items {
		switch item.Type {
		case session.DisplayItemTurnCompleted, session.DisplayItemTurnFailed, session.DisplayItemTurnInterrupted:
			if item.Turn != nil && item.Turn.ID == turnID {
				return true
			}
		}
	}
	return false
}

func pendingApprovalFor(items []session.DisplayItem, turnID string) *session.DisplayApproval {
	var pending *session.DisplayApproval
	for _, item := range items {
		if item.Approval == nil || item.Approval.TurnID != turnID {
			continue
		}
		switch item.Type {
		case session.DisplayItemApprovalRequest:
			approval := *item.Approval
			pending = &approval
		case session.DisplayItemApprovalDecision:
			if pending != nil && pending.ID == item.Approval.ID {
				pending = nil
			}
		}
	}
	return pending
}
