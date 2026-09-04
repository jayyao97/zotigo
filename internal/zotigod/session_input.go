package zotigod

import (
	"context"
	"errors"

	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

func workerInputErrorDetails(err error) (string, string) {
	switch {
	case errors.Is(err, errNoActiveTurn):
		return "no_active_turn", "steering requires an active turn"
	case errors.Is(err, errTurnMismatch):
		return "turn_mismatch", "expected_turn_id does not match active turn"
	case errors.Is(err, errCommandIDConflict):
		return "command_id_conflict", err.Error()
	default:
		return "input_failed", err.Error()
	}
}

func findExistingSessionInput(ctx context.Context, source displayItemSource, sessionID string, request commandResponse, rootDir string) (commandResponse, bool, error) {
	items, _, err := source.LoadItems(ctx, sessionID)
	if err != nil {
		return commandResponse{}, false, err
	}
	for _, item := range items {
		if item.ID != request.ID {
			continue
		}
		if item.Command == nil || (item.Command.Type != sessionCommandMessage && item.Command.Type != sessionCommandSteering) {
			return commandResponse{}, false, errCommandIDConflict
		}
		existing, err := sessionInputCommandFromItem(item, rootDir)
		if err != nil {
			return commandResponse{}, false, err
		}
		if !sameCommandInput(existing, request) {
			return commandResponse{}, false, errCommandIDConflict
		}
		return existing, true, nil
	}
	return commandResponse{}, false, nil
}

func sameCommandInput(existing commandResponse, request commandResponse) bool {
	existingText, existingImages, existingSkills := commandInput(existing)
	requestText, requestImages, requestSkills := commandInput(request)
	if existingText != requestText || !equalStrings(existingSkills, requestSkills) || len(existingImages) != len(requestImages) {
		return false
	}
	for index := range existingImages {
		left := existingImages[index]
		right := requestImages[index]
		if left.MimeType != right.MimeType || left.SizeBytes != right.SizeBytes || left.Width != right.Width || left.Height != right.Height || left.DataBase64 != right.DataBase64 {
			return false
		}
	}
	return true
}

func commandInput(command commandResponse) (string, []commandImageData, []string) {
	if command.Message != nil {
		return command.Message.Text, command.Message.Images, command.Message.Skills
	}
	if command.Steering != nil {
		return command.Steering.Text, command.Steering.Images, command.Steering.Skills
	}
	return "", nil, nil
}

func sameCommandImagePaths(left commandResponse, right commandResponse) bool {
	_, leftImages, _ := commandInput(left)
	_, rightImages, _ := commandInput(right)
	if len(leftImages) != len(rightImages) {
		return false
	}
	for index := range leftImages {
		if leftImages[index].BlobPath != rightImages[index].BlobPath {
			return false
		}
	}
	return true
}

func inputMessageFromRequest(request workerInputRequest) (message protocol.Message, err error) {
	text, images, _ := commandInput(request.Command)
	return userMessageFromCommand(text, images, "session input")
}

func messageCommandForRequest(request workerInputRequest) commandResponse {
	command := request.Command
	command.Type = sessionCommandMessage
	if command.Message == nil && command.Steering != nil {
		command.Message = &messageCommandPayload{Text: command.Steering.Text, Images: command.Steering.Images, Skills: command.Steering.Skills}
	}
	command.Steering = nil
	return command
}

func steeringCommandForRequest(request workerInputRequest, turnID string) commandResponse {
	command := request.Command
	text, images, skills := commandInput(command)
	command.Type = sessionCommandSteering
	command.Message = nil
	command.Steering = &steeringCommandPayload{Text: text, Images: images, Skills: skills, TurnID: turnID}
	return command
}

func appendAcceptedSessionInput(ctx context.Context, source displayItemSource, sessionID string, command commandResponse) (commandResponse, error) {
	atomic, ok := source.(atomicDisplayItemSource)
	if !ok {
		return commandResponse{}, errors.New("display item source does not support atomic session input")
	}
	item, _, err := atomic.AppendItemAtomically(ctx, sessionID, func(items []zotigosession.DisplayItem) (zotigosession.DisplayItem, bool, error) {
		for _, existing := range items {
			if existing.ID == command.ID {
				return zotigosession.DisplayItem{}, false, errCommandIDConflict
			}
		}
		return displayItemForAcceptedInput(command), true, nil
	})
	if err != nil {
		return commandResponse{}, err
	}
	command.Sequence = item.Sequence
	command.CreatedAt = item.CreatedAt
	return command, nil
}

func displayItemForAcceptedInput(command commandResponse) zotigosession.DisplayItem {
	text, images, skills := commandInput(command)
	displayImages := displayImagesFromCommand(images)
	itemType := zotigosession.DisplayItemUserMessage
	if command.Type == sessionCommandSteering {
		itemType = zotigosession.DisplayItemSessionCommand
	}
	item := displayMessageItem(itemType, text, displayImages)
	item.ID = command.ID
	item.CreatedAt = command.CreatedAt
	item.Command = &zotigosession.DisplayCommand{
		Type: command.Type, Text: text, Images: displayCommandImages(displayImages), Skills: append([]string(nil), skills...),
	}
	if command.Steering != nil {
		item.Command.TurnID = command.Steering.TurnID
	}
	return item
}

func displayImagesFromCommand(images []commandImageData) []messageImage {
	result := make([]messageImage, 0, len(images))
	for _, image := range images {
		result = append(result, messageImage{
			MimeType: image.MimeType, SizeBytes: image.SizeBytes, Width: image.Width, Height: image.Height, BlobPath: image.BlobPath,
		})
	}
	return result
}

func storeRoot(store any) string {
	if rooted, ok := store.(interface{ RootDir() string }); ok {
		return rooted.RootDir()
	}
	return ""
}
