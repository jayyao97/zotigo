package zotigod

import (
	"context"
	"errors"
	"strconv"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
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
				command, err := sessionlog.MessageCommandFromItem(item, rootDir)
				if err != nil {
					if errors.Is(err, sessionlog.ErrCommandImageUnavailable) {
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
				command, err := sessionlog.SteeringCommandFromItem(item, rootDir)
				if err != nil {
					if errors.Is(err, sessionlog.ErrCommandImageUnavailable) {
						continue
					}
					return wire.CommandsPage{}, err
				}
				resp.Commands = append(resp.Commands, command)
				appended = true
			case wire.CommandPause:
				resp.Commands = append(resp.Commands, sessionlog.PauseCommandFromItem(item))
				appended = true
			case wire.CommandProfile:
				resp.Commands = append(resp.Commands, sessionlog.ProfileCommandFromItem(item))
				appended = true
			case wire.CommandApprovalPolicy:
				resp.Commands = append(resp.Commands, sessionlog.ApprovalPolicyCommandFromItem(item))
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

func buildCommandsResponseFromOffset(ctx context.Context, source sessionlog.ItemSource, sessionID string, query commandQuery, rootDir string) (wire.CommandsPage, error) {
	offsetSource, ok := source.(sessionlog.OffsetItemSource)
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
			command, ok, err := sessionlog.CommandFromItem(item, rootDir)
			if err != nil {
				if errors.Is(err, sessionlog.ErrCommandImageUnavailable) {
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
