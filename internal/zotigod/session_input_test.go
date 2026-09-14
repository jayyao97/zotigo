package zotigod

import (
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
)

func TestInputMessageKeepsCommandIDForSteeringAcknowledgement(t *testing.T) {
	message, err := inputMessageFromRequest(workerInputRequest{Command: commandResponse{
		ID: "steering-command", Type: sessionCommandMessage,
		Message: &messageCommandPayload{Text: "change direction"},
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
	command := commandResponse{ID: "channel-message", Type: sessionCommandMessage, Message: &messageCommandPayload{Text: "run", RequestContext: requestContext}}
	message, err := inputMessageFromRequest(workerInputRequest{Command: command})
	if err != nil {
		t.Fatal(err)
	}
	if message.Metadata == nil || message.Metadata.RequestContext == nil || message.Metadata.RequestContext.Actor.Role != "owner" {
		t.Fatalf("message=%+v", message)
	}
	item := displayItemForAcceptedInput(command)
	if item.Command == nil || item.Command.RequestContext == nil || item.Command.RequestContext.ConversationName != "Shadow Test" {
		t.Fatalf("item=%+v", item)
	}
	requestContext.Actor.Role = "member"
	if item.Command.RequestContext.Actor.Role != "owner" || message.Metadata.RequestContext.Actor.Role != "owner" {
		t.Fatal("request context was retained by reference")
	}
}
