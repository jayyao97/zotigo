package zotigod

import "testing"

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
