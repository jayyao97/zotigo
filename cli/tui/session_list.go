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
	sessions []session.Metadata
	manager  *session.Manager
	cursor   int
	ChosenID string
	quitting bool
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

func (m SessionSelectionModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.quitting = true
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.sessions)-1 {
				m.cursor++
			}
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

	for i, sess := range m.sessions {
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

	s += descStyle.Render("\nUse ↑/↓ to navigate • Enter to select • Esc to quit") + "\n"
	return tea.NewView(s)
}
