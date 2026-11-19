package sessionlog

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

var ErrCommandImageUnavailable = errors.New("command image payload unavailable")

func CommandFromItem(item zotigosession.DisplayItem, rootDir string) (wire.Command, bool, error) {
	if item.Command == nil {
		return wire.Command{}, false, nil
	}
	switch item.Command.Type {
	case wire.CommandMessage:
		command, err := MessageCommandFromItem(item, rootDir)
		return command, err == nil, err
	case wire.CommandPause:
		return PauseCommandFromItem(item), true, nil
	case wire.CommandProfile:
		return ProfileCommandFromItem(item), true, nil
	case wire.CommandApprovalPolicy:
		return ApprovalPolicyCommandFromItem(item), true, nil
	default:
		return wire.Command{}, false, nil
	}
}

func MessageCommandFromItem(item zotigosession.DisplayItem, rootDir string) (wire.Command, error) {
	command := wire.Command{
		ID:        item.ID,
		Sequence:  item.Sequence,
		Type:      wire.CommandMessage,
		CreatedAt: item.CreatedAt,
		Message:   &wire.MessageCommandPayload{},
	}
	if item.Command != nil {
		command.Message.Text = item.Command.Text
		command.Message.Skills = append([]string(nil), item.Command.Skills...)
		command.Message.RequestContext = item.Command.RequestContext.Clone()
		images, err := CommandImagesFromDisplay(item.Command.Images, rootDir)
		if err != nil {
			return wire.Command{}, err
		}
		command.Message.Images = images
	}
	return command, nil
}

func inputCommandFromItem(item zotigosession.DisplayItem, rootDir string) (wire.Command, error) {
	if item.Command != nil && item.Command.Type == wire.CommandSteering {
		return SteeringCommandFromItem(item, rootDir)
	}
	return MessageCommandFromItem(item, rootDir)
}

func SteeringCommandFromItem(item zotigosession.DisplayItem, rootDir string) (wire.Command, error) {
	command := wire.Command{
		ID:        item.ID,
		Sequence:  item.Sequence,
		Type:      wire.CommandSteering,
		CreatedAt: item.CreatedAt,
		Steering:  &wire.SteeringCommandPayload{},
	}
	if item.Command != nil {
		command.Steering.Text = item.Command.Text
		command.Steering.RequestContext = item.Command.RequestContext.Clone()
		command.Steering.TurnID = item.Command.TurnID
		command.Steering.Skills = append([]string(nil), item.Command.Skills...)
		images, err := CommandImagesFromDisplay(item.Command.Images, rootDir)
		if err != nil {
			return wire.Command{}, err
		}
		command.Steering.Images = images
	}
	return command, nil
}

func CommandImagesFromDisplay(images []zotigosession.DisplayCommandImage, rootDir string) ([]wire.CommandImageData, error) {
	if len(images) == 0 {
		return nil, nil
	}
	resp := make([]wire.CommandImageData, 0, len(images))
	for idx, img := range images {
		part := wire.CommandImageData{
			MimeType:  img.MimeType,
			SizeBytes: img.SizeBytes,
			Width:     img.Width,
			Height:    img.Height,
			BlobPath:  img.BlobPath,
		}
		if len(img.Data) > 0 {
			part.DataBase64 = base64.StdEncoding.EncodeToString(img.Data)
		} else if img.BlobPath != "" {
			if rootDir == "" {
				return nil, fmt.Errorf("%w: image persistence is not configured", ErrCommandImageUnavailable)
			}
			data, err := os.ReadFile(filepath.Join(rootDir, img.BlobPath))
			if err != nil {
				return nil, fmt.Errorf("%w: read command image %d: %v", ErrCommandImageUnavailable, idx, err)
			}
			part.DataBase64 = base64.StdEncoding.EncodeToString(data)
		}
		if part.DataBase64 == "" {
			return nil, fmt.Errorf("%w: command image %d payload is unavailable", ErrCommandImageUnavailable, idx)
		}
		resp = append(resp, part)
	}
	return resp, nil
}

func PauseCommandFromItem(item zotigosession.DisplayItem) wire.Command {
	command := wire.Command{
		ID:        item.ID,
		Sequence:  item.Sequence,
		Type:      wire.CommandPause,
		CreatedAt: item.CreatedAt,
		Pause:     &wire.PauseCommandPayload{},
	}
	if item.Command != nil {
		command.Pause.TurnID = item.Command.TurnID
		command.Pause.Reason = item.Command.Reason
	}
	return command
}

func ProfileCommandFromItem(item zotigosession.DisplayItem) wire.Command {
	command := wire.Command{
		ID:        item.ID,
		Sequence:  item.Sequence,
		Type:      wire.CommandProfile,
		CreatedAt: item.CreatedAt,
		Profile:   &wire.ProfileCommandPayload{},
	}
	if item.Command != nil {
		command.Profile.Name = item.Command.Profile
		command.Profile.RequireIdle = item.Command.RequireIdle
	}
	return command
}

func ApprovalPolicyCommandFromItem(item zotigosession.DisplayItem) wire.Command {
	command := wire.Command{
		ID:             item.ID,
		Sequence:       item.Sequence,
		Type:           wire.CommandApprovalPolicy,
		CreatedAt:      item.CreatedAt,
		ApprovalPolicy: &wire.ApprovalPolicyCommandPayload{},
	}
	if item.Command != nil {
		command.ApprovalPolicy.Policy = agent.ApprovalPolicy(item.Command.ApprovalPolicy)
	}
	return command
}

func UserMessageFromCommand(text string, images []wire.CommandImageData, label string) (protocol.Message, error) {
	msg := protocol.Message{
		Role:      protocol.RoleUser,
		Content:   make([]protocol.ContentPart, 0, 1+len(images)),
		CreatedAt: time.Now(),
	}
	if text = strings.TrimSpace(text); text != "" {
		msg.Content = append(msg.Content, protocol.ContentPart{
			Type: protocol.ContentTypeText,
			Text: text,
		})
	}
	for idx, img := range images {
		if img.DataBase64 == "" {
			return protocol.Message{}, fmt.Errorf("%s image payload unavailable for image %d", label, idx)
		}
		data, err := base64.StdEncoding.Strict().DecodeString(img.DataBase64)
		if err != nil {
			return protocol.Message{}, fmt.Errorf("decode %s image %d: %w", label, idx, err)
		}
		msg.Content = append(msg.Content, protocol.ContentPart{
			Type: protocol.ContentTypeImage,
			Image: &protocol.MediaPart{
				Data:      data,
				MediaType: img.MimeType,
			},
		})
	}
	return msg, nil
}
