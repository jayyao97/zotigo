package zotigod

import (
	"context"
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/session"
)

func TestSessionToolSteeringAuthority(t *testing.T) {
	origin := &protocol.RequestContext{Source: "feishu", ConnectionID: "bot", ConversationID: "conversation", ExternalConversation: "chat", Actor: protocol.RequestActor{ID: "owner"}}
	for _, name := range []string{"same actor", "different actor", "different connection", "different conversation", "local into channel", "channel into local", "missing channel origin", "delegated input"} {
		t.Run(name, func(t *testing.T) {
			h, caller := newSessionToolFixture(t)
			ctx := context.Background()
			initial, steering := origin.Clone(), origin.Clone()
			id := "channel:bot:steering"
			wantAllowed := name == "same actor"
			steering.ExternalMessageID = "new-message"
			steering.Actor.DisplayName = "renamed"
			switch name {
			case "different actor":
				steering.Actor.ID = "member"
			case "different connection":
				steering.ConnectionID = "other"
			case "different conversation":
				steering.ConversationID = "other"
			case "local into channel":
				steering, id = nil, "local-steering"
			case "channel into local":
				initial = nil
			case "missing channel origin":
				initial, steering = nil, nil
			case "delegated input":
				initial, steering, id = nil, nil, "session_tool:delegation"
			}
			for _, item := range []session.DisplayItem{
				{Type: session.DisplayItemTurnCompleted, Turn: &session.DisplayTurn{ID: "turn"}},
				{ID: "next-input", Type: session.DisplayItemUserMessage, Command: &session.DisplayCommand{Type: sessionCommandMessage, RequestContext: initial}},
				{Type: session.DisplayItemTurnStarted, Turn: &session.DisplayTurn{ID: "next"}},
				{ID: id, Type: session.DisplayItemSessionCommand, Command: &session.DisplayCommand{Type: sessionCommandSteering, TurnID: "next", RequestContext: steering}},
			} {
				if _, err := h.store.AppendDisplayItem(ctx, caller.SessionID, item); err != nil {
					t.Fatal(err)
				}
			}
			_, err := h.resolveSessionToolCaller(ctx, caller.SessionID, caller.Generation, "next")
			if (err == nil) != wantAllowed {
				t.Fatalf("allowed=%v, error=%v", wantAllowed, err)
			}
		})
	}
}
