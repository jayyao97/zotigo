package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/jayyao97/zotigo/core/protocol"
)

type transcriptMsg string

func (m *Model) commitLine(s string) tea.Cmd {
	if !m.viewportEnabled {
		return tea.Println(s)
	}
	return func() tea.Msg { return transcriptMsg(s) }
}

func (m *Model) appendTranscript(s string) {
	m.transcript = append(m.transcript, s)
	if m.viewportAutoScroll {
		m.viewport.GotoBottom()
	}
}

func (m *Model) inlineView() tea.View {
	var sb strings.Builder

	if m.thinking && m.currentAsstMsg != "" {
		sb.WriteString(m.currentAsstMsg)
		sb.WriteString("\n")
	} else if m.thinking {
		sb.WriteString(asstMarkerStyle.Render("⏺ ") + "Thinking...\n")
	}

	if m.approving {
		m.writeApprovalView(&sb)
	} else {
		m.writeInputFooter(&sb)
	}

	sb.WriteString("\n")

	return tea.NewView(sb.String())
}

func (m *Model) View() tea.View {
	// Wait for WindowSizeMsg to initialize width.
	if m.width == 0 {
		if !m.viewportEnabled {
			return tea.NewView("")
		}
		return altScreenView("")
	}
	if !m.viewportEnabled {
		return m.inlineView()
	}

	live := m.liveView()
	viewportHeight := m.height - viewLineCount(live)
	if viewportHeight < 1 {
		viewportHeight = 1
	}
	m.viewport.SetWidth(m.width)
	m.viewport.SetHeight(viewportHeight)
	m.updateViewportContent()
	if m.viewportAutoScroll {
		m.viewport.GotoBottom()
	}

	var sb strings.Builder
	sb.WriteString(m.viewport.View())
	if live != "" {
		sb.WriteString("\n")
		sb.WriteString(live)
	}

	return altScreenView(sb.String())
}

func altScreenView(s string) tea.View {
	view := tea.NewView(s)
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	return view
}

func (m *Model) liveView() string {
	var sb strings.Builder

	if m.approving {
		m.writeApprovalView(&sb)
	} else {
		m.writeInputFooter(&sb)
	}

	sb.WriteString("\n")

	return sb.String()
}

func (m *Model) writeApprovalView(sb *strings.Builder) {
	if len(m.pendingApprovals) > 1 {
		current := m.pendingApprovals[m.approvalItemChoice]
		tc := current.ToolCall
		if tc == nil {
			tc = &protocol.ToolCall{Name: current.Name, Arguments: current.Arguments}
		}
		sb.WriteString(warningStyle.Render("⚠ ") + "Execute: " + formatToolCall(tc) + "\n")
		sb.WriteString(blurredChoice.Render(fmt.Sprintf("  Approval %d/%d", m.approvalItemChoice+1, len(m.pendingApprovals))) + "\n")
		if hint := approvalHintForAction(current); hint != "" {
			sb.WriteString(blurredChoice.Render("  " + hint))
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	} else {
		sb.WriteString(warningStyle.Render("⚠ ") + "Execute: " + m.pendingToolName + "\n")
		if len(m.pendingApprovals) == 1 {
			if hint := approvalHintForAction(m.pendingApprovals[0]); hint != "" {
				sb.WriteString(blurredChoice.Render("  " + hint))
				sb.WriteString("\n")
			}
		}
	}

	// Accept line
	if m.approvalChoice == 0 {
		fmt.Fprintf(sb, "  %s %s\n", focusedChoice.Render(">"), focusedChoice.Render("Accept"))
	} else {
		fmt.Fprintf(sb, "    %s\n", blurredChoice.Render("Accept"))
	}

	// Deny line
	denyLabel := denyLabelForApprovalCount(len(m.pendingApprovals))
	if m.approvalChoice == 1 {
		fmt.Fprintf(sb, "  %s %s\n", focusedChoice.Render(">"), focusedChoice.Render(denyLabel))
	} else {
		fmt.Fprintf(sb, "    %s\n", blurredChoice.Render(denyLabel))
	}

	// Feedback input line
	if m.approvalChoice == 2 {
		sb.WriteString("  " + focusedChoice.Render("> ") + m.input.View())
	} else {
		placeholder := "Send feedback..."
		if v := m.input.Value(); v != "" {
			placeholder = v
		}
		fmt.Fprintf(sb, "    %s", blurredChoice.Render(placeholder))
	}
}

func (m *Model) writeInputFooter(sb *strings.Builder) {
	// Only show indicator when auto-approve is on
	if m.autoApprove {
		sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Bold(true).Render(">> Auto-approve"))
		sb.WriteString("\n")
	}

	if status := renderUsageStatus(m.agent); status != "" {
		sb.WriteString(status)
		sb.WriteString("\n")
	}

	// Prefix the first visual line with a prompt arrow, then pad wrapped lines equally.
	prompt := promptStyle.Render("➜ ")
	pad := strings.Repeat(" ", lipgloss.Width(prompt))
	taView := m.input.View()
	lines := strings.Split(taView, "\n")
	for i := range lines {
		if i == 0 {
			lines[i] = prompt + lines[i]
		} else {
			lines[i] = pad + lines[i]
		}
	}
	content := strings.Join(lines, "\n")
	sb.WriteString(inputStyle.Render(content))
}

func (m *Model) transcriptContent() string {
	if !m.thinking {
		return strings.Join(m.transcript, "\n")
	}

	parts := append([]string(nil), m.transcript...)
	if m.currentAsstMsg != "" {
		parts = append(parts, m.currentAsstMsg)
	} else {
		parts = append(parts, asstMarkerStyle.Render("⏺ ")+"Thinking...")
	}
	return strings.Join(parts, "\n")
}

func (m *Model) updateViewportContent() {
	content := wrapViewportContent(m.transcriptContent(), m.viewport.Width())
	if content == m.viewportContent {
		return
	}
	m.viewport.SetContent(content)
	m.viewportContent = content
}

func wrapViewportContent(content string, width int) string {
	if width <= 0 {
		return content
	}
	lines := strings.Split(content, "\n")
	wrapped := make([]string, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			wrapped = append(wrapped, line)
			continue
		}
		for lineWidth := ansi.StringWidth(line); lineWidth > width; lineWidth = ansi.StringWidth(line) {
			wrapped = append(wrapped, ansi.Cut(line, 0, width))
			line = ansi.Cut(line, width, lineWidth)
		}
		wrapped = append(wrapped, line)
	}
	return strings.Join(wrapped, "\n")
}

func (m *Model) logViewportScroll(action string, button tea.MouseButton, beforeY int, beforeBottom bool) {
	if os.Getenv("ZOTIGO_SCROLL_DEBUG") == "" {
		return
	}
	file, err := os.OpenFile("/tmp/zotigo-scroll-debug.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()

	_, _ = fmt.Fprintf(
		file,
		"%s action=%s button=%v before_y=%d after_y=%d before_bottom=%t after_bottom=%t auto=%t height=%d width=%d percent=%.3f lines=%d thinking=%t current_len=%d\n",
		time.Now().Format(time.RFC3339Nano),
		action,
		button,
		beforeY,
		m.viewport.YOffset(),
		beforeBottom,
		m.viewport.AtBottom(),
		m.viewportAutoScroll,
		m.viewport.Height(),
		m.viewport.Width(),
		m.viewport.ScrollPercent(),
		viewportContentLineCount(m.viewportContent),
		m.thinking,
		len(m.currentAsstMsg),
	)
}

func viewportContentLineCount(content string) int {
	if content == "" {
		return 0
	}
	return strings.Count(content, "\n") + 1
}

func viewLineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

func shouldUseViewportRenderer() bool {
	return !isJetBrainsTerminal()
}

func isJetBrainsTerminal() bool {
	for _, value := range []string{os.Getenv("TERMINAL_EMULATOR"), os.Getenv("TERM_PROGRAM")} {
		value = strings.ToLower(value)
		if strings.Contains(value, "jetbrains") || strings.Contains(value, "jediterm") {
			return true
		}
	}
	return false
}
