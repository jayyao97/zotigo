package sessionlog

import (
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

func TestInputMessageKeepsCommandIDForSteeringAcknowledgement(t *testing.T) {
	message, err := InputMessageFromRequest(wire.InputRequest{Command: wire.Command{
		ID: "steering-command", Type: wire.CommandMessage,
		Message: &wire.MessageCommandPayload{Text: "change direction"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if message.ID != "steering-command" {
		t.Fatalf("message ID = %q", message.ID)
	}
}

func TestInputMessageAndDisplayItemKeepRequestContext(t *testing.T) {
	requestContext := &protocol.RequestContext{Source: "feishu", ConversationName: "Shadow Test", Actor: protocol.RequestActor{ID: "ou_owner", Role: "owner"}}
	command := wire.Command{ID: "channel-message", Type: wire.CommandMessage, Message: &wire.MessageCommandPayload{Text: "run", RequestContext: requestContext}}
	message, err := InputMessageFromRequest(wire.InputRequest{Command: command})
	if err != nil {
		t.Fatal(err)
	}
	if message.Metadata == nil || message.Metadata.RequestContext == nil || message.Metadata.RequestContext.Actor.Role != "owner" {
		t.Fatalf("message=%+v", message)
	}
	item := itemForAcceptedInput(command)
	if item.Command == nil || item.Command.RequestContext == nil || item.Command.RequestContext.ConversationName != "Shadow Test" {
		t.Fatalf("item=%+v", item)
	}
	requestContext.Actor.Role = "member"
	if item.Command.RequestContext.Actor.Role != "owner" || message.Metadata.RequestContext.Actor.Role != "owner" {
		t.Fatal("request context was retained by reference")
	}
}

func TestSteeringPreservesTrustedOriginThroughReplay(t *testing.T) {
	origin := &protocol.RequestContext{Source: "feishu", ConnectionID: "bot", Actor: protocol.RequestActor{ID: "owner"}}
	command := wire.Command{ID: "channel:bot:steer", Type: wire.CommandMessage, Message: &wire.MessageCommandPayload{Text: "continue", RequestContext: origin}}
	steering := SteeringCommandForRequest(wire.InputRequest{Command: command}, "turn")
	item := itemForAcceptedInput(steering)
	restored, err := SteeringCommandFromItem(item, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	message := MessageCommandForRequest(wire.InputRequest{Command: restored})
	for _, got := range []*protocol.RequestContext{commandRequestContext(steering), item.Command.RequestContext, commandRequestContext(restored), commandRequestContext(message)} {
		if !SameRequestContext(origin, got) || got == origin {
			t.Fatalf("origin lost or aliased: %+v", got)
		}
	}
}
