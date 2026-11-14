package zotigod

import (
	"context"
	"errors"
	"reflect"

	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

var errInputPolicyMismatch = errors.New("input policy changed before admission")

func workerInputErrorDetails(err error) (string, string) {
	switch {
	case errors.Is(err, errNoActiveTurn):
		return "no_active_turn", "steering requires an active turn"
	case errors.Is(err, errActiveTurn):
		if err != errActiveTurn {
			return "active_turn", err.Error()
		}
		return "active_turn", "a turn is already active"
	case errors.Is(err, errTurnMismatch):
		return "turn_mismatch", "expected_turn_id does not match active turn"
	case errors.Is(err, errCommandIDConflict):
		return "command_id_conflict", err.Error()
	case errors.Is(err, errInputPolicyMismatch):
		return "input_policy_mismatch", err.Error()
	default:
		return "input_failed", err.Error()
	}
}

func findExistingSessionInput(ctx context.Context, source displayItemSource, sessionID string, request wire.Command, rootDir string) (wire.Command, bool, error) {
	items, _, err := source.LoadItems(ctx, sessionID)
	if err != nil {
		return wire.Command{}, false, err
	}
	for _, item := range items {
		if item.ID != request.ID {
			continue
		}
		if item.Command == nil || (item.Command.Type != wire.CommandMessage && item.Command.Type != wire.CommandSteering) {
			return wire.Command{}, false, errCommandIDConflict
		}
		existing, err := sessionInputCommandFromItem(item, rootDir)
		if err != nil {
			return wire.Command{}, false, err
		}
		if !sameCommandInput(existing, request) {
			return wire.Command{}, false, errCommandIDConflict
		}
		return existing, true, nil
	}
	return wire.Command{}, false, nil
}

func sameCommandInput(existing wire.Command, request wire.Command) bool {
	existingText, existingImages, existingSkills := commandInput(existing)
	requestText, requestImages, requestSkills := commandInput(request)
	if existingText != requestText || !equalStrings(existingSkills, requestSkills) || !sameRequestContext(commandRequestContext(existing), commandRequestContext(request)) || len(existingImages) != len(requestImages) {
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

func commandRequestContext(command wire.Command) *protocol.RequestContext {
	if command.Steering != nil {
		return command.Steering.RequestContext
	}
	if command.Message == nil {
		return nil
	}
	return command.Message.RequestContext
}

func sameRequestContext(left, right *protocol.RequestContext) bool {
	if left == nil || right == nil {
		return left == right
	}
	return reflect.DeepEqual(left, right)
}

func commandInput(command wire.Command) (string, []wire.CommandImageData, []string) {
	if command.Message != nil {
		return command.Message.Text, command.Message.Images, command.Message.Skills
	}
	if command.Steering != nil {
		return command.Steering.Text, command.Steering.Images, command.Steering.Skills
	}
	return "", nil, nil
}

func sameCommandImagePaths(left wire.Command, right wire.Command) bool {
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

func inputMessageFromRequest(request wire.InputRequest) (message protocol.Message, err error) {
	text, images, _ := commandInput(request.Command)
	message, err = userMessageFromCommand(text, images, "session input")
	if err == nil && request.Command.Message != nil && request.Command.Message.RequestContext != nil {
		message.Metadata = &protocol.MessageMetadata{RequestContext: request.Command.Message.RequestContext.Clone()}
	}
	message.ID = request.Command.ID
	return message, err
}

func messageCommandForRequest(request wire.InputRequest) wire.Command {
	command := request.Command
	command.Type = wire.CommandMessage
	if command.Message == nil && command.Steering != nil {
		command.Message = &wire.MessageCommandPayload{Text: command.Steering.Text, Images: command.Steering.Images, Skills: command.Steering.Skills, RequestContext: command.Steering.RequestContext.Clone()}
	}
	command.Steering = nil
	return command
}

func steeringCommandForRequest(request wire.InputRequest, turnID string) wire.Command {
	command := request.Command
	text, images, skills := commandInput(command)
	origin := commandRequestContext(command).Clone()
	command.Type = wire.CommandSteering
	command.Message = nil
	command.Steering = &wire.SteeringCommandPayload{Text: text, Images: images, Skills: skills, TurnID: turnID, RequestContext: origin}
	return command
}

func appendAcceptedSessionInput(ctx context.Context, source displayItemSource, sessionID string, command wire.Command) (wire.Command, error) {
	atomic, ok := source.(atomicDisplayItemSource)
	if !ok {
		return wire.Command{}, errors.New("display item source does not support atomic session input")
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
		return wire.Command{}, err
	}
	command.Sequence = item.Sequence
	command.CreatedAt = item.CreatedAt
	return command, nil
}

func displayItemForAcceptedInput(command wire.Command) zotigosession.DisplayItem {
	text, images, skills := commandInput(command)
	displayImages := displayImagesFromCommand(images)
	itemType := zotigosession.DisplayItemUserMessage
	if command.Type == wire.CommandSteering {
		itemType = zotigosession.DisplayItemSessionCommand
	}
	item := displayMessageItem(itemType, text, displayImages)
	item.ID = command.ID
	item.CreatedAt = command.CreatedAt
	item.Command = &zotigosession.DisplayCommand{
		Type: command.Type, Text: text, Images: displayCommandImages(displayImages), Skills: append([]string(nil), skills...), RequestContext: commandRequestContext(command).Clone(),
	}
	if command.Steering != nil {
		item.Command.TurnID = command.Steering.TurnID
	}
	return item
}

func displayImagesFromCommand(images []wire.CommandImageData) []messageImage {
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
