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
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/jayyao97/zotigo/cli/commands"
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/transport"
	"github.com/jayyao97/zotigo/internal/sessionadapter"
)

var (
	userMarkerStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true)
	asstMarkerStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("86")).Bold(true)
	toolMarkerStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("75")).Bold(true)
	reasoningStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("243")).Italic(true)
	reasoningLabelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("243")).Italic(true).Bold(true)
	resultStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	timingStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	warningStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("202")).Bold(true)
	errorStyle          = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	headerStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Bold(true).Padding(0, 1)
	focusedChoice       = lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true)
	blurredChoice       = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	inputStyle          = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("62")).Padding(0, 1)
	promptStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true)
)

type Model struct {
	agent              *agent.Agent
	sessionMgr         *session.Manager
	sessionID          string
	cmdRegistry        *commands.Registry
	ctx                context.Context
	input              textarea.Model
	transcript         []string
	viewport           viewport.Model
	viewportContent    string
	viewportEnabled    bool
	viewportAutoScroll bool
	currentAsstMsg     string
	thinking           bool
	approving          bool
	approvalChoice     int
	approvalItemChoice int
	pendingApprovals   []*agent.PendingAction
	approvalDecisions  map[string]bool
	pendingToolName    string
	pendingToolArgs    string
	err                error
	eventCh            <-chan protocol.Event
	width              int
	height             int
	initialPrinted     bool
	kittyChecked       bool
	autoApprove        bool
	streamFlushed      int // lines already committed to scrollback during streaming
	turnStartTime      time.Time
	displayTurnID      string
	displayAsstContent []session.DisplayContentPart
	needsAsstMarker    bool // next text content block should get a ⏺ prefix
	streamingReasoning bool // currently streaming reasoning content
}

type streamReadyMsg <-chan protocol.Event
type errMsg error
type denialSettledMsg struct{}
type transcriptMsg string

func NewModel(ag *agent.Agent, sessMgr *session.Manager, sessID string, cmdRegistry *commands.Registry) *Model {
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

	vp := viewport.New()
	vp.SoftWrap = false
	vp.MouseWheelEnabled = true
	viewportEnabled := shouldUseViewportRenderer()

	m := Model{
		agent:              ag,
		sessionMgr:         sessMgr,
		sessionID:          sessID,
		cmdRegistry:        cmdRegistry,
		ctx:                context.Background(),
		input:              ta,
		viewport:           vp,
		viewportEnabled:    viewportEnabled,
		viewportAutoScroll: true,
		// Keep the local auto-approve toggle in sync with the agent's
		// actual policy — new sessions default to Auto, and resuming
		// a session inherits whatever policy was active. Without this
		// sync, shift-tab would start at "off" while the agent was
		// actually in Auto.
		autoApprove: ag.Describe().ApprovalPolicy == agent.ApprovalPolicyAuto,
	}

	// If the agent was saved in a paused state with pending actions,
	// restore the approval UI so the user can approve/deny.
	snap := ag.Snapshot()
	if snap.State == agent.StatePaused && len(snap.PendingActions) > 0 {
		m.approving = true
		m.approvalChoice = 0
		m.setPendingApprovals(snap.PendingActions)
		m.restoreOpenDisplayTurnID()
	}

	return &m
}

func (m *Model) Init() tea.Cmd {
	return textarea.Blink
}

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

func (m *Model) printInitialHistory(isRepaint bool) tea.Cmd {
	items, truncated := m.initialDisplayItems()

	headerText := "Welcome to Zotigo CLI"
	if len(items) > 0 {
		headerText = "Welcome back to Zotigo CLI"
	}

	header := headerStyle.Render("── " + headerText + " ──")

	var cmds []tea.Cmd
	cmds = append(cmds, m.commitLine(header))
	cmds = append(cmds, m.commitLine(renderAgentBanner(m.agent.Describe())))
	if truncated {
		cmds = append(cmds, m.commitLine(headerStyle.Render("── (...earlier messages truncated...) ──")))
	}
	cmds = append(cmds, m.commitLine(""))

	for _, item := range items {
		if str, ok := renderDisplayItem(item); ok {
			cmds = append(cmds, m.commitLine(str))
		}
	}
	cmds = append(cmds, m.commitLine(""))
	return tea.Sequence(cmds...)
}

func (m *Model) initialDisplayItems() ([]session.DisplayItem, bool) {
	if m.sessionMgr == nil || m.sessionID == "" {
		return nil, false
	}
	items, ok, err := m.sessionMgr.ListDisplayItems(m.sessionID)
	if err != nil || !ok {
		return nil, false
	}

	const maxHistory = 100
	truncated := false
	if len(items) > maxHistory {
		items = items[len(items)-maxHistory:]
		truncated = true
	}
	return items, truncated
}

func (m *Model) restoreOpenDisplayTurnID() {
	if m.sessionMgr == nil || m.sessionID == "" {
		return
	}
	items, ok, err := m.sessionMgr.ListDisplayItems(m.sessionID)
	if err != nil || !ok {
		return
	}
	m.displayTurnID = lastOpenDisplayTurnID(items)
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case transcriptMsg:
		m.appendTranscript(string(msg))
		return m, nil

	case tea.MouseWheelMsg:
		if !m.viewportEnabled {
			break
		}
		m.updateViewportContent()
		beforeY := m.viewport.YOffset()
		beforeBottom := m.viewport.AtBottom()
		m.viewport, cmd = m.viewport.Update(msg)
		m.viewportAutoScroll = m.viewport.AtBottom()
		m.logViewportScroll("viewport-update", msg.Button, beforeY, beforeBottom)
		return m, cmd

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
		m.viewport.SetWidth(m.width)

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
		m.input.InsertString(msg.Content)
		m.input.SetHeight(m.inputLineCount())
		return m, nil

	case tea.KeyPressMsg:
		keyStr := msg.String()

		if keyStr == "=" || keyStr == ";" || keyStr == "u" || keyStr == "[" {
			if !m.input.Focused() || m.thinking {
				return m, nil
			}
		}

		if keyStr == "ctrl+c" || keyStr == "esc" {
			m.saveSession()
			if summary := renderUsageSummary(m.agent); summary != "" {
				return m, tea.Sequence(m.commitLine("\n"+summary), tea.Quit)
			}
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
					if len(m.pendingApprovals) > 1 {
						return m.acceptCurrentApproval()
					}
					return m.submitApproval()
				case 1: // Deny → back to input
					if len(m.pendingApprovals) > 1 {
						return m.denyCurrentApproval("")
					}
					return m.denyAndReturn("")
				case 2: // Feedback textarea → deny with text
					v := m.input.Value()
					if strings.TrimSpace(v) == "" {
						return m, nil // empty text, do nothing
					}
					m.input.Reset()
					if len(m.pendingApprovals) > 1 {
						return m.denyCurrentApproval(v)
					}
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
						return m, m.commitLine(errorStyle.Render("✗ ") + "Error: " + err.Error())
					}
					if output.Len() > 0 {
						return m, m.commitLine(output.String())
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
							switch ext {
							case ".jpg", ".jpeg":
								mime = "image/jpeg"
							case ".webp":
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
			m.displayTurnID = fmt.Sprintf("turn_%d", m.turnStartTime.UnixNano())
			m.needsAsstMarker = true
			m.currentAsstMsg = ""
			m.displayAsstContent = nil
			m.appendDisplayItem(session.DisplayItem{
				Type: session.DisplayItemTurnStarted,
				Turn: &session.DisplayTurn{ID: m.displayTurnID},
			})
			m.appendDisplayItem(displayMessageItem(session.DisplayItemUserMessage, protocol.RoleUser, msg))

			userMsgStr, _ := renderMessage(msg)
			return m, tea.Batch(m.commitLine(userMsgStr), m.startRun(msg))
		}
	case streamReadyMsg:
		m.eventCh = msg
		m.currentAsstMsg = ""
		m.displayAsstContent = nil
		m.streamFlushed = 0
		m.needsAsstMarker = true
		return m, waitForNextEvent(m.eventCh)

	case protocol.Event:
		switch msg.Type {
		case protocol.EventTypeContentDelta:
			if msg.ContentPartDelta != nil {
				isReasoning := msg.ContentPartDelta.Type == protocol.ContentTypeReasoning
				var pendingFlush tea.Cmd

				// Transition: start reasoning block
				if isReasoning && !m.streamingReasoning {
					m.streamingReasoning = true
					m.currentAsstMsg += reasoningLabelStyle.Render("⏺ Thinking...") + "\n"
					m.needsAsstMarker = true // reset for when text starts
				}
				// Transition: reasoning ended, text started
				if !isReasoning && m.streamingReasoning {
					m.streamingReasoning = false
					if !strings.HasSuffix(m.currentAsstMsg, "\n") {
						m.currentAsstMsg += "\n"
					}
					pendingFlush = m.flushStreamedLines()
				}

				if !isReasoning && m.needsAsstMarker {
					m.currentAsstMsg += asstMarkerStyle.Render("⏺ ")
					m.needsAsstMarker = false
				}
				if isReasoning {
					m.currentAsstMsg += reasoningStyle.Render(msg.ContentPartDelta.Text)
					m.appendAssistantDisplayPart(string(protocol.ContentTypeReasoning), msg.ContentPartDelta.Text)
				} else {
					m.currentAsstMsg += msg.ContentPartDelta.Text
					m.appendAssistantDisplayPart(string(protocol.ContentTypeText), msg.ContentPartDelta.Text)
				}
				if cmd := m.flushStreamedLines(); cmd != nil {
					if pendingFlush != nil {
						return m, tea.Batch(pendingFlush, cmd, waitForNextEvent(m.eventCh))
					}
					return m, tea.Batch(cmd, waitForNextEvent(m.eventCh))
				}
				if pendingFlush != nil {
					return m, tea.Batch(pendingFlush, waitForNextEvent(m.eventCh))
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
				m.appendToolCallDisplayPart(msg.ToolCall)
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
				m.appendToolResultDisplayPart(msg.ToolResult)
				if cmd := m.flushStreamedLines(); cmd != nil {
					return m, tea.Batch(cmd, waitForNextEvent(m.eventCh))
				}
			}
		case protocol.EventTypeToolProgress:
			if msg.ToolResult != nil {
				rendered := formatToolResult(msg.ToolResult, 10)
				m.currentAsstMsg += rendered + "\n"
				m.appendToolResultDisplayPart(msg.ToolResult)
				if cmd := m.flushStreamedLines(); cmd != nil {
					return m, tea.Batch(cmd, waitForNextEvent(m.eventCh))
				}
			}
		case protocol.EventTypeFinish:
			m.thinking = false
			m.streamingReasoning = false
			snap := m.agent.Snapshot()
			timingSuffix := "\n" + timingStyle.Render("✻ Completed in "+formatDuration(time.Since(m.turnStartTime)))

			if m.streamFlushed > 0 {
				// Lines were incrementally committed — just flush the remaining tail
				var batchCmds []tea.Cmd
				if m.currentAsstMsg != "" {
					batchCmds = append(batchCmds, m.commitLine(m.currentAsstMsg))
				}
				m.currentAsstMsg = ""
				m.streamFlushed = 0

				if msg.FinishReason == "need_approval" {
					m.approving = true
					m.approvalChoice = 0
					m.setPendingApprovals(snap.PendingActions)
					m.pendingToolArgs = ""
					m.appendTurnPaused()
					m.saveSession()
					if len(batchCmds) > 0 {
						return m, tea.Batch(batchCmds...)
					}
					return m, nil
				}

				batchCmds = append(batchCmds, m.commitLine(timingSuffix))
				m.eventCh = nil
				m.appendTurnCompleted(msg.FinishReason)
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
				m.setPendingApprovals(snap.PendingActions)
				m.pendingToolArgs = ""

				m.currentAsstMsg = ""
				m.appendTurnPaused()
				m.saveSession()
				if formattedMsg != "" {
					return m, m.commitLine(formattedMsg)
				}
				return m, nil
			}

			m.currentAsstMsg = ""
			m.eventCh = nil
			m.appendTurnCompleted(msg.FinishReason)
			m.saveSession()
			if formattedMsg != "" {
				return m, tea.Sequence(m.commitLine(formattedMsg), m.commitLine(timingSuffix))
			}
			return m, m.commitLine(timingSuffix)

		case protocol.EventTypeError:
			m.err = msg.Error
			m.thinking = false
			m.appendTurnFailed(msg.Error)
			m.saveSession()
			errStr := "\n" + errorStyle.Render("✗ ") + "Error: " + fmt.Sprintf("%v", msg.Error)
			return m, m.commitLine(errStr)
		}
		return m, waitForNextEvent(m.eventCh)

	case denialSettledMsg:
		m.saveSession()
		return m, nil

	case errMsg:
		if strings.Contains(msg.Error(), "agent is not paused") {
			return m, nil
		}
		m.err = msg
		m.thinking = false
		if m.eventCh != nil || m.approving || m.hasOpenDisplayTurn() {
			m.appendTurnFailed(msg)
			m.saveSession()
		}
		errStr := "\n" + errorStyle.Render("✗ ") + "System Error: " + fmt.Sprintf("%v", msg)
		return m, m.commitLine(errStr)
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

func (m *Model) startRun(msg protocol.Message) tea.Cmd {
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

func (m *Model) submitApproval() (*Model, tea.Cmd) {
	m.approving = false
	approvalMsg := fmt.Sprintf("\n%s\n%s", m.pendingToolName, "✅ Approved")
	m.thinking = true
	m.turnStartTime = time.Now()

	cmd := func() tea.Msg {
		ch, err := m.agent.ApproveAndExecutePendingActions(m.ctx)
		if err != nil {
			return errMsg(err)
		}
		return streamReadyMsg(ch)
	}

	return m, tea.Batch(m.commitLine(approvalMsg), cmd)
}

func (m *Model) setPendingApprovals(actions []*agent.PendingAction) {
	m.pendingApprovals = append([]*agent.PendingAction(nil), actions...)
	m.approvalDecisions = make(map[string]bool, len(actions))
	m.approvalItemChoice = 0
	m.pendingToolName = formatPendingActions(actions)
}

func (m *Model) acceptCurrentApproval() (*Model, tea.Cmd) {
	if m.approvalItemChoice < 0 || m.approvalItemChoice >= len(m.pendingApprovals) {
		return m, nil
	}
	m.approvalDecisions[m.pendingApprovals[m.approvalItemChoice].ToolCallID] = true
	if m.approvalItemChoice < len(m.pendingApprovals)-1 {
		m.approvalItemChoice++
		m.approvalChoice = 0
		return m, nil
	}
	return m.submitApprovalBatch("")
}

func (m *Model) denyCurrentApproval(reason string) (*Model, tea.Cmd) {
	if m.approvalItemChoice < 0 || m.approvalItemChoice >= len(m.pendingApprovals) {
		return m, nil
	}
	m.approvalDecisions[m.pendingApprovals[m.approvalItemChoice].ToolCallID] = false
	return m.submitApprovalBatch(reason)
}

func (m *Model) submitApprovalBatch(reason string) (*Model, tea.Cmd) {
	m.approving = false
	if strings.TrimSpace(reason) == "" {
		reason = "User denied in TUI"
	}

	results := make([]transport.ApprovalResult, 0, len(m.pendingApprovals))
	denied := 0
	approved := 0
	for _, action := range m.pendingApprovals {
		approvedDecision, decided := m.approvalDecisions[action.ToolCallID]
		if !decided {
			continue
		}
		result := transport.ApprovalResult{ToolCallID: action.ToolCallID, Approved: approvedDecision}
		if !approvedDecision {
			result.Reason = reason
			denied++
		} else {
			approved++
		}
		results = append(results, result)
	}

	status := fmt.Sprintf("✅ Approved %d", approved)
	if denied > 0 {
		status = fmt.Sprintf("🚫 Denied %d/%d · skipped all pending calls", denied, len(m.pendingApprovals))
	}
	approvalMsg := fmt.Sprintf("\n%s\n%s", m.pendingToolName, status)
	m.thinking = true
	m.turnStartTime = time.Now()

	cmd := func() tea.Msg {
		ch, err := m.agent.ResolvePendingActions(m.ctx, deniedToolResultsFromTUI(results, m.pendingApprovals))
		if err != nil {
			return errMsg(err)
		}
		return streamReadyMsg(ch)
	}

	return m, tea.Batch(m.commitLine(approvalMsg), cmd)
}

func deniedToolResultsFromTUI(results []transport.ApprovalResult, pending []*agent.PendingAction) map[string]protocol.ToolResult {
	byID := make(map[string]*agent.PendingAction, len(pending))
	for _, action := range pending {
		byID[action.ToolCallID] = action
	}
	denied := make(map[string]protocol.ToolResult)
	for _, result := range results {
		if result.Approved {
			continue
		}
		action, ok := byID[result.ToolCallID]
		if !ok {
			continue
		}
		reason := result.Reason
		if reason == "" {
			reason = "User denied in TUI"
		}
		denied[result.ToolCallID] = protocol.ToolResult{
			ToolCallID: result.ToolCallID,
			ToolName:   action.Name,
			Type:       protocol.ToolResultTypeExecutionDenied,
			Reason:     reason,
			IsError:    true,
		}
	}
	return denied
}

func (m *Model) denyAndReturn(feedback string) (*Model, tea.Cmd) {
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
			ToolName:   act.Name,
			Type:       protocol.ToolResultTypeExecutionDenied,
			Reason:     reason,
			IsError:    true,
		})
	}

	if feedback == "" {
		// Simple deny: back to input mode
		m.thinking = false
		m.appendDeniedToolResults(outputs)
		m.appendTurnInterrupted(reason)
		cmd := func() tea.Msg {
			ch, err := m.agent.SubmitToolOutputs(m.ctx, outputs)
			if err != nil {
				return errMsg(err)
			}
			// Drain the channel so agent settles, but don't continue the loop
			for range ch {
			}
			return denialSettledMsg{}
		}
		return m, tea.Batch(m.commitLine(approvalMsg), cmd)
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
	return m, tea.Batch(m.commitLine(approvalMsg), cmd)
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
	return m.commitLine(prefix + toCommit)
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
			sb.WriteString(fmt.Sprintf("  %s %s\n", focusedChoice.Render(">"), focusedChoice.Render("Accept")))
		} else {
			sb.WriteString(fmt.Sprintf("    %s\n", blurredChoice.Render("Accept")))
		}

		// Deny line
		denyLabel := denyLabelForApprovalCount(len(m.pendingApprovals))
		if m.approvalChoice == 1 {
			sb.WriteString(fmt.Sprintf("  %s %s\n", focusedChoice.Render(">"), focusedChoice.Render(denyLabel)))
		} else {
			sb.WriteString(fmt.Sprintf("    %s\n", blurredChoice.Render(denyLabel)))
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
			sb.WriteString(fmt.Sprintf("  %s %s\n", focusedChoice.Render(">"), focusedChoice.Render("Accept")))
		} else {
			sb.WriteString(fmt.Sprintf("    %s\n", blurredChoice.Render("Accept")))
		}

		// Deny line
		denyLabel := denyLabelForApprovalCount(len(m.pendingApprovals))
		if m.approvalChoice == 1 {
			sb.WriteString(fmt.Sprintf("  %s %s\n", focusedChoice.Render(">"), focusedChoice.Render(denyLabel)))
		} else {
			sb.WriteString(fmt.Sprintf("    %s\n", blurredChoice.Render(denyLabel)))
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

	sb.WriteString("\n")

	return sb.String()
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
	defer file.Close()

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

func (m *Model) saveSession() {
	if m.sessionMgr == nil || m.sessionID == "" {
		return
	}

	snap := m.agent.Snapshot()

	if m.currentAsstMsg != "" {
		partialMsg := protocol.NewAssistantMessage(m.currentAsstMsg)
		snap.History = append(snap.History, partialMsg)
	}

	sess, err := m.sessionMgr.Load(m.sessionID)
	if err == nil {
		if contextCompacted(sess, snap) {
			_, _ = m.sessionMgr.AppendDisplayItem(m.sessionID, session.DisplayItem{Type: session.DisplayItemContextCompacted})
		}
		sessionadapter.ApplySnapshot(sess, snap, sessionadapter.LastUserPrompt(snap.History))
		_ = m.sessionMgr.Save(sess)
	}
}

func (m *Model) inputLineCount() int {
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
	defer func() { _ = src.Close() }()

	dst, err := os.Create(destPath)
	if err != nil {
		return "", err
	}
	defer func() { _ = dst.Close() }()

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
	_ = os.MkdirAll(uploadDir, 0755)

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
	_ = os.Remove(relPath)

	return "", false
}
