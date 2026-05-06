package tui

import (
	"context"
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

// newTestModelWithStore creates a model backed by a real FileStore in tmpDir.
// Each of the n sessions is persisted so Delete() actually has something to
// remove. Session IDs are zero-padded to satisfy the ID[5:13] slice in the
// View/status messages.
func newTestModelWithStore(t *testing.T, n, cursor, height int) (SessionSelectionModel, *session.Manager) {
	t.Helper()
	tmp := t.TempDir()
	store, err := session.NewFileStore(tmp)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	mgr := session.NewManagerWithStore(store)
	metas := make([]session.Metadata, 0, n)
	for i := range n {
		// ID must be at least 13 chars for the ID[5:13] slice used in the View.
		id := fmt.Sprintf("sess_test%04d", i)
		meta := session.Metadata{
			ID:               id,
			WorkingDirectory: "/tmp",
			UpdatedAt:        time.Now(),
		}
		sess := &session.Session{Metadata: meta}
		if err := store.Put(context.Background(), sess); err != nil {
			t.Fatalf("put: %v", err)
		}
		metas = append(metas, meta)
	}
	m := SessionSelectionModel{
		sessions: metas,
		manager:  mgr,
		cursor:   cursor,
		height:   height,
	}
	return m, mgr
}

func keyPress(s string) tea.KeyPressMsg {
	if len(s) == 1 {
		return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
	}
	return tea.KeyPressMsg{Text: s}
}

func TestVisibleRows_UnknownHeight(t *testing.T) {
	m := newTestSessionsModel(50, 0, 0)
	if got := m.visibleRows(); got != defaultVisibleSessionRows {
		t.Fatalf("unknown height should render conservative page, got %d", got)
	}
}

func TestVisibleRows_ReservesHeaderFooter(t *testing.T) {
	m := newTestSessionsModel(50, 0, 30)
	if got := m.visibleRows(); got != 26 {
		t.Fatalf("height 30 minus 4 overhead = 26, got %d", got)
	}
}

func TestVisibleRows_TinyTerminalFloor(t *testing.T) {
	m := newTestSessionsModel(50, 0, 4)
	if got := m.visibleRows(); got != 3 {
		t.Fatalf("visible rows should floor at 3 on tiny terminals, got %d", got)
	}
}

func TestClampOffset_CursorBelowWindowScrollsDown(t *testing.T) {
	// height 10 -> visible 6. Cursor at 20 should make offset = 15 so cursor
	// sits on the last visible row.
	m := newTestSessionsModel(50, 20, 10).clampOffset()
	if m.offset != 15 {
		t.Fatalf("expected offset 15, got %d", m.offset)
	}
}

func TestClampOffset_CursorAboveWindowScrollsUp(t *testing.T) {
	// Start scrolled down, then jump cursor to the top.
	m := newTestSessionsModel(50, 2, 10)
	m.offset = 30
	m = m.clampOffset()
	if m.offset != 2 {
		t.Fatalf("expected offset to follow cursor up to 2, got %d", m.offset)
	}
}

func TestClampOffset_CursorInsideWindowNoop(t *testing.T) {
	m := newTestSessionsModel(50, 20, 10)
	m.offset = 18
	m = m.clampOffset()
	if m.offset != 18 {
		t.Fatalf("cursor already visible, offset should not move; got %d", m.offset)
	}
}

func TestClampOffset_ShorterThanViewport(t *testing.T) {
	// 3 sessions, viewport fits them all -> offset must be 0.
	m := newTestSessionsModel(3, 2, 30)
	m.offset = 99 // garbage
	m = m.clampOffset()
	if m.offset != 0 {
		t.Fatalf("list shorter than viewport should reset offset to 0, got %d", m.offset)
	}
}

func TestClampOffset_OffsetNeverPastEnd(t *testing.T) {
	// height 10 -> visible 6; 10 sessions -> maxOffset = 4.
	m := newTestSessionsModel(10, 9, 10)
	m.offset = 99
	m = m.clampOffset()
	if m.offset != 4 {
		t.Fatalf("expected offset clamped to 4 (10-6), got %d", m.offset)
	}
}

func TestDelete_FirstDEntersConfirm(t *testing.T) {
	m, _ := newTestModelWithStore(t, 3, 1, 20)
	m2, _ := m.Update(keyPress("d"))
	got := m2.(SessionSelectionModel)
	if !got.pendingDelete {
		t.Fatalf("first 'd' should enter pendingDelete state")
	}
	if len(got.sessions) != 3 {
		t.Fatalf("list should be unchanged until confirmation, got %d", len(got.sessions))
	}
}

func TestDelete_ConfirmRemovesFromStoreAndList(t *testing.T) {
	m, mgr := newTestModelWithStore(t, 3, 1, 20)
	targetID := m.sessions[1].ID

	m2, _ := m.Update(keyPress("d"))
	m3, _ := m2.(SessionSelectionModel).Update(keyPress("y"))
	got := m3.(SessionSelectionModel)

	if got.pendingDelete {
		t.Fatal("pendingDelete should be cleared after confirm")
	}
	if len(got.sessions) != 2 {
		t.Fatalf("expected 2 sessions after delete, got %d", len(got.sessions))
	}
	for _, s := range got.sessions {
		if s.ID == targetID {
			t.Fatalf("deleted session %s still present", targetID)
		}
	}
	// Store must also have dropped it.
	if _, err := mgr.Load(targetID); err == nil {
		t.Fatalf("expected Load(%s) to fail after delete", targetID)
	}
}

func TestDelete_CancelLeavesListIntact(t *testing.T) {
	m, _ := newTestModelWithStore(t, 3, 1, 20)
	m2, _ := m.Update(keyPress("d"))
	// Any non-y key should cancel — use "n".
	m3, _ := m2.(SessionSelectionModel).Update(keyPress("n"))
	got := m3.(SessionSelectionModel)

	if got.pendingDelete {
		t.Fatal("pendingDelete should be cleared on cancel")
	}
	if len(got.sessions) != 3 {
		t.Fatalf("list should be untouched, got %d", len(got.sessions))
	}
}

func TestDelete_ArrowCancelsAndDoesNotNavigate(t *testing.T) {
	// While in pendingDelete, an arrow key must cancel *without* moving the
	// cursor — otherwise a user aborting delete would also scroll away from
	// the row they were inspecting.
	m, _ := newTestModelWithStore(t, 3, 1, 20)
	m2, _ := m.Update(keyPress("d"))
	m3, _ := m2.(SessionSelectionModel).Update(keyPress("down"))
	got := m3.(SessionSelectionModel)

	if got.pendingDelete {
		t.Fatal("arrow should cancel pending delete")
	}
	if got.cursor != 1 {
		t.Fatalf("cursor should not have moved during cancel, got %d", got.cursor)
	}
}

func TestDelete_CursorClampsWhenLastRowDeleted(t *testing.T) {
	m, _ := newTestModelWithStore(t, 3, 2, 20) // cursor on last row
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
	m, _ := newTestModelWithStore(t, 50, 0, 0)
	view := m.viewString()

	if strings.Contains(view, "1–50 of 50") {
		t.Fatalf("unknown-height view rendered the full long list:\n%s", view)
	}
	if !strings.Contains(view, fmt.Sprintf("1–%d of 50", defaultVisibleSessionRows)) {
		t.Fatalf("unknown-height view should render one conservative page, got:\n%s", view)
	}
}

func TestView_SessionRowsStaySingleLine(t *testing.T) {
	m, _ := newTestModelWithStore(t, 20, 0, 8)
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
