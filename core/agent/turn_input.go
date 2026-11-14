package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/agent/prompt"
	"github.com/jayyao97/zotigo/core/protocol"
)

// QueueTurnUserMessage queues structured user input for the active turn.
func (a *Agent) QueueTurnUserMessage(msg protocol.Message) error {
	return a.QueueTurnUserMessageWithAdmission(msg, nil)
}

// QueueTurnUserMessageWithAdmission commits admission while holding the same
// lock that decides whether the active turn can still accept input. The admit
// callback must not call back into Agent methods.
func (a *Agent) QueueTurnUserMessageWithAdmission(msg protocol.Message, admit func() error) error {
	msg, err := normalizeQueuedTurnUserMessage(msg)
	if err != nil {
		return err
	}
	if err := a.lockForInput(); err != nil {
		return err
	}
	if a.state != StateRunning && a.state != StatePaused {
		a.mu.Unlock()
		return ErrNotRunning
	}
	if admit != nil {
		if err := admit(); err != nil {
			a.mu.Unlock()
			return err
		}
	}
	a.pendingTurnUser = append(a.pendingTurnUser, msg)
	var cancel context.CancelFunc
	if a.activeToolRun != nil {
		a.activeToolRun.interrupted = true
		cancel = a.activeToolRun.cancel
	}
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func normalizeQueuedTurnUserMessage(msg protocol.Message) (protocol.Message, error) {
	if msg.Role != protocol.RoleUser {
		return protocol.Message{}, errors.New("input must be a user message")
	}
	content := make([]protocol.ContentPart, 0, len(msg.Content))
	for _, part := range msg.Content {
		switch part.Type {
		case protocol.ContentTypeText:
			text := strings.TrimSpace(part.Text)
			if text != "" {
				part.Text = text
				content = append(content, part)
			}
		case protocol.ContentTypeImage:
			if part.Image != nil {
				content = append(content, part)
			}
		}
	}
	if len(content) == 0 {
		return protocol.Message{}, errors.New("input is required")
	}
	msg.Content = content
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	return msg, nil
}

// ClearPendingTurnUserInput discards active-turn input that was not drained
// before a turn was interrupted or otherwise abandoned.
func (a *Agent) ClearPendingTurnUserInput() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pendingTurnUser = nil
}

func (a *Agent) applyPendingTurnUserInput(ctx context.Context, outCh chan<- protocol.Event) (bool, error) {
	a.mu.RLock()
	pendingCount := len(a.pendingTurnUser)
	steeringIDs := make([]string, 0, pendingCount)
	for _, msg := range a.pendingTurnUser {
		if msg.ID != "" {
			steeringIDs = append(steeringIDs, msg.ID)
		}
	}
	a.mu.RUnlock()
	if pendingCount == 0 {
		return false, nil
	}

	acknowledged := make(chan error, 1)
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case outCh <- protocol.Event{Type: protocol.EventTypeSteeringApplied, SteeringIDs: steeringIDs, SteeringAck: acknowledged}:
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case err := <-acknowledged:
		if err != nil {
			return false, err
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	return a.drainPendingTurnUserInputLocked(pendingCount)
}

func (a *Agent) drainPendingTurnUserInputLocked(pendingCount int) (bool, error) {
	if pendingCount <= 0 || len(a.pendingTurnUser) < pendingCount {
		return false, nil
	}
	pending := a.pendingTurnUser[:pendingCount]
	content := make([]protocol.ContentPart, 0, pendingCount)
	texts := make([]string, 0, pendingCount)
	flushText := func() {
		if len(texts) == 0 {
			return
		}
		content = append(content, protocol.ContentPart{
			Type: protocol.ContentTypeText,
			Text: strings.Join(texts, "\n\n"),
		})
		texts = texts[:0]
	}
	for _, msg := range pending {
		for _, part := range msg.Content {
			if part.Type == protocol.ContentTypeText {
				text := strings.TrimSpace(part.Text)
				if text != "" {
					texts = append(texts, text)
				}
				continue
			}
			flushText()
			content = append(content, part)
		}
	}
	flushText()
	msg := protocol.Message{
		Role:      protocol.RoleUser,
		Content:   content,
		CreatedAt: time.Now(),
	}
	if err := a.appendUserInputLocked(msg, userInputContinuesTurn); err != nil {
		return false, err
	}
	a.pendingTurnUser = append([]protocol.Message(nil), a.pendingTurnUser[pendingCount:]...)
	return true, nil
}

func (a *Agent) appendUserInputLocked(msg protocol.Message, boundary userInputBoundary) error {
	messages := make([]protocol.Message, 0, 3)
	var nextState *prompt.UserContextState
	if a.userContext != nil {
		contextText, state, err := a.userContext.BuildUpdate(a.promptContext(), a.conversation.userContextState)
		if err != nil {
			return fmt.Errorf("build user context: %w", err)
		}
		if contextText != "" {
			messages = append(messages, protocol.NewContextualUserMessage(contextText))
		}
		nextState = state.Clone()
	}
	if requestContext := messageRequestContext(msg); requestContext != nil {
		messages = append(messages, protocol.NewContextualUserMessage(requestContext.PromptText()))
	} else if boundary == userInputStartsTurn && a.turnSafety.RequestContext != nil {
		// Contextual messages remain in the model history until compaction. Make
		// the transition back to a local input explicit so the prior external
		// sender and role cannot be mistaken for the current request provenance.
		messages = append(messages, protocol.NewContextualUserMessage("<request_context>\nnull\n</request_context>"))
	}
	messages = append(messages, msg)
	if err := a.conversation.append(messages, nextState, a.userContext != nil); err != nil {
		return fmt.Errorf("record user history: %w", err)
	}
	return nil
}

func messageRequestContext(msg protocol.Message) *protocol.RequestContext {
	if msg.Metadata == nil {
		return nil
	}
	return msg.Metadata.RequestContext.Clone()
}

func (a *Agent) promptContext() prompt.PromptContext {
	return prompt.PromptContext{
		WorkDir:  a.executor.WorkDir(),
		Platform: a.executor.Platform(),
		Model:    a.cfg.Model,
	}
}
