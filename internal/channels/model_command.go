package channels

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ChannelModelSwitcher lets owners change models from the chat. Choices are
// interpreted for the current agent; /model never switches between agents.
type ChannelModelSwitcher interface {
	// ChannelSessionRuntime reports a Session's current runtime.
	ChannelSessionRuntime(ctx context.Context, sessionID string) (SessionRuntimeConfig, error)
	// ChannelRuntimeChoices lists what /model accepts for an agent in a Workspace.
	ChannelRuntimeChoices(ctx context.Context, workspaceID, agent string) ([]string, error)
	// ResolveChannelRuntime turns /model arguments into a runtime for new Sessions.
	ResolveChannelRuntime(ctx context.Context, workspaceID string, current SessionRuntimeConfig, choice []string) (SessionRuntimeConfig, error)
	// SwitchChannelSessionRuntime changes an idle Session's runtime. Pending
	// reports that a running worker applies it before the next turn. It
	// resolves the choice first, then calls hold before taking the Session's
	// operation lock and changes the Session only while hold's release is
	// pending. An error from hold is returned as is.
	SwitchChannelSessionRuntime(ctx context.Context, sessionID string, choice []string, hold func() (release func(), err error)) (runtime SessionRuntimeConfig, pending bool, err error)
}

const maxModelChoices = 20

// runModelCommand shows or changes the model for the message's scope: the
// Session of a shared group or of the topic the message replies in; for a
// top-level message in a one-session-per-topic group, the runtime of new topics.
func (s *Service) runModelCommand(ctx context.Context, connectionID string, in InboundMessage, group Conversation, choice []string, current func() error) string {
	if !group.Enabled || group.WorkspaceID == "" {
		return "This group is not bound. An owner can bind it with /bind first."
	}
	s.mu.Lock()
	switcher, ok := s.dispatcher.(ChannelModelSwitcher)
	binder, _ := s.dispatcher.(ChannelBinder)
	s.mu.Unlock()
	if !ok || binder == nil {
		return "Changing models from the chat is unavailable on this daemon."
	}
	// owner is the conversation the Session is bound to.
	sessionID, owner, scope := "", "", "new topics in this group"
	switch {
	case group.SessionStrategy == SessionStrategyShared:
		sessionID, owner, scope = group.SessionID, group.ID, "this group's shared session"
		if sessionID == "" {
			scope = "this group's shared session, created on the next mention"
		}
	case !in.StartsConversation:
		s.configMu.RLock()
		topic, err := s.store.GetConversationByScope(ctx, connectionID, in.ChatID, in.RootID)
		s.configMu.RUnlock()
		if err != nil && !errors.Is(err, ErrNotFound) {
			return s.commandFailure(connectionID, "read this topic", err)
		}
		if topic.SessionID == "" {
			return "This topic has no session yet. Mention me with a request first, or send /model as a new message to change the model for new topics."
		}
		sessionID, owner, scope = topic.SessionID, topic.ID, "this topic's session"
	}

	if len(choice) == 0 {
		runtime := conversationRuntime(group)
		if sessionID != "" {
			var err error
			if runtime, err = switcher.ChannelSessionRuntime(ctx, sessionID); err != nil {
				return s.commandFailure(connectionID, "read the session", err)
			}
		}
		reply := fmt.Sprintf("Model for %s: %s.", scope, describeRuntime(runtime))
		choices, err := switcher.ChannelRuntimeChoices(ctx, group.WorkspaceID, runtime.Agent)
		if err != nil {
			return reply + " " + s.commandFailure(connectionID, "list the choices", err)
		}
		if len(choices) > maxModelChoices {
			choices = append(choices[:maxModelChoices], "…")
		}
		usage := "/model <profile>"
		if runtime.Agent == SessionAgentCodex {
			usage = "/model <model> [reasoning effort]"
		}
		return reply + "\nSwitch with " + usage + ". Choices:\n" + strings.Join(choices, "\n")
	}

	if err := current(); err != nil {
		return s.commandFailure(connectionID, "change the model", err)
	}
	if sessionID != "" {
		// Resolving the choice can take a while, and the group may be disabled
		// or rebound meanwhile. hold rechecks the binding under the configuration
		// read lock and keeps it until the switch is done. Channel page edits take
		// the write lock, so they land wholly before the recheck or after the
		// switch. The order, configuration then Session lock, matches admission.
		hold := func() (func(), error) {
			s.configMu.RLock()
			if err := s.sessionStillBound(ctx, group, owner, sessionID, current); err != nil {
				s.configMu.RUnlock()
				return nil, err
			}
			return s.configMu.RUnlock, nil
		}
		runtime, pending, err := switcher.SwitchChannelSessionRuntime(ctx, sessionID, choice, hold)
		if err != nil {
			return s.commandFailure(connectionID, "change the model", err)
		}
		if pending {
			return fmt.Sprintf("Switching %s to %s before its next turn.", scope, describeRuntime(runtime))
		}
		return fmt.Sprintf("%s now uses %s.", capitalize(scope), describeRuntime(runtime))
	}
	runtime, err := switcher.ResolveChannelRuntime(ctx, group.WorkspaceID, conversationRuntime(group), choice)
	if err != nil {
		return s.commandFailure(connectionID, "change the model", err)
	}
	_, err = s.updateConversation(ctx, group.ID, func(latest Conversation) (ConversationInput, error) {
		// The choice was resolved for the group as read; a group disabled,
		// rebound or given another model meanwhile keeps that newer state.
		if err := bindingChangedSince(group, latest); err != nil {
			return ConversationInput{}, err
		}
		if conversationRuntime(latest) != conversationRuntime(group) {
			return ConversationInput{}, errBindingChanged
		}
		input := conversationInputFrom(latest)
		input.Agent, input.ProfileName, input.Model, input.ReasoningEffort = &runtime.Agent, &runtime.ProfileName, &runtime.Model, &runtime.ReasoningEffort
		return input, nil
	}, binder.ValidateChannelBinding, current)
	if err != nil {
		return s.commandFailure(connectionID, "change the model", err)
	}
	if group.SessionStrategy == SessionStrategyShared {
		return fmt.Sprintf("The shared session will use %s when the next mention creates it.", describeRuntime(runtime))
	}
	return fmt.Sprintf("New topics in this group now use %s. Existing topics keep their model.", describeRuntime(runtime))
}

// sessionStillBound reports whether the group still has the binding the
// command read and the Session still belongs to owner. The caller holds
// configMu.
func (s *Service) sessionStillBound(ctx context.Context, group Conversation, owner, sessionID string, current func() error) error {
	if s.closed {
		return errors.New("channel service is closed")
	}
	if err := current(); err != nil {
		return err
	}
	latest, err := s.store.GetConversation(ctx, group.ID)
	if err != nil {
		return err
	}
	if err := bindingChangedSince(group, latest); err != nil {
		return err
	}
	bound, err := s.store.GetConversationBySession(ctx, sessionID)
	if errors.Is(err, ErrNotFound) || err == nil && (bound.ID != owner || !bound.Enabled) {
		return errBindingChanged
	}
	return err
}

func describeRuntime(runtime SessionRuntimeConfig) string {
	if runtime.Agent == SessionAgentCodex {
		return fmt.Sprintf("Codex %s, %s reasoning", runtime.Model, runtime.ReasoningEffort)
	}
	if runtime.ProfileName == "" {
		return "the Workspace default profile"
	}
	return "profile " + runtime.ProfileName
}

func capitalize(text string) string {
	if text == "" {
		return text
	}
	return strings.ToUpper(text[:1]) + text[1:]
}
