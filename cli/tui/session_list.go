package tui

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/jayyao97/zotigo/core/session"
)

var (
	titleStyle        = lipgloss.NewStyle().MarginLeft(2).Bold(true).Foreground(lipgloss.Color("205"))
	itemStyle         = lipgloss.NewStyle().PaddingLeft(4)
	selectedItemStyle = lipgloss.NewStyle().PaddingLeft(2).Foreground(lipgloss.Color("170")).Bold(true)
	descStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	lockedStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true) // Red for locked
)

type SessionSelectionModel struct {
	sessions      []session.Metadata
	manager       *session.Manager
	cursor        int
	offset        int
	height        int
	pendingDelete bool
	statusMsg     string
	ChosenID      string
	quitting      bool
}

func NewSessionSelectionModel(sessions []session.Metadata, mgr *session.Manager) SessionSelectionModel {
	return SessionSelectionModel{
		sessions: sessions,
		manager:  mgr,
		cursor:   0,
	}
}

func (m SessionSelectionModel) Init() tea.Cmd {
	return nil
}

// visibleRows returns how many session rows fit on screen given the current
// terminal height, reserving space for the title (2 lines) and footer (2 lines).
// Returns 0 to signal "height unknown, render everything" (first frame before
// WindowSizeMsg arrives).
func (m SessionSelectionModel) visibleRows() int {
	if m.height <= 0 {
		return 0
	}
	return max(m.height-4, 3)
}

// confirmDelete removes the session at the cursor from the store and the
// in-memory list, then fixes up cursor/offset. On error, leaves the list
// untouched and surfaces a status line.
func (m SessionSelectionModel) confirmDelete() SessionSelectionModel {
	m.pendingDelete = false
	if len(m.sessions) == 0 {
		return m
	}
	target := m.sessions[m.cursor]
	if err := m.manager.Delete(target.ID); err != nil {
		m.statusMsg = fmt.Sprintf("Delete failed: %v", err)
		return m
	}
	m.sessions = append(m.sessions[:m.cursor], m.sessions[m.cursor+1:]...)
	if m.cursor >= len(m.sessions) {
		m.cursor = max(len(m.sessions)-1, 0)
	}
	m.statusMsg = fmt.Sprintf("Deleted session %s.", target.ID[5:13])
	return m.clampOffset()
}

func (m SessionSelectionModel) clampOffset() SessionSelectionModel {
	visible := m.visibleRows()
	if visible == 0 || len(m.sessions) <= visible {
		m.offset = 0
		return m
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	} else if m.cursor >= m.offset+visible {
		m.offset = m.cursor - visible + 1
	}
	maxOffset := len(m.sessions) - visible
	if m.offset > maxOffset {
		m.offset = maxOffset
	}
	if m.offset < 0 {
		m.offset = 0
	}
	return m
}

func (m SessionSelectionModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.height = msg.Height
		return m.clampOffset(), nil
	case tea.KeyPressMsg:
		key := msg.String()

		// Delete-confirmation mode intercepts everything: y/Y confirms,
		// ctrl+c still quits the program, any other key cancels.
		if m.pendingDelete {
			switch key {
			case "y", "Y":
				return m.confirmDelete(), nil
			case "ctrl+c":
				m.quitting = true
				return m, tea.Quit
			default:
				m.pendingDelete = false
				m.statusMsg = ""
				return m, nil
			}
		}

		// Any keypress clears a stale status line.
		m.statusMsg = ""

		switch key {
		case "ctrl+c", "q", "esc":
			m.quitting = true
			return m, tea.Quit
		case "d":
			if len(m.sessions) == 0 {
				return m, nil
			}
			if m.manager.IsLocked(m.sessions[m.cursor].ID) {
				m.statusMsg = "Cannot delete a locked session (in use by another process)."
				return m, nil
			}
			m.pendingDelete = true
			return m, nil
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
			return m.clampOffset(), nil
		case "down", "j":
			if m.cursor < len(m.sessions)-1 {
				m.cursor++
			}
			return m.clampOffset(), nil
		case "pgup":
			step := m.visibleRows()
			if step <= 0 {
				step = 10
			}
			m.cursor -= step
			if m.cursor < 0 {
				m.cursor = 0
			}
			return m.clampOffset(), nil
		case "pgdown":
			step := m.visibleRows()
			if step <= 0 {
				step = 10
			}
			m.cursor += step
			if m.cursor > len(m.sessions)-1 {
				m.cursor = len(m.sessions) - 1
			}
			return m.clampOffset(), nil
		case "home", "g":
			m.cursor = 0
			return m.clampOffset(), nil
		case "end", "G":
			m.cursor = max(len(m.sessions)-1, 0)
			return m.clampOffset(), nil
		case "enter":
			if len(m.sessions) == 0 {
				return m, nil
			}
			selected := m.sessions[m.cursor]
			// Check lock again just in case
			if m.manager.IsLocked(selected.ID) {
				// Flash message or ignore? For now ignore.
				return m, nil
			}
			m.ChosenID = selected.ID
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m SessionSelectionModel) View() tea.View {
	if m.ChosenID != "" {
		return tea.NewView("")
	}
	if m.quitting {
		return tea.NewView("Bye!\n")
	}

	s := titleStyle.Render("Resume a Session") + "\n\n"

	if len(m.sessions) == 0 {
		s += itemStyle.Render("No previous sessions found for this project.") + "\n"
		s += itemStyle.Render("Press 'q' to quit, or run without --resume to start a new one.") + "\n"
		return tea.NewView(s)
	}

	start, end := 0, len(m.sessions)
	visible := m.visibleRows()
	if visible > 0 && len(m.sessions) > visible {
		start = m.offset
		end = min(start+visible, len(m.sessions))
	}

	for i := start; i < end; i++ {
		sess := m.sessions[i]
		cursor := "  "
		style := itemStyle

		if m.cursor == i {
			cursor = "> "
			style = selectedItemStyle
		}

		isLocked := m.manager.IsLocked(sess.ID)

		// Format Time: "Mon Jan 02 15:04"
		ts := sess.UpdatedAt.Format(time.Kitchen)
		if time.Since(sess.UpdatedAt) > 24*time.Hour {
			ts = sess.UpdatedAt.Format("Jan 02 15:04")
		}

		// Prompt Preview
		prompt := sess.LastPrompt
		if prompt == "" {
			prompt = "(No messages)"
		}
		if len(prompt) > 40 {
			prompt = prompt[:37] + "..."
		}

		line := fmt.Sprintf("%s [%s] %s", ts, sess.ID[5:13], prompt)

		if isLocked {
			line += " [LOCKED]"
			if m.cursor == i {
				// Highlight locked item differently if selected
				style = style.Foreground(lipgloss.Color("124"))
			} else {
				style = style.Foreground(lipgloss.Color("240")) // Dim locked items
			}
		}

		s += style.Render(fmt.Sprintf("%s%s", cursor, line)) + "\n"
	}

	s += "\n"

	switch {
	case m.pendingDelete:
		target := m.sessions[m.cursor]
		s += lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")).Bold(true).
			Render(fmt.Sprintf("  Delete session [%s]? (y/N)", target.ID[5:13])) + "\n"
	case m.statusMsg != "":
		s += descStyle.Render("  "+m.statusMsg) + "\n"
	case visible > 0 && len(m.sessions) > visible:
		s += descStyle.Render(fmt.Sprintf("  %d–%d of %d  •  ↑/↓ PgUp/PgDn g/G • Enter select • d delete • Esc quit",
			start+1, end, len(m.sessions))) + "\n"
	default:
		s += descStyle.Render("Use ↑/↓ to navigate • Enter to select • d to delete • Esc to quit") + "\n"
	}
	return tea.NewView(s)
}
