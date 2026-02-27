package tui

import (
	"context"
	"encoding/json"
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
	"github.com/jayyao97/zotigo/cli/commands"
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/session"
)

var (
	userMarkerStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true)
	asstMarkerStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("86")).Bold(true)
	toolMarkerStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("75")).Bold(true)
	resultStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	timingStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	warningStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("202")).Bold(true)
	errorStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	headerStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Bold(true).Padding(0, 1)
	focusedChoice   = lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true)
	blurredChoice   = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	inputStyle      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("62")).Padding(0, 1)
	promptStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true)

	// reBlankRun matches 3+ consecutive newlines (2+ blank lines) for compression.
	reBlankRun = regexp.MustCompile(`\n{3,}`)
)

type Model struct {
	agent           *agent.Agent
	sessionMgr      *session.Manager
	sessionID       string
	cmdRegistry     *commands.Registry
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
	autoApprove     bool
	streamFlushed   int // lines already committed to scrollback during streaming
	turnStartTime   time.Time
	needsAsstMarker bool // next text content block should get a ⏺ prefix
}

type streamReadyMsg <-chan protocol.Event
type errMsg error

func NewModel(ag *agent.Agent, sessMgr *session.Manager, sessID string, cmdRegistry *commands.Registry) Model {
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

	m := Model{
		agent:       ag,
		sessionMgr:  sessMgr,
		sessionID:   sessID,
		cmdRegistry: cmdRegistry,
		ctx:         context.Background(),
		input:       ta,
	}

	// If the agent was saved in a paused state with pending actions,
	// restore the approval UI so the user can approve/deny.
	snap := ag.Snapshot()
	if snap.State == agent.StatePaused && len(snap.PendingActions) > 0 {
		m.approving = true
		m.approvalChoice = 0
		m.pendingToolName = formatPendingActions(snap.PendingActions)
	}

	return m
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

		return m, nil

	case tea.KeyboardEnhancementsMsg:
		if !m.kittyChecked {
			m.kittyChecked = true
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

		if keyStr == "shift+tab" {
			m.autoApprove = !m.autoApprove
			if m.autoApprove {
				m.agent.SetApprovalPolicy(agent.ApprovalPolicyAuto)
			} else {
				m.agent.SetApprovalPolicy(agent.ApprovalPolicyManual)
			}
			// Re-focus textarea to maintain cursor blink cycle
			cmd := m.input.Focus()
			return m, cmd
		}

		if m.approving {
			switch keyStr {
			case "up", "k":
				if m.approvalChoice > 0 {
					m.approvalChoice--
				}
				return m, nil
			case "down", "j":
				if m.approvalChoice < 2 {
					m.approvalChoice++
				}
				return m, nil
			case "enter":
				switch m.approvalChoice {
				case 0: // Accept
					return m.submitApproval(true)
				case 1: // Deny → back to input
					return m.denyAndReturn("")
				case 2: // Feedback textarea → deny with text
					v := m.input.Value()
					if strings.TrimSpace(v) == "" {
						return m, nil // empty text, do nothing
					}
					m.input.Reset()
					return m.denyAndReturn(v)
				}
			}
			// When approvalChoice==2, let other keys fall through to textarea update
			if m.approvalChoice == 2 {
				// fall through to bottom textarea update logic
			} else {
				return m, nil
			}
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

			// Slash command routing
			if commands.IsCommand(v) {
				cmdName, args, _ := commands.Parse(v)

				// 1. Try builtin commands (e.g., /help, /clear, /model)
				if cmd, ok := m.cmdRegistry.Get(cmdName); ok {
					m.input.Reset()
					var output strings.Builder
					env := m.buildCmdEnv(&output)
					err := cmd.Execute(m.ctx, env, args)
					if err != nil {
						return m, tea.Println(errorStyle.Render("✗ ") + "Error: " + err.Error())
					}
					if output.Len() > 0 {
						return m, tea.Println(output.String())
					}
					return m, nil
				}

				// 2. Not a builtin command — send as user message to the model
				// The model has all skill instructions injected and can handle
				// slash-style invocations like "/commit fix bug" naturally.
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
			m.turnStartTime = time.Now()
			m.needsAsstMarker = true
			m.currentAsstMsg = ""

			userMsgStr, _ := renderMessage(msg)
			return m, tea.Batch(tea.Println(userMsgStr), m.startRun(msg))
		}
	case streamReadyMsg:
		m.eventCh = msg
		m.currentAsstMsg = ""
		m.streamFlushed = 0
		m.needsAsstMarker = true
		return m, waitForNextEvent(m.eventCh)

	case protocol.Event:
		switch msg.Type {
		case protocol.EventTypeContentDelta:
			if msg.ContentPartDelta != nil {
				if m.needsAsstMarker {
					m.currentAsstMsg += asstMarkerStyle.Render("⏺ ")
					m.needsAsstMarker = false
				}
				m.currentAsstMsg += msg.ContentPartDelta.Text
				if cmd := m.flushStreamedLines(); cmd != nil {
					return m, tea.Batch(cmd, waitForNextEvent(m.eventCh))
				}
			}
		case protocol.EventTypeToolCallDelta:
			if msg.ToolCallDelta != nil && msg.ToolCallDelta.Name != "" {
				// Flush any pending text before the tool call
				if m.currentAsstMsg != "" {
					if !strings.HasSuffix(m.currentAsstMsg, "\n") {
						m.currentAsstMsg += "\n"
					}
					if cmd := m.flushStreamedLines(); cmd != nil {
						m.currentAsstMsg = fmt.Sprintf("⏺ %s ...", toPascalCase(msg.ToolCallDelta.Name))
						return m, tea.Batch(cmd, waitForNextEvent(m.eventCh))
					}
				}
				m.currentAsstMsg = fmt.Sprintf("⏺ %s ...", toPascalCase(msg.ToolCallDelta.Name))
			}
		case protocol.EventTypeToolCallEnd:
			if msg.ToolCall != nil {
				placeholder := fmt.Sprintf("⏺ %s ...", toPascalCase(msg.ToolCall.Name))
				full := toolMarkerStyle.Render("⏺ ") + formatToolCall(msg.ToolCall) + "\n"
				m.currentAsstMsg = strings.Replace(m.currentAsstMsg, placeholder, full, 1)
				m.needsAsstMarker = true
				// Flush tool call to scrollback so next content starts fresh
				if cmd := m.flushStreamedLines(); cmd != nil {
					return m, tea.Batch(cmd, waitForNextEvent(m.eventCh))
				}
			}
		case protocol.EventTypeToolResultDone:
			if msg.ToolResult != nil {
				rendered := formatToolResult(msg.ToolResult, 10)
				m.currentAsstMsg += rendered + "\n"
				if cmd := m.flushStreamedLines(); cmd != nil {
					return m, tea.Batch(cmd, waitForNextEvent(m.eventCh))
				}
			}
		case protocol.EventTypeFinish:
			m.thinking = false
			snap := m.agent.Snapshot()
			timingSuffix := "\n" + timingStyle.Render("✻ Completed in "+formatDuration(time.Since(m.turnStartTime)))

			if m.streamFlushed > 0 {
				// Lines were incrementally committed — just flush the remaining tail
				var batchCmds []tea.Cmd
				if m.currentAsstMsg != "" {
					batchCmds = append(batchCmds, tea.Println(m.currentAsstMsg))
				}
				m.currentAsstMsg = ""
				m.streamFlushed = 0

				if msg.FinishReason == "need_approval" {
					m.approving = true
					m.approvalChoice = 0
					m.pendingToolName = formatPendingActions(snap.PendingActions)
					m.pendingToolArgs = ""
					m.saveSession()
					if len(batchCmds) > 0 {
						return m, tea.Batch(batchCmds...)
					}
					return m, nil
				}

				batchCmds = append(batchCmds, tea.Println(timingSuffix))
				m.eventCh = nil
				m.saveSession()
				return m, tea.Sequence(batchCmds...)
			}

			// Short reply — no incremental flush happened, use renderMessage
			formattedMsg := ""
			if len(snap.History) > 0 {
				lastMsg := snap.History[len(snap.History)-1]
				if lastMsg.Role == protocol.RoleAssistant {
					if str, ok := renderMessage(lastMsg); ok {
						formattedMsg = str
					}
				}
			}
			if formattedMsg == "" && m.currentAsstMsg != "" {
				formattedMsg = "\n" + m.currentAsstMsg
			}

			if msg.FinishReason == "need_approval" {
				m.approving = true
				m.approvalChoice = 0
				m.pendingToolName = formatPendingActions(snap.PendingActions)
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
				return m, tea.Sequence(tea.Println(formattedMsg), tea.Println(timingSuffix))
			}
			return m, tea.Println(timingSuffix)

		case protocol.EventTypeError:
			m.err = msg.Error
			m.thinking = false
			errStr := "\n" + errorStyle.Render("✗ ") + "Error: " + fmt.Sprintf("%v", msg.Error)
			return m, tea.Println(errStr)
		}
		return m, waitForNextEvent(m.eventCh)

	case errMsg:
		if strings.Contains(msg.Error(), "agent is not paused") {
			return m, nil
		}
		m.err = msg
		m.thinking = false
		errStr := "\n" + errorStyle.Render("✗ ") + "System Error: " + fmt.Sprintf("%v", msg)
		return m, tea.Println(errStr)
	}

	if !m.approving || m.approvalChoice == 2 {
		// Filter IME composition events: key press with no printable text
		// and not a special/functional key. These are intermediate states
		// from CJK input methods that cause cursor flickering.
		if k, ok := msg.(tea.KeyPressMsg); ok {
			if len(k.Text) == 0 && !isSpecialKey(k) {
				return m, nil
			}
		}

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

func (m *Model) buildCmdEnv(output *strings.Builder) *commands.Environment {
	return &commands.Environment{
		Agent:        m.agent,
		SkillManager: m.agent.SkillManager(),
		Output: func(format string, args ...interface{}) {
			fmt.Fprintf(output, format+"\n", args...)
		},
		ClearHistory: func() {
			m.agent.Restore(agent.Snapshot{})
		},
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
	m.turnStartTime = time.Now()

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

func (m Model) denyAndReturn(feedback string) (Model, tea.Cmd) {
	m.approving = false

	reason := "User denied"
	if feedback != "" {
		reason = feedback
	}

	status := "🚫 Denied"
	if feedback != "" {
		status = fmt.Sprintf("🚫 Denied (feedback: %s)", feedback)
	}
	approvalMsg := fmt.Sprintf("\n%s\n%s", m.pendingToolName, status)

	snap := m.agent.Snapshot()
	var outputs []protocol.ToolResult
	for _, act := range snap.PendingActions {
		outputs = append(outputs, protocol.ToolResult{
			ToolCallID: act.ToolCallID,
			Type:       protocol.ToolResultTypeExecutionDenied,
			Reason:     reason,
		})
	}

	if feedback == "" {
		// Simple deny: back to input mode
		m.thinking = false
		cmd := func() tea.Msg {
			ch, err := m.agent.SubmitToolOutputs(m.ctx, outputs)
			if err != nil {
				return errMsg(err)
			}
			// Drain the channel so agent settles, but don't continue the loop
			for range ch {
			}
			return nil
		}
		return m, tea.Batch(tea.Println(approvalMsg), cmd)
	}

	// Deny with feedback: keep thinking, agent continues with user feedback
	m.thinking = true
	m.turnStartTime = time.Now()
	cmd := func() tea.Msg {
		ch, err := m.agent.SubmitToolOutputs(m.ctx, outputs)
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

func (m *Model) flushStreamedLines() tea.Cmd {
	// Flush all complete lines (up to the last \n) to scrollback.
	// Only the incomplete trailing fragment stays in View().
	idx := strings.LastIndex(m.currentAsstMsg, "\n")
	if idx < 0 {
		return nil // no complete line yet
	}
	toCommit := m.currentAsstMsg[:idx]
	m.currentAsstMsg = m.currentAsstMsg[idx+1:]

	prefix := ""
	if m.streamFlushed == 0 {
		prefix = "\n"
	}
	m.streamFlushed++
	return tea.Println(prefix + toCommit)
}

func (m Model) View() tea.View {
	// Wait for WindowSizeMsg to initialize width.
	if m.width == 0 {
		return tea.NewView("")
	}

	var sb strings.Builder

	if m.thinking && m.currentAsstMsg != "" {
		sb.WriteString(m.currentAsstMsg)
		sb.WriteString("\n")
	} else if m.thinking {
		sb.WriteString(asstMarkerStyle.Render("⏺ ") + "Thinking...\n")
	}

	if m.approving {
		sb.WriteString(warningStyle.Render("⚠ ") + "Execute: " + m.pendingToolName + "\n")

		// Accept line
		if m.approvalChoice == 0 {
			sb.WriteString(fmt.Sprintf("  %s %s\n", focusedChoice.Render(">"), focusedChoice.Render("Accept")))
		} else {
			sb.WriteString(fmt.Sprintf("    %s\n", blurredChoice.Render("Accept")))
		}

		// Deny line
		if m.approvalChoice == 1 {
			sb.WriteString(fmt.Sprintf("  %s %s\n", focusedChoice.Render(">"), focusedChoice.Render("Deny")))
		} else {
			sb.WriteString(fmt.Sprintf("    %s\n", blurredChoice.Render("Deny")))
		}

		// Feedback input line
		if m.approvalChoice == 2 {
			sb.WriteString("  " + focusedChoice.Render("> ") + m.input.View())
		} else {
			placeholder := "Send feedback..."
			if v := m.input.Value(); v != "" {
				placeholder = v
			}
			sb.WriteString(fmt.Sprintf("    %s", blurredChoice.Render(placeholder)))
		}
	} else {
		// Only show indicator when auto-approve is on
		if m.autoApprove {
			sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Bold(true).Render(">> Auto-approve"))
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
	switch msg.Role {
	case protocol.RoleUser:
		text := msg.String()
		if text == "" {
			return "", false
		}
		return "\n" + userMarkerStyle.Render("❯ ") + text, true

	case protocol.RoleAssistant:
		var parts []string
		for _, p := range msg.Content {
			switch p.Type {
			case protocol.ContentTypeText, protocol.ContentTypeReasoning:
				if p.Text != "" {
					parts = append(parts, "\n"+asstMarkerStyle.Render("⏺ ")+p.Text)
				}
			case protocol.ContentTypeToolCall:
				if p.ToolCall != nil {
					parts = append(parts, "\n"+toolMarkerStyle.Render("⏺ ")+formatToolCall(p.ToolCall))
				}
			}
		}
		if len(parts) == 0 {
			return "", false
		}
		return strings.Join(parts, ""), true

	case protocol.RoleTool:
		var parts []string
		for _, p := range msg.Content {
			if p.Type == protocol.ContentTypeToolResult && p.ToolResult != nil {
				parts = append(parts, formatToolResult(p.ToolResult, 10))
			}
		}
		if len(parts) == 0 {
			return "", false
		}
		return "\n" + strings.Join(parts, "\n"), true

	default:
		return "", false
	}
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

func isSpecialKey(k tea.KeyPressMsg) bool {
	// Keys that should always be forwarded to textarea even without Text:
	// navigation, deletion, modifiers, function keys, etc.
	s := k.String()
	switch {
	case strings.HasPrefix(s, "ctrl+"),
		strings.HasPrefix(s, "alt+"),
		strings.HasPrefix(s, "shift+"):
		return true
	}
	switch k.Code {
	case tea.KeyUp, tea.KeyDown, tea.KeyLeft, tea.KeyRight,
		tea.KeyHome, tea.KeyEnd, tea.KeyPgUp, tea.KeyPgDown,
		tea.KeyDelete, tea.KeyBackspace, tea.KeyTab,
		tea.KeyEnter, tea.KeyEscape:
		return true
	}
	return false
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

// primaryArgKey maps tool names to the single most informative argument.
var primaryArgKey = map[string]string{
	"shell":           "command",
	"bash":            "command",
	"execute_command": "command",
	"read_file":       "path",
	"write_file":      "path",
	"edit_file":       "path",
	"create_file":     "path",
	"delete_file":     "path",
	"list_files":      "path",
	"search_files":    "pattern",
	"search":          "query",
	"web_search":      "query",
	"grep":            "pattern",
	"find":            "pattern",
}

// toPascalCase converts a snake_case name to PascalCase, e.g. "read_file" → "ReadFile".
func toPascalCase(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

// formatDuration formats a duration for the timing footer.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm %ds", m, s)
}

// formatToolCall returns a compact summary like "Shell(git status)" or "ReadFile(path=src/main.go)".
func formatToolCall(tc *protocol.ToolCall) string {
	name := toPascalCase(tc.Name)

	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil || len(args) == 0 {
		return name + "()"
	}

	// Try the known primary arg first.
	if key, ok := primaryArgKey[tc.Name]; ok {
		if v, found := args[key]; found {
			s := fmt.Sprintf("%v", v)
			if len(s) > 80 {
				s = s[:77] + "..."
			}
			return fmt.Sprintf("%s(%s)", name, s)
		}
	}

	// Fallback: pick the first non-large arg.
	for k, v := range args {
		s := fmt.Sprintf("%v", v)
		if len(s) > 200 {
			continue // skip large values
		}
		if len(s) > 80 {
			s = s[:77] + "..."
		}
		return fmt.Sprintf("%s(%s=%s)", name, k, s)
	}

	return name + "(...)"
}

// formatToolResult renders tool result lines with ⎿ prefix and indentation.
func formatToolResult(tr *protocol.ToolResult, maxLines int) string {
	if tr.IsError || tr.Type == protocol.ToolResultTypeErrorText || tr.Type == protocol.ToolResultTypeErrorJSON {
		errText := tr.Text
		if errText == "" {
			errText = fmt.Sprintf("%v", tr.JSON)
		}
		if len(errText) > 200 {
			errText = errText[:197] + "..."
		}
		return "  " + errorStyle.Render("⎿  Error: "+errText)
	}

	text := tr.Text
	if text == "" && tr.JSON != nil {
		// If JSON is a slice/array, join elements as lines for readability.
		switch arr := tr.JSON.(type) {
		case []any:
			var lines []string
			for _, item := range arr {
				lines = append(lines, fmt.Sprintf("%v", item))
			}
			text = strings.Join(lines, "\n")
		case []string:
			text = strings.Join(arr, "\n")
		default:
			b, _ := json.Marshal(tr.JSON)
			text = string(b)
		}
	}
	if tr.Type == protocol.ToolResultTypeExecutionDenied {
		text = "Denied: " + tr.Reason
	}

	if text == "" {
		return resultStyle.Render("  ⎿  (No output)")
	}

	// Compress runs of 2+ blank lines into a single blank line for display.
	text = reBlankRun.ReplaceAllString(text, "\n\n")
	text = strings.TrimRight(text, " \t\n\r")

	// Hard cap on total characters to handle single-line mega outputs (e.g. JSON blobs).
	const maxDisplayChars = 300
	charTruncated := false
	if len(text) > maxDisplayChars {
		text = text[:maxDisplayChars]
		charTruncated = true
	}

	lines := strings.Split(text, "\n")
	totalLines := len(lines)

	if maxLines > 0 && totalLines > maxLines {
		lines = lines[:maxLines]
	}

	var sb strings.Builder
	for i, line := range lines {
		if i == 0 {
			sb.WriteString("  ⎿  " + line)
		} else {
			sb.WriteString("\n     " + line)
		}
	}

	if maxLines > 0 && totalLines > maxLines {
		sb.WriteString(fmt.Sprintf("\n     ... (%d lines total)", totalLines))
	} else if charTruncated {
		sb.WriteString("\n     ... (output truncated)")
	}

	return resultStyle.Render(sb.String())
}

// formatPendingActions builds the approval header string from pending tool actions.
func formatPendingActions(actions []*agent.PendingAction) string {
	var parts []string
	for _, act := range actions {
		tc := act.ToolCall
		if tc == nil {
			tc = &protocol.ToolCall{Name: act.Name, Arguments: act.Arguments}
		}
		parts = append(parts, formatToolCall(tc))
	}
	return strings.Join(parts, "\n")
}
