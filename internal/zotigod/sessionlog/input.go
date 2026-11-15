package sessionlog

import (
	"context"
	"errors"
	"reflect"
	"strings"

	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

var (
	ErrActiveTurn          = errors.New("active_turn: a turn is already active")
	ErrNoActiveTurn        = errors.New("no active turn")
	ErrTurnMismatch        = errors.New("active turn does not match expected turn")
	ErrCommandIDConflict   = errors.New("client message ID already belongs to different input")
	ErrInputPolicyMismatch = errors.New("input policy changed before admission")
)

func InputErrorDetails(err error) (string, string) {
	switch {
	case errors.Is(err, ErrNoActiveTurn):
		return "no_active_turn", "steering requires an active turn"
	case errors.Is(err, ErrActiveTurn):
		if err != ErrActiveTurn {
			return "active_turn", err.Error()
		}
		return "active_turn", "a turn is already active"
	case errors.Is(err, ErrTurnMismatch):
		return "turn_mismatch", "expected_turn_id does not match active turn"
	case errors.Is(err, ErrCommandIDConflict):
		return "command_id_conflict", err.Error()
	case errors.Is(err, ErrInputPolicyMismatch):
		return "input_policy_mismatch", err.Error()
	default:
		return "input_failed", err.Error()
	}
}

func FindExistingInput(ctx context.Context, source ItemSource, sessionID string, request wire.Command, rootDir string) (wire.Command, bool, error) {
	items, _, err := source.LoadItems(ctx, sessionID)
	if err != nil {
		return wire.Command{}, false, err
	}
	for _, item := range items {
		if item.ID != request.ID {
			continue
		}
		if item.Command == nil || (item.Command.Type != wire.CommandMessage && item.Command.Type != wire.CommandSteering) {
			return wire.Command{}, false, ErrCommandIDConflict
		}
		existing, err := inputCommandFromItem(item, rootDir)
		if err != nil {
			return wire.Command{}, false, err
		}
		if !SameCommandInput(existing, request) {
			return wire.Command{}, false, ErrCommandIDConflict
		}
		return existing, true, nil
	}
	return wire.Command{}, false, nil
}

func SameCommandInput(existing wire.Command, request wire.Command) bool {
	existingText, existingImages, existingSkills := commandInput(existing)
	requestText, requestImages, requestSkills := commandInput(request)
	if existingText != requestText || !equalStrings(existingSkills, requestSkills) || !SameRequestContext(commandRequestContext(existing), commandRequestContext(request)) || len(existingImages) != len(requestImages) {
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

func SameRequestContext(left, right *protocol.RequestContext) bool {
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

func SameCommandImagePaths(left wire.Command, right wire.Command) bool {
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

func InputMessageFromRequest(request wire.InputRequest) (message protocol.Message, err error) {
	text, images, _ := commandInput(request.Command)
	message, err = UserMessageFromCommand(text, images, "session input")
	if err == nil && request.Command.Message != nil && request.Command.Message.RequestContext != nil {
		message.Metadata = &protocol.MessageMetadata{RequestContext: request.Command.Message.RequestContext.Clone()}
	}
	message.ID = request.Command.ID
	return message, err
}

func MessageCommandForRequest(request wire.InputRequest) wire.Command {
	command := request.Command
	command.Type = wire.CommandMessage
	if command.Message == nil && command.Steering != nil {
		command.Message = &wire.MessageCommandPayload{Text: command.Steering.Text, Images: command.Steering.Images, Skills: command.Steering.Skills, RequestContext: command.Steering.RequestContext.Clone()}
	}
	command.Steering = nil
	return command
}

func SteeringCommandForRequest(request wire.InputRequest, turnID string) wire.Command {
	command := request.Command
	text, images, skills := commandInput(command)
	origin := commandRequestContext(command).Clone()
	command.Type = wire.CommandSteering
	command.Message = nil
	command.Steering = &wire.SteeringCommandPayload{Text: text, Images: images, Skills: skills, TurnID: turnID, RequestContext: origin}
	return command
}

func AppendAcceptedInput(ctx context.Context, source ItemSource, sessionID string, command wire.Command) (wire.Command, error) {
	atomic, ok := source.(AtomicItemSource)
	if !ok {
		return wire.Command{}, errors.New("display item source does not support atomic session input")
	}
	item, _, err := atomic.AppendItemAtomically(ctx, sessionID, func(items []zotigosession.DisplayItem) (zotigosession.DisplayItem, bool, error) {
		for _, existing := range items {
			if existing.ID == command.ID {
				return zotigosession.DisplayItem{}, false, ErrCommandIDConflict
			}
		}
		return itemForAcceptedInput(command), true, nil
	})
	if err != nil {
		return wire.Command{}, err
	}
	command.Sequence = item.Sequence
	command.CreatedAt = item.CreatedAt
	return command, nil
}

func itemForAcceptedInput(command wire.Command) zotigosession.DisplayItem {
	text, images, skills := commandInput(command)
	displayImages := MessageImagesFromCommand(images)
	itemType := zotigosession.DisplayItemUserMessage
	if command.Type == wire.CommandSteering {
		itemType = zotigosession.DisplayItemSessionCommand
	}
	item := MessageItem(itemType, text, displayImages)
	item.ID = command.ID
	item.CreatedAt = command.CreatedAt
	item.Command = &zotigosession.DisplayCommand{
		Type: command.Type, Text: text, Images: CommandImages(displayImages), Skills: append([]string(nil), skills...), RequestContext: commandRequestContext(command).Clone(),
	}
	if command.Steering != nil {
		item.Command.TurnID = command.Steering.TurnID
	}
	return item
}

func MessageImagesFromCommand(images []wire.CommandImageData) []MessageImage {
	result := make([]MessageImage, 0, len(images))
	for _, image := range images {
		result = append(result, MessageImage{
			MimeType: image.MimeType, SizeBytes: image.SizeBytes, Width: image.Width, Height: image.Height, BlobPath: image.BlobPath,
		})
	}
	return result
}

func StoreRoot(store any) string {
	if rooted, ok := store.(interface{ RootDir() string }); ok {
		return rooted.RootDir()
	}
	return ""
}

// Recheck at admission as an approval can appear after the dispatcher observed
// the active turn. Callback data must not act as a human approval response.
func ValidateCompletionSteering(ctx context.Context, source ItemSource, id string, command wire.Command) error {
	if !strings.HasPrefix(command.ID, "completion:") {
		return nil
	}
	items, _, err := source.LoadItems(ctx, id)
	if err != nil {
		return err
	}
	if HasPendingHumanRequest(items) {
		return ErrActiveTurn
	}
	return nil
}

func equalStrings(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
