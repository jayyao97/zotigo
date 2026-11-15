package sessionlog

import (
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

// WorkerRestartedReason is the reason recorded on a turn that was open when
// its worker process went away.
const WorkerRestartedReason = "worker_restarted"

type MessageImage struct {
	MimeType  string
	Data      []byte
	SizeBytes int
	Width     int
	Height    int
	BlobPath  string
	LocalPath string
}

func MessageItem(itemType zotigosession.DisplayItemType, text string, images []MessageImage) zotigosession.DisplayItem {
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

func CommandImages(images []MessageImage) []zotigosession.DisplayCommandImage {
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

func LastOpenTurnID(items []zotigosession.DisplayItem) string {
	id, _ := LastOpenTurn(items)
	return id
}

func LastOpenTurn(items []zotigosession.DisplayItem) (string, time.Time) {
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

func HasPendingApprovalForTurn(items []zotigosession.DisplayItem, turnID string) bool {
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
