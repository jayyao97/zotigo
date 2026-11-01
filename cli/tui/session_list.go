package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/jayyao97/zotigo/core/session"
)

var (
	titleStyle        = lipgloss.NewStyle().MarginLeft(2).Bold(true).Foreground(lipgloss.Color("205"))
	itemStyle         = lipgloss.NewStyle().PaddingLeft(4)
	selectedItemStyle = lipgloss.NewStyle().PaddingLeft(2).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62")).Bold(true)
	descStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

type SessionSelectionModel struct {
	sessions       []session.Metadata
	archive        func(id string) error
	cursor         int
	offset         int
	height         int
	width          int
	pendingArchive bool
	statusMsg      string
	ChosenID       string
	quitting       bool
	descriptions   map[string]string
	disabled       map[string]string
}

// NewSessionSelectionModel lists resumable sessions. archive removes a
// session from the list through the session service (history is kept);
// nil makes the picker read-only. descriptions annotate rows, and disabled
// maps a session ID to the reason it cannot be resumed.
func NewSessionSelectionModel(sessions []session.Metadata, archive func(id string) error, descriptions, disabled map[string]string) SessionSelectionModel {
	return SessionSelectionModel{
		sessions:     sessions,
		archive:      archive,
		descriptions: descriptions,
		disabled:     disabled,
	}
}

func shortSessionID(id string) string {
	if len(id) >= 13 {
		return id[5:13]
	}
	return id
}

func (m SessionSelectionModel) Init() tea.Cmd {
	return nil
}

const defaultVisibleSessionRows = 12

// visibleRows returns how many single-line session rows fit on screen given the
// current terminal height, reserving space for the title and footer. Before the
// first WindowSizeMsg, render a conservative page instead of the full list so a
// long resume history cannot overflow the terminal.
func (m SessionSelectionModel) visibleRows() int {
	if m.height <= 0 {
		return defaultVisibleSessionRows
	}
	return max(m.height-4, 3)
}

// confirmArchive archives the session at the cursor and drops it from the
// in-memory list, then fixes up cursor/offset. On error, leaves the list
// untouched and surfaces a status line.
func (m SessionSelectionModel) confirmArchive() SessionSelectionModel {
	m.pendingArchive = false
	if len(m.sessions) == 0 {
		return m
	}
	target := m.sessions[m.cursor]
	if err := m.archive(target.ID); err != nil {
		m.statusMsg = fmt.Sprintf("Archive failed: %v", err)
		return m
	}
	m.sessions = append(m.sessions[:m.cursor], m.sessions[m.cursor+1:]...)
	if m.cursor >= len(m.sessions) {
		m.cursor = max(len(m.sessions)-1, 0)
	}
	m.statusMsg = fmt.Sprintf("Archived session %s.", shortSessionID(target.ID))
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
		m.width = msg.Width
		return m.clampOffset(), nil
	case tea.KeyPressMsg:
		key := msg.String()

		// Archive-confirmation mode intercepts everything: y/Y confirms,
		// ctrl+c still quits the program, any other key cancels.
		if m.pendingArchive {
			switch key {
			case "y", "Y":
				return m.confirmArchive(), nil
			case "ctrl+c":
				m.quitting = true
				return m, tea.Quit
			default:
				m.pendingArchive = false
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
			if len(m.sessions) == 0 || m.archive == nil {
				return m, nil
			}
			m.pendingArchive = true
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
			if reason := m.disabled[selected.ID]; reason != "" {
				m.statusMsg = reason
				return m, nil
			}
			m.ChosenID = selected.ID
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m SessionSelectionModel) View() tea.View {
	return tea.NewView(m.viewString())
}

func (m SessionSelectionModel) viewString() string {
	if m.ChosenID != "" {
		return ""
	}
	if m.quitting {
		return "Bye!\n"
	}

	s := titleStyle.Render("Resume a Session") + "\n\n"

	if len(m.sessions) == 0 {
		s += itemStyle.Render("No previous sessions found for this project.") + "\n"
		s += itemStyle.Render("Press 'q' to quit, or run without --resume to start a new one.") + "\n"
		return s
	}

	visible := m.visibleRows()
	start := m.offset
	end := min(start+visible, len(m.sessions))

	for i := start; i < end; i++ {
		sess := m.sessions[i]
		cursor := "  "
		style := itemStyle

		if m.cursor == i {
			cursor = "> "
			style = selectedItemStyle
		}

		// Format Time: "Mon Jan 02 15:04"
		ts := sess.UpdatedAt.Format(time.Kitchen)
		if time.Since(sess.UpdatedAt) > 24*time.Hour {
			ts = sess.UpdatedAt.Format("Jan 02 15:04")
		}

		prompt := sessionPromptPreview(sess.LastPrompt, m.previewWidth())
		if description := m.descriptions[sess.ID]; description != "" {
			prompt = description + " · " + prompt
		}

		line := fmt.Sprintf("%s [%s] %s", ts, shortSessionID(sess.ID), prompt)
		if reason := m.disabled[sess.ID]; reason != "" {
			line += " [UNAVAILABLE: " + reason + "]"
			style = style.Foreground(lipgloss.Color("240"))
		}

		s += style.Render(fmt.Sprintf("%s%s", cursor, line)) + "\n"
	}

	s += "\n"

	switch {
	case m.pendingArchive:
		target := m.sessions[m.cursor]
		s += lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")).Bold(true).
			Render(fmt.Sprintf("  Archive session [%s]? (y/N)", shortSessionID(target.ID))) + "\n"
	case m.statusMsg != "":
		s += descStyle.Render("  "+m.statusMsg) + "\n"
	case visible > 0 && len(m.sessions) > visible:
		s += descStyle.Render(fmt.Sprintf("  %d–%d of %d  •  ↑/↓ PgUp/PgDn g/G • Enter select • d archive • Esc quit",
			start+1, end, len(m.sessions))) + "\n"
	default:
		s += descStyle.Render("Use ↑/↓ to navigate • Enter to select • d to archive • Esc to quit") + "\n"
	}
	return s
}

func (m SessionSelectionModel) previewWidth() int {
	if m.width <= 0 {
		return 44
	}
	// Indent + cursor + timestamp + id + spaces leave roughly this much
	// horizontal room for the prompt on a normal terminal.
	return max(m.width-28, 12)
}

func sessionPromptPreview(prompt string, maxWidth int) string {
	prompt = strings.Join(strings.Fields(prompt), " ")
	if prompt == "" {
		prompt = "(No messages)"
	}
	return truncateDisplay(prompt, maxWidth)
}

func truncateDisplay(s string, maxWidth int) string {
	if maxWidth <= 0 || lipgloss.Width(s) <= maxWidth {
		return s
	}
	const ellipsis = "..."
	if maxWidth <= len(ellipsis) {
		return ellipsis[:maxWidth]
	}
	limit := maxWidth - len(ellipsis)
	var b strings.Builder
	width := 0
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError && size == 0 {
			break
		}
		ch := s[:size]
		w := lipgloss.Width(ch)
		if width+w > limit {
			break
		}
		b.WriteString(ch)
		width += w
		s = s[size:]
	}
	return b.String() + ellipsis
}
