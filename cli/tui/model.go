package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/session"
)

var (
	focusedButtonStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true).Border(lipgloss.RoundedBorder()).Padding(0, 1)
	blurredButtonStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Border(lipgloss.RoundedBorder()).Padding(0, 1)
	warningStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("202")).Bold(true)
	inputStyle         = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("62")).Padding(0, 1)
	promptStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true)
	assistantStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("86"))
	userStyle          = lipgloss.NewStyle().Foreground(lipgloss.Color("212"))
	headerStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Padding(0, 1).Bold(true)
)

type Model struct {
	agent           *agent.Agent
	sessionMgr      *session.Manager
	sessionID       string
	ctx             context.Context
	input           textarea.Model
	currentAsstMsg  string
	thinking        bool
	approving       bool
	approvalChoice  int
	pendingToolName string
	pendingToolArgs string
	err             error
	eventCh         <-chan protocol.Event
	width           int
	height          int
	initialPrinted  bool
	kittyChecked    bool
}

type streamReadyMsg <-chan protocol.Event
type errMsg error

func NewModel(ag *agent.Agent, sessMgr *session.Manager, sessID string) Model {
	ta := textarea.New()
	ta.Placeholder = "Ask Zotigo..."
	ta.Focus()
	ta.CharLimit = 0
	ta.MaxHeight = 0
	ta.SetHeight(1)
	ta.ShowLineNumbers = false

	// Do not use textarea's built-in prompt (it appears on every wrapped visual line).
	ta.Prompt = ""

	// Custom styles
	styles := ta.Styles()
	styles.Focused.Base = lipgloss.NewStyle()
	styles.Focused.Text = lipgloss.NewStyle()
	styles.Focused.CursorLine = lipgloss.NewStyle()
	styles.Blurred.Base = lipgloss.NewStyle()
	styles.Blurred.Text = lipgloss.NewStyle()
	styles.Blurred.CursorLine = lipgloss.NewStyle()
	ta.SetStyles(styles)

	return Model{
		agent:      ag,
		sessionMgr: sessMgr,
		sessionID:  sessID,
		ctx:        context.Background(),
		input:      ta,
	}
}

func (m Model) Init() tea.Cmd {
	return textarea.Blink
}

func (m Model) printInitialHistory(isRepaint bool) tea.Cmd {
	snap := m.agent.Snapshot()
	history := snap.History
	const maxHistory = 100
	truncated := false
	if len(history) > maxHistory {
		history = history[len(history)-maxHistory:]
		truncated = true
	}

	headerText := "Welcome to Zotigo CLI"
	if len(snap.History) > 0 {
		headerText = "Welcome back to Zotigo CLI"
	}
	
	header := headerStyle.Render("── " + headerText + " ──")
	
	var cmds []tea.Cmd
	cmds = append(cmds, tea.Println(header))
	if truncated {
		cmds = append(cmds, tea.Println(headerStyle.Render("── (...earlier messages truncated...) ──")))
	}
	cmds = append(cmds, tea.Println(""))

	for _, msg := range history {
		if str, ok := renderMessage(msg); ok {
			cmds = append(cmds, tea.Println(str))
		}
	}
	cmds = append(cmds, tea.Println(""))
	return tea.Sequence(cmds...)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

		// textarea width = screen width - borders - padding - prompt width - safety margin
		frameWidth := inputStyle.GetHorizontalFrameSize()
		promptWidth := lipgloss.Width(promptStyle.Render("➜ "))
		inputWidth := m.width - frameWidth - promptWidth - 1
		if inputWidth < 1 {
			inputWidth = 1
		}
		m.input.SetWidth(inputWidth)
		m.input.SetHeight(m.inputLineCount())

		if !m.initialPrinted {
			m.initialPrinted = true
			return m, m.printInitialHistory(false)
		}
		
		return m, tea.Sequence(
			tea.ClearScreen, 
			m.printInitialHistory(true),
		)

	case tea.KeyboardEnhancementsMsg:
		if !m.kittyChecked {
			m.kittyChecked = true
			return m, func() tea.Msg { return tea.ClearScreen() }
		}
		return m, nil

	case tea.PasteMsg:
		if token, ok := m.handlePaste(msg.Content); ok {
			m.input.InsertString(" " + token + " ")
			m.input.SetHeight(m.inputLineCount())
			return m, nil
		}

	case tea.KeyPressMsg:
		keyStr := msg.String()

		if keyStr == "=" || keyStr == ";" || keyStr == "u" || keyStr == "[" {
			if !m.input.Focused() || m.thinking {
				return m, nil
			}
		}

		if keyStr == "ctrl+c" || keyStr == "esc" {
			m.saveSession()
			return m, tea.Quit
		}

		if m.approving {
			if keyStr == "enter" {
				return m.submitApproval(m.approvalChoice == 0)
			}
			switch keyStr {
			case "left", "h":
				m.approvalChoice = 0
			case "right", "l", "tab":
				if m.approvalChoice == 0 {
					m.approvalChoice = 1
				} else {
					m.approvalChoice = 0
				}
			}
			return m, nil
		}

		if keyStr == "ctrl+v" {
			if imgPath, ok := m.pasteImageFromClipboard(); ok {
				m.input.InsertString(fmt.Sprintf("@%s ", imgPath))
				m.input.SetHeight(m.inputLineCount())
				return m, nil
			}
		}

		if keyStr == "shift+enter" || keyStr == "ctrl+j" {
			m.input.InsertString("\n")
			m.input.SetHeight(m.inputLineCount())
			return m, nil
		}

		if keyStr == "enter" {
			if m.thinking {
				return m, nil
			}
			v := m.input.Value()
			if strings.TrimSpace(v) == "" {
				return m, nil
			}

			// Parse input for @file references
			var msg protocol.Message
			msg.Role = protocol.RoleUser
			msg.CreatedAt = time.Now()

			re := regexp.MustCompile(`(?:^|\s)@([^\s]+)`) // Corrected escaping for backslash in regex
			matches := re.FindAllStringSubmatchIndex(v, -1)

			lastIndex := 0
			for _, match := range matches {
				start, end := match[0], match[1]
				path := v[match[2]:match[3]]

				if start > lastIndex {
					msg.Content = append(msg.Content, protocol.ContentPart{
						Type: protocol.ContentTypeText,
						Text: v[lastIndex:start],
					})
				}

				isImage := false
			if isImagePath(path) {
				if _, err := os.Stat(path); err == nil {
					data, err := os.ReadFile(path)
					if err == nil {
						mime := "image/png"
						ext := strings.ToLower(filepath.Ext(path))
						if ext == ".jpg" || ext == ".jpeg" {
							mime = "image/jpeg"
						} else if ext == ".webp" {
							mime = "image/webp"
						}
					msg.Content = append(msg.Content, protocol.ContentPart{
							Type: protocol.ContentTypeImage,
							Image: &protocol.MediaPart{
									Data:      data,
									MediaType: mime,
								},
							})
						isImage = true
						}
				}
			}
			
				if !isImage {
					msg.Content = append(msg.Content, protocol.ContentPart{
						Type: protocol.ContentTypeText,
						Text: v[start:end],
					})
				}
				lastIndex = end
			}

			if lastIndex < len(v) {
				msg.Content = append(msg.Content, protocol.ContentPart{
					Type: protocol.ContentTypeText,
					Text: v[lastIndex:],
				})
			}

			m.input.Reset()
			m.thinking = true
			m.currentAsstMsg = ""

			userMsgStr, _ := renderMessage(msg)
			return m, tea.Batch(tea.Println(userMsgStr), m.startRun(msg))
		}
	case streamReadyMsg:
		m.eventCh = msg
		m.currentAsstMsg = ""
		return m, waitForNextEvent(m.eventCh)

	case protocol.Event:
		switch msg.Type {
		case protocol.EventTypeContentDelta:
			if msg.ContentPartDelta != nil {
				m.currentAsstMsg += msg.ContentPartDelta.Text
			}
		case protocol.EventTypeToolCallDelta:
			if msg.ToolCallDelta != nil && msg.ToolCallDelta.Name != "" {
				m.currentAsstMsg += fmt.Sprintf("\n[Call Tool: %s...]", msg.ToolCallDelta.Name)
			}
		case protocol.EventTypeFinish:
			m.thinking = false

			formattedMsg := ""
			snap := m.agent.Snapshot()
			if len(snap.History) > 0 {
				lastMsg := snap.History[len(snap.History)-1]
				if lastMsg.Role == protocol.RoleAssistant {
					if str, ok := renderMessage(lastMsg); ok {
						formattedMsg = str
					}
				}
			}
			if formattedMsg == "" && m.currentAsstMsg != "" {
				formattedMsg = "\n" + assistantStyle.Render("Zotigo: ") + m.currentAsstMsg
			}

			if msg.FinishReason == "need_approval" {
				m.approving = true
				m.approvalChoice = 0

				var sb strings.Builder
				if len(snap.PendingActions) > 1 {
					sb.WriteString(fmt.Sprintf("%d tools:\n", len(snap.PendingActions)))
				}
				for _, act := range snap.PendingActions {
					args := act.Arguments
					if len(args) > 50 {
						args = args[:47] + "..."
					}
					sb.WriteString(fmt.Sprintf("• %s %s\n", act.Name, args))
				}
				m.pendingToolName = strings.TrimSpace(sb.String())
				m.pendingToolArgs = ""

				m.currentAsstMsg = ""
				m.saveSession()
				if formattedMsg != "" {
					return m, tea.Println(formattedMsg)
				}
				return m, nil
			}

			m.currentAsstMsg = ""
			m.eventCh = nil
			m.saveSession()
			if formattedMsg != "" {
				return m, tea.Println(formattedMsg)
			}
			return m, nil

		case protocol.EventTypeError:
			m.err = msg.Error
			m.thinking = false
			errStr := fmt.Sprintf("\n❌ Error: %v", msg.Error)
			return m, tea.Println(errStr)
		}
		return m, waitForNextEvent(m.eventCh)

	case errMsg:
		if strings.Contains(msg.Error(), "agent is not paused") {
			return m, nil
		}
		m.err = msg
		m.thinking = false
		errStr := fmt.Sprintf("\n❌ System Error: %v", msg)
		return m, tea.Println(errStr)
	}

	if !m.approving {
		// Before handling key input, predict line wrap and pre-grow the input height.
		if k, ok := msg.(tea.KeyPressMsg); ok && len(k.Text) > 0 {
			w := m.input.Width()
			if w > 0 {
				val := m.input.Value()
				// Compute the width of the current last line.
				lastLineLen := 0
				if idx := strings.LastIndex(val, "\n"); idx >= 0 {
					lastLineLen = lipgloss.Width(val[idx+1:])
				} else {
					lastLineLen = lipgloss.Width(val)
				}
				// If adding the next char exceeds width, allocate one more line in advance.
				if lastLineLen+lipgloss.Width(k.Text) >= w {
					m.input.SetHeight(m.inputLineCount() + 1)
				}
			}
		}

		m.input, cmd = m.input.Update(msg)
		cmds = append(cmds, cmd)

		m.input.SetHeight(m.inputLineCount())
	}

	return m, tea.Batch(cmds...)
}

func (m Model) startRun(msg protocol.Message) tea.Cmd {
	return func() tea.Msg {
		ch, err := m.agent.RunMessage(m.ctx, msg)
		if err != nil {
			return errMsg(err)
		}
		return streamReadyMsg(ch)
	}
}

func (m Model) submitApproval(approved bool) (Model, tea.Cmd) {
	m.approving = false
	status := "✅ Approved"
	if !approved {
		status = "🚫 Denied"
	}
	approvalMsg := fmt.Sprintf("\n%s\n%s", m.pendingToolName, status)
	m.thinking = true

	cmd := func() tea.Msg {
		var ch <-chan protocol.Event
		var err error
		if approved {
			ch, err = m.agent.ApproveAndExecutePendingActions(m.ctx)
		} else {
			snap := m.agent.Snapshot()
			var outputs []protocol.ToolResult
			for _, act := range snap.PendingActions {
				outputs = append(outputs, protocol.ToolResult{
					ToolCallID: act.ToolCallID,
					Type:       protocol.ToolResultTypeExecutionDenied,
					Reason:     "User denied in TUI",
				})
			}
			ch, err = m.agent.SubmitToolOutputs(m.ctx, outputs)
		}
		if err != nil {
			return errMsg(err)
		}
		return streamReadyMsg(ch)
	}

	return m, tea.Batch(tea.Println(approvalMsg), cmd)
}

func waitForNextEvent(ch <-chan protocol.Event) tea.Cmd {
	return func() tea.Msg {
		if ch == nil {
			return nil
		}
		evt, ok := <-ch
		if !ok {
			return nil
		}
		return evt
	}
}

func (m Model) View() tea.View {
	// Wait for WindowSizeMsg to initialize width.
	if m.width == 0 {
		return tea.NewView("")
	}

	var sb strings.Builder

	if m.thinking && m.currentAsstMsg != "" {
		sb.WriteString(assistantStyle.Render("Zotigo: "))
		sb.WriteString(m.currentAsstMsg)
		sb.WriteString("\n")
	} else if m.thinking {
		sb.WriteString("Thinking...\n")
	}

	if m.approving {
		yesStyle := blurredButtonStyle
		noStyle := blurredButtonStyle
		if m.approvalChoice == 0 {
			yesStyle = focusedButtonStyle
		} else {
			noStyle = focusedButtonStyle
		}
		buttons := lipgloss.JoinHorizontal(lipgloss.Top,
			yesStyle.Render("Yes (Run All)"),
			"  ",
			noStyle.Render("No (Skip All)"),
		)

		info := warningStyle.Render("⚠️  Execute:")
		list := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(m.pendingToolName)

		sb.WriteString(fmt.Sprintf("%s\n%s\n\n%s", info, list, buttons))
	} else {
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

	sb.WriteString("\n")

	return tea.NewView(sb.String())
}

func (m Model) saveSession() {
	if m.sessionMgr == nil || m.sessionID == "" {
		return
	}

	snap := m.agent.Snapshot()

	if m.currentAsstMsg != "" {
		partialMsg := protocol.NewAssistantMessage(m.currentAsstMsg)
		snap.History = append(snap.History, partialMsg)
	}

	lastPrompt := ""
	if len(snap.History) > 0 {
		lastMsg := snap.History[len(snap.History)-1]
		if lastMsg.Role == protocol.RoleUser {
			lastPrompt = lastMsg.String()
		} else {
			for i := len(snap.History) - 1; i >= 0; i-- {
				if snap.History[i].Role == protocol.RoleUser {
					lastPrompt = snap.History[i].String()
					break
				}
			}
		}
	}

	sess, err := m.sessionMgr.Load(m.sessionID)
	if err == nil {
		sess.AgentSnapshot = snap
		if lastPrompt != "" {
			sess.LastPrompt = lastPrompt
		}
		m.sessionMgr.Save(sess)
	}
}

func renderMessage(msg protocol.Message) (string, bool) {
	if msg.Role == protocol.RoleTool {
		return "", false
	}

	role := "User"
	style := userStyle
	if msg.Role == protocol.RoleAssistant {
		role = "Zotigo"
		style = assistantStyle
	}

	content := ""
	if msg.Role == protocol.RoleAssistant {
		var textParts []string
		var toolNames []string

		for _, p := range msg.Content {
			switch p.Type {
			case protocol.ContentTypeText, protocol.ContentTypeReasoning:
				textParts = append(textParts, p.Text)
			case protocol.ContentTypeToolCall:
				if p.ToolCall != nil {
					toolNames = append(toolNames, p.ToolCall.Name)
				}
			}
		}

		content = strings.Join(textParts, "")
		if len(toolNames) > 0 {
			if content != "" {
				content += "\n"
			}
			content += fmt.Sprintf("🛠️  Called: %s", strings.Join(toolNames, ", "))
		}
	} else {
		content = msg.String()
	}

	if content == "" {
		return "", false
	}

	return fmt.Sprintf("\n%s: %s", style.Render(role), content), true
}

func (m Model) inputLineCount() int {
	val := m.input.Value()
	if val == "" {
		return 1
	}

	w := m.input.Width()
	if w < 1 {
		w = 1
	}

	lines := 0
	lastLineRemainder := 0
	for _, line := range strings.Split(val, "\n") {
		if line == "" {
			lines++
			lastLineRemainder = w
			continue
		}
		lineWidth := lipgloss.Width(line)
		visualLines := (lineWidth + w - 1) / w
		if visualLines < 1 {
			visualLines = 1
		}
		lines += visualLines
		lastLineRemainder = w - (lineWidth % w)
		if lastLineRemainder == w {
			lastLineRemainder = 0
		}
	}

	if lines < 1 {
		lines = 1
	}
	// Reserve one more line when the current visual line is full.
	if lastLineRemainder == 0 {
		lines++
	}
	return lines
}

func isImagePath(s string) bool {
	ext := strings.ToLower(filepath.Ext(s))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".bmp":
		return true
	}
	return false
}

func (m *Model) handlePaste(content string) (string, bool) {
	trimmed := strings.TrimSpace(content)

	if !strings.Contains(trimmed, "\n") && isImagePath(trimmed) {
		if newPath, err := m.storeImage(trimmed); err == nil {
			return fmt.Sprintf("@%s", newPath), true
		}
	}

	return "", false
}

func (m *Model) storeImage(srcPath string) (string, error) {
	// Save to current directory's .zotigo folder for shorter paths
	uploadDir := ".zotigo/uploads"
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		return "", err
	}

	ext := filepath.Ext(srcPath)
	filename := fmt.Sprintf("img_%d%s", time.Now().UnixNano(), ext)
	destPath := filepath.Join(uploadDir, filename)

	src, err := os.Open(srcPath)
	if err != nil {
		return "", err
	}
	defer src.Close()

	dst, err := os.Create(destPath)
	if err != nil {
		return "", err
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return "", err
	}

	return destPath, nil
}

func (m *Model) pasteImageFromClipboard() (string, bool) {
	// Only support Mac for now via osascript
	if runtime.GOOS != "darwin" {
		return "", false
	}

	// Save to current directory's .zotigo folder for shorter paths
	uploadDir := ".zotigo/uploads"
	os.MkdirAll(uploadDir, 0755)

	filename := fmt.Sprintf("paste_%d.png", time.Now().UnixNano())
	relPath := filepath.Join(uploadDir, filename)

	// AppleScript needs absolute path
	absPath, err := filepath.Abs(relPath)
	if err != nil {
		return "", false
	}

	// AppleScript to save clipboard to file
	script := fmt.Sprintf(`try
		set theFile to (open for access POSIX file "%s" with write permission)
		set eof theFile to 0
		write (the clipboard as «class PNGf») to theFile
		close access theFile
		return "OK"
	on error
		try
			close access theFile
		end try
		return "ERR"
	end try`, absPath)

	cmd := exec.Command("osascript", "-e", script)
	out, err := cmd.Output()

	if err == nil && strings.TrimSpace(string(out)) == "OK" {
		info, err := os.Stat(relPath)
		if err == nil && info.Size() > 0 {
			return relPath, true // Return relative path for display
		}
	}
	os.Remove(relPath)

	return "", false
}
