package zotigod

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

func buildCommandsResponse(items []zotigosession.DisplayItem, query commandQuery, rootDir string) (wire.CommandsPage, error) {
	resp := wire.CommandsPage{Commands: make([]wire.Command, 0)}
	lastScanned := query.After

	for _, item := range items {
		if item.Sequence <= query.After {
			continue
		}
		lastScanned = item.Sequence

		if item.Command != nil {
			appended := false
			switch item.Command.Type {
			case wire.CommandMessage:
				command, err := messageCommandFromItem(item, rootDir)
				if err != nil {
					if errors.Is(err, errCommandImageUnavailable) {
						continue
					}
					return wire.CommandsPage{}, err
				}
				resp.Commands = append(resp.Commands, command)
				appended = true
			case wire.CommandSteering:
				if item.Type != zotigosession.DisplayItemSessionCommand {
					break
				}
				command, err := steeringCommandFromItem(item, rootDir)
				if err != nil {
					if errors.Is(err, errCommandImageUnavailable) {
						continue
					}
					return wire.CommandsPage{}, err
				}
				resp.Commands = append(resp.Commands, command)
				appended = true
			case wire.CommandPause:
				resp.Commands = append(resp.Commands, pauseCommandFromItem(item))
				appended = true
			case wire.CommandProfile:
				resp.Commands = append(resp.Commands, profileCommandFromItem(item))
				appended = true
			case wire.CommandApprovalPolicy:
				resp.Commands = append(resp.Commands, approvalPolicyCommandFromItem(item))
				appended = true
			}
			if appended && len(resp.Commands) >= query.Limit {
				break
			}
		}
	}

	switch {
	case len(resp.Commands) > 0:
		resp.NextCursor = strconv.FormatUint(resp.Commands[len(resp.Commands)-1].Sequence, 10)
	case lastScanned > query.After:
		resp.NextCursor = strconv.FormatUint(lastScanned, 10)
	default:
		resp.NextCursor = strconv.FormatUint(query.After, 10)
	}
	return resp, nil
}

func buildCommandsResponseFromOffset(ctx context.Context, source displayItemSource, sessionID string, query commandQuery, rootDir string) (wire.CommandsPage, error) {
	offsetSource, ok := source.(offsetDisplayItemSource)
	if !ok {
		items, _, err := source.LoadItems(ctx, sessionID)
		if err != nil {
			return wire.CommandsPage{}, err
		}
		resp, err := buildCommandsResponse(items, commandQuery{Limit: query.Limit}, rootDir)
		if err != nil {
			return wire.CommandsPage{}, err
		}
		return resp, nil
	}

	resp := wire.CommandsPage{Commands: make([]wire.Command, 0)}
	offset := query.Offset
	for len(resp.Commands) < query.Limit {
		previousOffset := offset
		items, _, nextOffset, err := offsetSource.LoadItemsFromOffset(ctx, sessionID, offset, commandOffsetScanLines)
		if err != nil {
			return wire.CommandsPage{}, err
		}
		for _, item := range items {
			command, ok, err := commandFromDisplayItem(item, rootDir)
			if err != nil {
				if errors.Is(err, errCommandImageUnavailable) {
					continue
				}
				return wire.CommandsPage{}, err
			}
			if ok {
				resp.Commands = append(resp.Commands, command)
				if len(resp.Commands) >= query.Limit {
					if item.LogOffset > 0 {
						offset = item.LogOffset
					} else {
						offset = nextOffset
					}
					resp.NextOffset = offset
					resp.NextCursor = strconv.FormatUint(resp.Commands[len(resp.Commands)-1].Sequence, 10)
					return resp, nil
				}
			}
		}
		if nextOffset == previousOffset || len(items) < commandOffsetScanLines {
			offset = nextOffset
			break
		}
		offset = nextOffset
	}
	resp.NextOffset = offset
	if len(resp.Commands) > 0 {
		resp.NextCursor = strconv.FormatUint(resp.Commands[len(resp.Commands)-1].Sequence, 10)
	}
	return resp, nil
}

func commandFromDisplayItem(item zotigosession.DisplayItem, rootDir string) (wire.Command, bool, error) {
	if item.Command == nil {
		return wire.Command{}, false, nil
	}
	switch item.Command.Type {
	case wire.CommandMessage:
		command, err := messageCommandFromItem(item, rootDir)
		return command, err == nil, err
	case wire.CommandPause:
		return pauseCommandFromItem(item), true, nil
	case wire.CommandProfile:
		return profileCommandFromItem(item), true, nil
	case wire.CommandApprovalPolicy:
		return approvalPolicyCommandFromItem(item), true, nil
	default:
		return wire.Command{}, false, nil
	}
}

func displayMessageItem(itemType zotigosession.DisplayItemType, text string, images []messageImage) zotigosession.DisplayItem {
	content := make([]zotigosession.DisplayContentPart, 0, 1+len(images))
	if text != "" {
		content = append(content, zotigosession.DisplayContentPart{
			Type: string(protocol.ContentTypeText),
			Text: text,
		})
	}
	for _, img := range images {
		content = append(content, zotigosession.DisplayContentPart{
			Type: string(protocol.ContentTypeImage),
			Image: &zotigosession.DisplayMediaPart{
				MediaType: img.MimeType,
				SizeBytes: img.SizeBytes,
				Width:     img.Width,
				Height:    img.Height,
			},
		})
	}
	return zotigosession.DisplayItem{
		Type:    itemType,
		Role:    string(protocol.RoleUser),
		Content: content,
	}
}

func displayCommandImages(images []messageImage) []zotigosession.DisplayCommandImage {
	if len(images) == 0 {
		return nil
	}
	resp := make([]zotigosession.DisplayCommandImage, 0, len(images))
	for _, img := range images {
		resp = append(resp, zotigosession.DisplayCommandImage{
			MimeType:  img.MimeType,
			SizeBytes: img.SizeBytes,
			Width:     img.Width,
			Height:    img.Height,
			BlobPath:  img.BlobPath,
			Data:      img.Data,
		})
	}
	return resp
}

func messageCommandFromItem(item zotigosession.DisplayItem, rootDir string) (wire.Command, error) {
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
		images, err := commandImagesFromDisplay(item.Command.Images, rootDir)
		if err != nil {
			return wire.Command{}, err
		}
		command.Message.Images = images
	}
	return command, nil
}

func sessionInputCommandFromItem(item zotigosession.DisplayItem, rootDir string) (wire.Command, error) {
	if item.Command != nil && item.Command.Type == wire.CommandSteering {
		return steeringCommandFromItem(item, rootDir)
	}
	return messageCommandFromItem(item, rootDir)
}

func steeringCommandFromItem(item zotigosession.DisplayItem, rootDir string) (wire.Command, error) {
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
		images, err := commandImagesFromDisplay(item.Command.Images, rootDir)
		if err != nil {
			return wire.Command{}, err
		}
		command.Steering.Images = images
	}
	return command, nil
}

func commandImagesFromDisplay(images []zotigosession.DisplayCommandImage, rootDir string) ([]wire.CommandImageData, error) {
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
				return nil, fmt.Errorf("%w: image persistence is not configured", errCommandImageUnavailable)
			}
			data, err := os.ReadFile(filepath.Join(rootDir, img.BlobPath))
			if err != nil {
				return nil, fmt.Errorf("%w: read command image %d: %v", errCommandImageUnavailable, idx, err)
			}
			part.DataBase64 = base64.StdEncoding.EncodeToString(data)
		}
		if part.DataBase64 == "" {
			return nil, fmt.Errorf("%w: command image %d payload is unavailable", errCommandImageUnavailable, idx)
		}
		resp = append(resp, part)
	}
	return resp, nil
}

func publicCommandFromCommand(command wire.Command) publicCommandResponse {
	resp := publicCommandResponse{
		ID:        command.ID,
		Sequence:  command.Sequence,
		Type:      command.Type,
		CreatedAt: command.CreatedAt,
	}
	switch command.Type {
	case wire.CommandMessage:
		if command.Message != nil {
			resp.Text = command.Message.Text
			resp.Skills = append([]string(nil), command.Message.Skills...)
			resp.Images = publicCommandImages(command.Message.Images)
		}
	case wire.CommandSteering:
		if command.Steering != nil {
			resp.Text = command.Steering.Text
			resp.Images = publicCommandImages(command.Steering.Images)
			resp.TurnID = command.Steering.TurnID
		}
	case wire.CommandPause:
		if command.Pause != nil {
			resp.TurnID = command.Pause.TurnID
			resp.Reason = command.Pause.Reason
		}
	case wire.CommandProfile:
		if command.Profile != nil {
			resp.Profile = command.Profile.Name
		}
	case wire.CommandApprovalPolicy:
		if command.ApprovalPolicy != nil {
			resp.ApprovalPolicy = command.ApprovalPolicy.Policy
		}
	}
	return resp
}

func publicCommandImages(images []wire.CommandImageData) []publicCommandImageResponse {
	if len(images) == 0 {
		return nil
	}
	resp := make([]publicCommandImageResponse, len(images))
	for i, img := range images {
		resp[i] = publicCommandImageResponse{
			MimeType:  img.MimeType,
			SizeBytes: img.SizeBytes,
			Width:     img.Width,
			Height:    img.Height,
			URL:       publicImageURLFromBlobPath(img.BlobPath),
		}
	}
	return resp
}

func pauseCommandFromItem(item zotigosession.DisplayItem) wire.Command {
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

func profileCommandFromItem(item zotigosession.DisplayItem) wire.Command {
	command := wire.Command{
		ID:        item.ID,
		Sequence:  item.Sequence,
		Type:      wire.CommandProfile,
		CreatedAt: item.CreatedAt,
		Profile:   &wire.ProfileCommandPayload{},
	}
	if item.Command != nil {
		command.Profile.Name = item.Command.Profile
	}
	return command
}

func approvalPolicyCommandFromItem(item zotigosession.DisplayItem) wire.Command {
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

func lastOpenTurnID(items []zotigosession.DisplayItem) string {
	id, _ := lastOpenTurn(items)
	return id
}

func lastOpenTurn(items []zotigosession.DisplayItem) (string, time.Time) {
	var open string
	var started time.Time
	for _, item := range items {
		if item.Turn == nil || item.Turn.ID == "" {
			continue
		}
		switch item.Type {
		case zotigosession.DisplayItemTurnStarted, zotigosession.DisplayItemTurnPaused:
			open = item.Turn.ID
			if item.Type == zotigosession.DisplayItemTurnStarted {
				started = item.CreatedAt
			}
		case zotigosession.DisplayItemTurnCompleted, zotigosession.DisplayItemTurnFailed, zotigosession.DisplayItemTurnInterrupted:
			if open == item.Turn.ID {
				open = ""
				started = time.Time{}
			}
		}
	}
	return open, started
}

func hasPendingApprovalForTurn(items []zotigosession.DisplayItem, turnID string) bool {
	pending := map[string]struct{}{}
	pendingByTurn := false
	for _, item := range items {
		if item.Approval == nil || item.Approval.TurnID != turnID {
			continue
		}
		switch item.Type {
		case zotigosession.DisplayItemApprovalRequest:
			if item.Approval.ID == "" {
				pendingByTurn = true
			} else {
				pending[item.Approval.ID] = struct{}{}
			}
		case zotigosession.DisplayItemApprovalDecision:
			if item.Approval.ID == "" {
				pendingByTurn = false
			} else {
				delete(pending, item.Approval.ID)
			}
		}
	}
	return pendingByTurn || len(pending) > 0
}

func hasPendingMessageCommand(items []zotigosession.DisplayItem) bool {
	pending := false
	for _, item := range items {
		if item.Command != nil && item.Command.Type == wire.CommandMessage {
			pending = true
		}
		switch item.Type {
		case zotigosession.DisplayItemTurnStarted:
			pending = false
		}
	}
	return pending
}
