package channels

import (
	"context"
	"errors"
	"slices"

	"github.com/jayyao97/zotigo/core/protocol"
)

// WithSessionToolAccess holds the configuration fence through admission. The
// group's existing workspace binding is the grant, not mere project membership.
func (s *Service) WithSessionToolAccess(ctx context.Context, sessionID string, origin *protocol.RequestContext, workspaceID string, write bool, run func(isOwner bool) error) error {
	if origin == nil || origin.ConnectionID == "" || origin.ConversationID == "" || origin.Actor.ID == "" {
		return errors.New("session tools require a trusted Channel origin")
	}
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	if s.closed {
		return errors.New("channel service is closed")
	}
	conversation, err := s.store.GetConversationBySession(ctx, sessionID)
	if err != nil {
		return err
	}
	if conversation.ConnectionID != origin.ConnectionID || conversation.ID != origin.ConversationID || conversation.ChatID != origin.ExternalConversation {
		return errors.New("channel origin does not match the source session")
	}
	connection, err := s.store.GetConnection(ctx, conversation.ConnectionID)
	if err != nil {
		return err
	}
	group, err := s.store.GetConversationByScope(ctx, connection.ID, conversation.ChatID, "")
	if err != nil {
		return err
	}
	if !connection.Enabled || connection.Provider != origin.Source || !group.Enabled || !conversation.Enabled || group.WorkspaceID != workspaceID || conversation.WorkspaceID != workspaceID || !senderAllowed(connection, group, origin.Actor.ID) {
		return errors.New("channel session tool access is no longer allowed")
	}
	isOwner := slices.Contains(connection.OwnerSenderIDs, origin.Actor.ID)
	if write && !isOwner {
		return errors.New("only the robot connection owner may send messages or create sessions")
	}
	return run(isOwner)
}
