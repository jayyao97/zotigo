package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/jayyao97/zotigo/core/session"
)

func newTestSessionsModel(n, cursor, height int) SessionSelectionModel {
	sessions := make([]session.Metadata, n)
	return SessionSelectionModel{
		sessions: sessions,
		cursor:   cursor,
		height:   height,
	}
}

// newTestModelWithArchive creates a picker whose archive hook records the
// archived IDs. Session IDs are long enough for the short-ID display.
func newTestModelWithArchive(t *testing.T, n, cursor, height int) (SessionSelectionModel, map[string]bool) {
	t.Helper()
	archived := map[string]bool{}
	metas := make([]session.Metadata, 0, n)
	for i := range n {
		metas = append(metas, session.Metadata{
			ID:               fmt.Sprintf("sess_test%04d", i),
			WorkingDirectory: "/tmp",
			UpdatedAt:        time.Now(),
		})
	}
	m := NewSessionSelectionModel(metas, func(id string) error {
		archived[id] = true
		return nil
	}, nil, nil)
	m.cursor = cursor
	m.height = height
	return m, archived
}

func keyPress(s string) tea.KeyPressMsg {
	if len(s) == 1 {
		return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
	}
	return tea.KeyPressMsg{Text: s}
}

func TestVisibleRows(t *testing.T) {
	tests := []struct {
		name   string
		height int
		want   int
		reason string
	}{
		{name: "UnknownHeight", height: 0, want: defaultVisibleSessionRows, reason: "unknown height should render conservative page"},
		{name: "ReservesHeaderFooter", height: 30, want: 26, reason: "height 30 minus 4 overhead = 26"},
		{name: "TinyTerminalFloor", height: 4, want: 3, reason: "visible rows should floor at 3 on tiny terminals"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestSessionsModel(50, 0, tc.height)
			if got := m.visibleRows(); got != tc.want {
				t.Fatalf("%s, got %d", tc.reason, got)
			}
		})
	}
}

func TestClampOffset(t *testing.T) {
	tests := []struct {
		name     string
		sessions int
		cursor   int
		height   int
		offset   int
		want     int
		reason   string
	}{
		// height 10 -> visible 6. Cursor at 20 should make offset = 15 so cursor
		// sits on the last visible row.
		{name: "CursorBelowWindowScrollsDown", sessions: 50, cursor: 20, height: 10, offset: 0, want: 15, reason: "expected offset 15"},
		// Start scrolled down, then jump cursor to the top.
		{name: "CursorAboveWindowScrollsUp", sessions: 50, cursor: 2, height: 10, offset: 30, want: 2, reason: "expected offset to follow cursor up to 2"},
		{name: "CursorInsideWindowNoop", sessions: 50, cursor: 20, height: 10, offset: 18, want: 18, reason: "cursor already visible, offset should not move"},
		// 3 sessions, viewport fits them all -> offset must be 0. Starts from a garbage offset.
		{name: "ShorterThanViewport", sessions: 3, cursor: 2, height: 30, offset: 99, want: 0, reason: "list shorter than viewport should reset offset to 0"},
		// height 10 -> visible 6; 10 sessions -> maxOffset = 4.
		{name: "OffsetNeverPastEnd", sessions: 10, cursor: 9, height: 10, offset: 99, want: 4, reason: "expected offset clamped to 4 (10-6)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestSessionsModel(tc.sessions, tc.cursor, tc.height)
			m.offset = tc.offset
			m = m.clampOffset()
			if m.offset != tc.want {
				t.Fatalf("%s, got %d", tc.reason, m.offset)
			}
		})
	}
}

func TestDelete_FirstDEntersConfirm(t *testing.T) {
	m, _ := newTestModelWithArchive(t, 3, 1, 20)
	m2, _ := m.Update(keyPress("d"))
	got := m2.(SessionSelectionModel)
	if !got.pendingArchive {
		t.Fatalf("first 'd' should enter pendingArchive state")
	}
	if len(got.sessions) != 3 {
		t.Fatalf("list should be unchanged until confirmation, got %d", len(got.sessions))
	}
}

func TestDelete_ConfirmRemovesFromStoreAndList(t *testing.T) {
	m, archived := newTestModelWithArchive(t, 3, 1, 20)
	targetID := m.sessions[1].ID

	m2, _ := m.Update(keyPress("d"))
	m3, _ := m2.(SessionSelectionModel).Update(keyPress("y"))
	got := m3.(SessionSelectionModel)

	if got.pendingArchive {
		t.Fatal("pendingArchive should be cleared after confirm")
	}
	if len(got.sessions) != 2 {
		t.Fatalf("expected 2 sessions after delete, got %d", len(got.sessions))
	}
	for _, s := range got.sessions {
		if s.ID == targetID {
			t.Fatalf("deleted session %s still present", targetID)
		}
	}
	if !archived[targetID] {
		t.Fatalf("expected %s to be archived through the service", targetID)
	}
}

func TestDelete_CancelLeavesListIntact(t *testing.T) {
	m, _ := newTestModelWithArchive(t, 3, 1, 20)
	m2, _ := m.Update(keyPress("d"))
	// Any non-y key should cancel — use "n".
	m3, _ := m2.(SessionSelectionModel).Update(keyPress("n"))
	got := m3.(SessionSelectionModel)

	if got.pendingArchive {
		t.Fatal("pendingArchive should be cleared on cancel")
	}
	if len(got.sessions) != 3 {
		t.Fatalf("list should be untouched, got %d", len(got.sessions))
	}
}

func TestDelete_ArrowCancelsAndDoesNotNavigate(t *testing.T) {
	// While in pendingArchive, an arrow key must cancel *without* moving the
	// cursor — otherwise a user aborting delete would also scroll away from
	// the row they were inspecting.
	m, _ := newTestModelWithArchive(t, 3, 1, 20)
	m2, _ := m.Update(keyPress("d"))
	m3, _ := m2.(SessionSelectionModel).Update(keyPress("down"))
	got := m3.(SessionSelectionModel)

	if got.pendingArchive {
		t.Fatal("arrow should cancel pending delete")
	}
	if got.cursor != 1 {
		t.Fatalf("cursor should not have moved during cancel, got %d", got.cursor)
	}
}

func TestDelete_CursorClampsWhenLastRowDeleted(t *testing.T) {
	m, _ := newTestModelWithArchive(t, 3, 2, 20) // cursor on last row
	m2, _ := m.Update(keyPress("d"))
	m3, _ := m2.(SessionSelectionModel).Update(keyPress("y"))
	got := m3.(SessionSelectionModel)

	if len(got.sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(got.sessions))
	}
	if got.cursor != 1 {
		t.Fatalf("cursor should clamp to new last index 1, got %d", got.cursor)
	}
}

func TestView_UnknownHeightDoesNotRenderFullLongList(t *testing.T) {
	m, _ := newTestModelWithArchive(t, 50, 0, 0)
	view := m.viewString()

	if strings.Contains(view, "1–50 of 50") {
		t.Fatalf("unknown-height view rendered the full long list:\n%s", view)
	}
	if !strings.Contains(view, fmt.Sprintf("1–%d of 50", defaultVisibleSessionRows)) {
		t.Fatalf("unknown-height view should render one conservative page, got:\n%s", view)
	}
}

func TestView_SessionRowsStaySingleLine(t *testing.T) {
	m, _ := newTestModelWithArchive(t, 20, 0, 8)
	m.width = 48
	m.sessions[0].LastPrompt = "<system-reminder>\n## Skills\n\n### Available skills"
	m.sessions[1].LastPrompt = "再详细解释一下方程法、假设法和列表法"

	view := m.viewString()
	if strings.Contains(view, "## Skills") {
		t.Fatalf("prompt preview should collapse embedded newlines, got:\n%s", view)
	}
	if strings.Contains(view, "�") {
		t.Fatalf("prompt preview should not split UTF-8 runes, got:\n%s", view)
	}
	if got := strings.Count(view, "[test"); got != m.visibleRows() {
		t.Fatalf("expected exactly %d rendered session rows, got %d:\n%s", m.visibleRows(), got, view)
	}
}

func TestSessionPromptPreview(t *testing.T) {
	got := sessionPromptPreview("再详细解释一下方程法、假设法和列表法", 12)
	if strings.Contains(got, "�") {
		t.Fatalf("preview split UTF-8 rune: %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("long preview should end with ellipsis, got %q", got)
	}

	got = sessionPromptPreview("<system-reminder>\n## Skills", 80)
	if strings.Contains(got, "\n") {
		t.Fatalf("preview should collapse newlines, got %q", got)
	}
}
