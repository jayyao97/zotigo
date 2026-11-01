package zotigodclient

import (
	"context"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/session"
)

// pagedControl serves a history in pages of two items, newest first.
type pagedControl struct {
	items  []session.DisplayItem
	paused []string
}

func (c *pagedControl) Items(_ context.Context, _ string, _ uint64, before uint64, _ int) (ItemsPage, error) {
	end := len(c.items)
	if before > 0 {
		end = int(before) - 1
	}
	start := end - 2
	if start < 0 {
		start = 0
	}
	page := ItemsPage{Items: append([]session.DisplayItem(nil), c.items[start:end]...)}
	if start > 0 {
		page.PrevCursor = "more"
	}
	return page, nil
}

func (c *pagedControl) Pause(_ context.Context, _ string, turnID string) (Command, error) {
	c.paused = append(c.paused, turnID)
	return Command{}, nil
}

func (c *pagedControl) DecideApproval(context.Context, string, string, []ApprovalDecision, bool) error {
	return nil
}

// An input older than the recent window is found in older pages.
func TestStopTurnLooksBeyondRecentWindow(t *testing.T) {
	control := &pagedControl{}
	add := func(item session.DisplayItem) {
		item.Sequence = uint64(len(control.items) + 1)
		control.items = append(control.items, item)
	}
	add(session.DisplayItem{ID: "mine", Type: session.DisplayItemUserMessage})
	add(session.DisplayItem{Type: session.DisplayItemTurnStarted, Turn: &session.DisplayTurn{ID: "turn_1"}})
	for range 5 {
		add(session.DisplayItem{Type: session.DisplayItemAssistantMessage})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := StopTurn(ctx, control, "sess", StopTarget{InputIDs: []string{"mine"}})
	if err != nil || result != StopRequested || len(control.paused) != 1 || control.paused[0] != "turn_1" {
		t.Fatalf("result=%v err=%v paused=%v", result, err, control.paused)
	}
}
