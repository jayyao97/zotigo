package channels

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Bind targets are referenced by ID or by an owner-defined alias.
const (
	sessionIDPrefix   = "sess_"
	workspaceIDPrefix = "workspace_"
)

func isBindTargetID(value string) bool {
	return strings.HasPrefix(value, sessionIDPrefix) || strings.HasPrefix(value, workspaceIDPrefix)
}

func normalizeBindAliases(aliases []BindAlias) ([]BindAlias, error) {
	out := make([]BindAlias, 0, len(aliases))
	seen := map[string]bool{}
	for _, alias := range aliases {
		name, target := strings.TrimSpace(alias.Name), strings.TrimSpace(alias.Target)
		if name == "" || target == "" {
			return nil, errors.New("each bind alias needs a name and a target")
		}
		if strings.ContainsFunc(name, func(r rune) bool { return r == ' ' || r == '\t' || r == '\r' || r == '\n' }) {
			return nil, errors.New("bind alias names cannot contain spaces")
		}
		if isBindTargetID(name) {
			return nil, errors.New("a bind alias name cannot look like a Session or Workspace ID")
		}
		if !isBindTargetID(target) {
			return nil, errors.New("a bind alias target must be a Session or Workspace ID")
		}
		key := strings.ToLower(name)
		if seen[key] {
			return nil, errors.New("bind alias names must be unique")
		}
		seen[key] = true
		out = append(out, BindAlias{Name: name, Target: target})
	}
	return out, nil
}

// resolveBindTarget turns a /bind argument into a Session or Workspace ID.
// Aliases match without regard to case; IDs are used as given.
func resolveBindTarget(connection Connection, argument string) (string, bool) {
	if isBindTargetID(argument) {
		return argument, true
	}
	for _, alias := range connection.BindAliases {
		if strings.EqualFold(alias.Name, argument) {
			return alias.Target, true
		}
	}
	return "", false
}

// resolveProgressDetail applies a group's override to the connection level.
func resolveProgressDetail(connection Connection, group Conversation) string {
	if level, err := NormalizeProgressDetail(group.ProgressDetail); err == nil && group.ProgressDetail != "" {
		return level
	}
	level, _ := NormalizeProgressDetail(connection.ProgressDetail)
	return level
}

// ChannelBindTarget is a resolved /bind target. A Session target also carries
// its Workspace and the runtime the group must adopt to share it.
type ChannelBindTarget struct {
	SessionID   string
	WorkspaceID string
	Runtime     SessionRuntimeConfig
}

// ChannelBinder lets owners bind a group from the chat. The daemon resolves
// targets and applies the same validation as a binding saved from the client.
type ChannelBinder interface {
	ResolveChannelBindTarget(ctx context.Context, id string) (ChannelBindTarget, error)
	ValidateChannelBinding(ctx context.Context, conversation Conversation, prompt SessionPromptConfig, runtime SessionRuntimeConfig) (SessionPromptConfig, SessionRuntimeConfig, error)
}

// CommandReplier sends a plain reply to a chat command outside the task flow.
type CommandReplier interface {
	ReplyCommand(ctx context.Context, in InboundMessage, text string) error
}

const (
	commandBind   = "/bind"
	commandUnbind = "/unbind"
	commandModel  = "/model"
)

// parseGroupCommand recognizes /bind, /unbind and /model. They only apply to a group
// message that mentions the bot and comes from an application owner; anything
// else is an ordinary message.
func parseGroupCommand(connection Connection, in InboundMessage) (command, argument string, ok bool) {
	if in.ChatType != "group" || !in.MentionedBot || !slices.Contains(connection.OwnerSenderIDs, in.Sender.ID) {
		return "", "", false
	}
	fields := strings.Fields(in.Text)
	if len(fields) == 0 {
		return "", "", false
	}
	command = strings.ToLower(fields[0])
	switch {
	case command == commandBind && len(fields) <= 2:
		if len(fields) == 2 {
			argument = fields[1]
		}
		return command, argument, true
	case command == commandUnbind && len(fields) == 1:
		return command, "", true
	case command == commandModel && len(fields) <= 3:
		return command, strings.Join(fields[1:], " "), true
	}
	return "", "", false
}

// commandDedupWindow outlasts the age after which commands are dropped, so a
// redelivered command never runs twice.
const commandDedupWindow = 10 * time.Minute

// enqueueGroupCommand runs a chat's commands one at a time in arrival order
// and drops a message that was already handled.
func (s *Service) enqueueGroupCommand(connectionID string, in InboundMessage, run func()) {
	now := time.Now()
	s.commandMu.Lock()
	for id, at := range s.handledCommands {
		if now.Sub(at) > commandDedupWindow {
			delete(s.handledCommands, id)
		}
	}
	messageKey := connectionID + "\x00" + in.MessageID
	if _, seen := s.handledCommands[messageKey]; seen {
		s.commandMu.Unlock()
		return
	}
	s.handledCommands[messageKey] = now
	queueKey := connectionID + "\x00" + in.ChatID
	queue, running := s.commandQueues[queueKey]
	s.commandQueues[queueKey] = append(queue, run)
	s.commandMu.Unlock()
	if running {
		return
	}
	go func() {
		for {
			s.commandMu.Lock()
			queue := s.commandQueues[queueKey]
			if len(queue) == 0 {
				delete(s.commandQueues, queueKey)
				s.commandMu.Unlock()
				return
			}
			next := queue[0]
			s.commandQueues[queueKey] = queue[1:]
			s.commandMu.Unlock()
			next()
		}
	}()
}

var errConnectionChanged = NewChatError(nil, "the connection was reconfigured; send the command again")

// ChatError is a failure that a group command can report in the chat. Group
// members other than owners read those replies, so only the public part is
// shown; Error keeps the cause for logs and API responses.
type ChatError struct {
	public string
	cause  error
}

// NewChatError makes an error whose formatted message may be shown in the chat.
func NewChatError(cause error, format string, args ...any) error {
	return &ChatError{public: fmt.Sprintf(format, args...), cause: cause}
}

func (e *ChatError) Error() string {
	if e.cause == nil {
		return e.public
	}
	return e.public + ": " + e.cause.Error()
}

func (e *ChatError) Unwrap() error { return e.cause }

// commandFailure words a failed group command for the chat. Failures without a
// public message may name paths or internal state, so they go to the log only.
func (s *Service) commandFailure(connectionID, action string, err error) string {
	var chatErr *ChatError
	if errors.As(err, &chatErr) {
		return "Zotigo could not " + action + ": " + chatErr.public + "."
	}
	if s.logger != nil {
		s.logger.Printf("channel %s: could not %s: %v", connectionID, action, err)
	}
	return "Zotigo could not " + action + ". The daemon log has the details."
}

// handleGroupCommand runs an owner's /bind or /unbind and replies in the chat.
// It runs without the configuration lock, because binding takes the lifecycle
// and configuration locks for writing. Its contexts end with the adapter run.
func (s *Service) handleGroupCommand(connectionID string, generation uint64, in InboundMessage, command, argument string) {
	s.mu.Lock()
	run := s.runs[connectionID]
	s.mu.Unlock()
	if !s.isRunActive(connectionID, run, generation) {
		return
	}
	ctx, cancel := context.WithTimeout(run.ctx, time.Minute)
	defer cancel()
	reply := s.runGroupCommand(ctx, connectionID, generation, in, command, argument)
	if !s.acquireRunOperation(connectionID, run) {
		return
	}
	defer run.deliveries.Done()
	replier, ok := run.adapter.(CommandReplier)
	if !ok {
		return
	}
	replyCtx, cancelReply := context.WithTimeout(run.ctx, s.deliveryTimeout)
	defer cancelReply()
	if err := replier.ReplyCommand(replyCtx, in, reply); err != nil && s.logger != nil {
		s.logger.Printf("channel %s: reply to %s: %v", connectionID, command, err)
	}
}

func (s *Service) runGroupCommand(ctx context.Context, connectionID string, generation uint64, in InboundMessage, command, argument string) string {
	// A stop or reconfiguration that lands while the command waits must win.
	current := func() error {
		if !s.isGenerationActive(connectionID, generation) {
			return errConnectionChanged
		}
		return nil
	}
	s.configMu.Lock()
	err := current()
	if s.closed {
		err = errors.New("channel service is closed")
	}
	var connection Connection
	if err == nil {
		connection, err = s.store.GetConnection(ctx, connectionID)
	}
	if err == nil {
		if err = s.store.EnsureGroupConversation(ctx, connectionID, in.ChatID); err == nil && in.ChatName != "" {
			err = s.store.PutGroupConversationName(ctx, connectionID, in.ChatID, in.ChatName)
		}
	}
	var group Conversation
	if err == nil {
		group, err = s.store.GetConversationByScope(ctx, connectionID, in.ChatID, "")
	}
	s.configMu.Unlock()
	if err != nil {
		return s.commandFailure(connectionID, "read this group's binding", err)
	}
	switch {
	case command == commandModel:
		return s.runModelCommand(ctx, connectionID, in, group, strings.Fields(argument), current)
	case command == commandBind && argument == "":
		return describeBinding(connection, group)
	case command == commandUnbind:
		if !group.Enabled && group.WorkspaceID == "" {
			return "This group is not bound."
		}
		input := conversationInputFrom(group)
		input.Enabled = false
		input.SessionID = ""
		input.WorkspaceID = ""
		if _, err := s.putConversation(ctx, group.ID, input, nil, current); err != nil {
			return s.commandFailure(connectionID, "unbind this group", err)
		}
		return "This group is unbound. Zotigo ignores its messages until an owner runs /bind again."
	}
	targetID, ok := resolveBindTarget(connection, argument)
	if !ok {
		return fmt.Sprintf("%q is not a Session ID, Workspace ID or alias.%s", argument, aliasHint(connection))
	}
	s.mu.Lock()
	binder, ok := s.dispatcher.(ChannelBinder)
	s.mu.Unlock()
	if !ok {
		return "Binding from the chat is unavailable on this daemon."
	}
	target, err := binder.ResolveChannelBindTarget(ctx, targetID)
	if err != nil {
		return s.commandFailure(connectionID, "bind this group", err)
	}
	input := conversationInputFrom(group)
	input.Enabled = true
	input.WorkspaceID = target.WorkspaceID
	input.SessionID = target.SessionID
	input.SessionStrategy = SessionStrategyTopic
	if target.SessionID != "" {
		input.SessionStrategy = SessionStrategyShared
		agent, profile, model, effort := target.Runtime.Agent, target.Runtime.ProfileName, target.Runtime.Model, target.Runtime.ReasoningEffort
		input.Agent, input.ProfileName, input.Model, input.ReasoningEffort = &agent, &profile, &model, &effort
	} else if group.WorkspaceID != target.WorkspaceID || group.SessionStrategy == SessionStrategyShared {
		// Profiles belong to a Workspace, and a shared Session's profile was its
		// own; new topics start from the target Workspace's default profile.
		empty := ""
		input.ProfileName = &empty
	}
	// A group that was never configured has no senders yet; start with the owners.
	if group.SenderPolicy == SenderPolicySelected && len(normalizeIDs(group.AllowedSenderIDs)) == 0 {
		owners := SenderPolicyOwners
		input.SenderPolicy = &owners
	}
	bound, err := s.putConversation(ctx, group.ID, input, binder.ValidateChannelBinding, current)
	if err != nil {
		return s.commandFailure(connectionID, "bind this group", err)
	}
	return describeBinding(connection, bound)
}

func conversationInputFrom(c Conversation) ConversationInput {
	agent, profile, model, effort, policy := c.Agent, c.ProfileName, c.Model, c.ReasoningEffort, c.SenderPolicy
	approval := c.ApprovalPolicy
	return ConversationInput{
		ApprovalPolicy: &approval, DisplayName: c.DisplayName, SessionStrategy: c.SessionStrategy, SessionID: c.SessionID, WorkspaceID: c.WorkspaceID,
		Agent: &agent, ProfileName: &profile, Model: &model, ReasoningEffort: &effort, Enabled: c.Enabled, SenderPolicy: &policy,
		AllowedSenderIDs: append([]string(nil), c.AllowedSenderIDs...), AgentInstructionsMode: c.AgentInstructionsMode, AgentInstructions: c.AgentInstructions,
		ApprovalInstructionsMode: c.ApprovalInstructionsMode, ApprovalInstructions: c.ApprovalInstructions, ReviewAllTools: c.ReviewAllTools,
	}
}

func describeBinding(connection Connection, group Conversation) string {
	if !group.Enabled || group.WorkspaceID == "" {
		return "This group is not bound. An owner can mention me with /bind followed by a Session ID, Workspace ID or alias." + aliasHint(connection)
	}
	senders := map[string]string{SenderPolicyOwners: "application owners", SenderPolicyAll: "everyone in the group", SenderPolicySelected: "selected members"}[group.SenderPolicy]
	if group.SessionStrategy == SessionStrategyShared {
		return fmt.Sprintf("This group shares Session %s (Workspace %s). Messages from %s are handled.", group.SessionID, group.WorkspaceID, senders)
	}
	return fmt.Sprintf("This group is bound to Workspace %s; each topic gets its own Session. Messages from %s are handled.", group.WorkspaceID, senders)
}

func aliasHint(connection Connection) string {
	if len(connection.BindAliases) == 0 {
		return ""
	}
	names := make([]string, 0, len(connection.BindAliases))
	for _, alias := range connection.BindAliases {
		names = append(names, alias.Name)
	}
	return " Aliases: " + strings.Join(names, ", ") + "."
}
