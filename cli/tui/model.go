package tui

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/google/uuid"
	"github.com/jayyao97/zotigo/cli/commands"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigodclient"
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

// SessionService is the subset of the zotigod client the TUI drives. The
// session service owns execution, persistence, approvals and recovery; the
// TUI only submits input and renders the durable item stream.
type SessionService interface {
	CreateSession(ctx context.Context, req zotigodclient.CreateSessionRequest) (zotigodclient.Session, error)
	GetSession(ctx context.Context, id string) (zotigodclient.Session, error)
	SubmitMessage(ctx context.Context, id string, req zotigodclient.MessageRequest) (zotigodclient.Command, error)
	Items(ctx context.Context, id string, after uint64, before uint64, limit int) (zotigodclient.ItemsPage, error)
	Pause(ctx context.Context, id string, turnID string) (zotigodclient.Command, error)
	DecideApproval(ctx context.Context, id string, approvalID string, decisions []zotigodclient.ApprovalDecision, stopTurn bool) error
	SetApprovalPolicy(ctx context.Context, id string, policy string) error
	SetProfile(ctx context.Context, id string, profile string) error
	Profiles(ctx context.Context, workingDirectory string) (zotigodclient.Profiles, error)
	Skills(ctx context.Context, sessionID string, forceReload bool) ([]zotigodclient.Skill, error)
	Subscribe(ctx context.Context, id string, after uint64, opts zotigodclient.StreamOptions) <-chan zotigodclient.Event
}

// ModelOptions configures a TUI bound to one service session.
type ModelOptions struct {
	Service  SessionService
	Session  zotigodclient.Session
	Registry *commands.Registry
	// History is the recent durable display log rendered at startup.
	History          []session.DisplayItem
	HistoryTruncated bool
	// Cursor is the last durable sequence already reflected by History;
	// the live stream starts after it.
	Cursor uint64
	// Location describes session ownership, for example
	// "Workspace backend-fix (code/backend)" or "Directory /tmp/x".
	Location string
	// Embedded reports that the session service lives in this process.
	Embedded bool
}

type Model struct {
	service     SessionService
	session     zotigodclient.Session
	location    string
	embedded    bool
	cmdRegistry *commands.Registry
	ctx         context.Context
	cancel      context.CancelFunc

	history          []session.DisplayItem
	historyTruncated bool
	cursor           uint64
	events           <-chan zotigodclient.Event
	reconnecting     bool
	streamClosed     bool

	input              textarea.Model
	transcript         []string
	viewport           viewport.Model
	viewportContent    string
	viewportEnabled    bool
	viewportAutoScroll bool
	width              int
	height             int
	initialPrinted     bool
	kittyChecked       bool

	// Streaming state for the current assistant block.
	currentAsstMsg     string
	streamItemID       string
	streamFlushed      int
	needsAsstMarker    bool
	streamingReasoning bool
	streamed           map[string]map[string]string
	// finalized holds durable assistant items already rendered. Deltas and
	// durable items travel separately, so a delta may arrive late.
	finalized map[string]bool

	// Turn state mirrored from durable lifecycle items.
	thinking      bool
	openTurnID    string
	turnStartTime time.Time
	stopping      bool
	ownInputs     map[string]bool
	// pendingInputs are this TUI's inputs whose turn it has not seen start
	// or join yet; submissions records how each submission ended. Both let
	// a stop target a turn the event stream has not reported yet.
	pendingInputs map[string]bool
	submissions   *zotigodclient.Submissions
	steeringTurns map[string]string // pending steering input -> joined turn

	// Approval state for the pending approval_request item.
	approving          bool
	approvalChoice     int
	approvalItemChoice int
	approval           *session.DisplayApproval
	approvalDecisions  map[string]bool
	approvalSubmitted  bool
	pendingToolName    string

	// exitPrompt asks whether a running turn should stop or keep running
	// in the shared daemon when the user quits.
	exitPrompt bool

	approvalPolicy string
	sessionUsage   protocol.Usage
	lastTurnUsage  *protocol.Usage
	turns          int
	contextUsage   *zotigodclient.ContextUsage
	// pendingCmds collects commands scheduled by slash-command hooks, which
	// run synchronously inside Update.
	pendingCmds []tea.Cmd
}

// serviceEventMsg carries a stream event tagged with its session, so events
// still in flight from a replaced session are ignored.
type serviceEventMsg struct {
	sessionID string
	event     zotigodclient.Event
}

type submitResultMsg struct {
	inputID string
	command zotigodclient.Command
	err     error
}

type sessionRefreshedMsg zotigodclient.Session

type serviceNoticeMsg string

func NewModel(opts ModelOptions) *Model {
	ta := textarea.New()
	ta.Placeholder = "Ask Zotigo..."
	ta.Focus()
	ta.CharLimit = 0
	ta.MaxHeight = 0
	ta.SetHeight(1)
	ta.ShowLineNumbers = false

	// Do not use textarea's built-in prompt (it appears on every wrapped visual line).
	ta.Prompt = ""

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

	ctx, cancel := context.WithCancel(context.Background())
	m := &Model{
		service:            opts.Service,
		session:            opts.Session,
		location:           opts.Location,
		embedded:           opts.Embedded,
		cmdRegistry:        opts.Registry,
		ctx:                ctx,
		cancel:             cancel,
		history:            opts.History,
		historyTruncated:   opts.HistoryTruncated,
		cursor:             opts.Cursor,
		input:              ta,
		viewport:           vp,
		viewportEnabled:    shouldUseViewportRenderer(),
		viewportAutoScroll: true,
		streamed:           make(map[string]map[string]string),
		ownInputs:          make(map[string]bool),
		pendingInputs:      make(map[string]bool),
		finalized:          make(map[string]bool),
		submissions:        zotigodclient.NewSubmissions(),
		steeringTurns:      make(map[string]string),
		approvalPolicy:     opts.Session.ApprovalPolicy,
		contextUsage:       opts.Session.ContextUsage,
		needsAsstMarker:    true,
	}
	m.restoreFromHistory()
	return m
}

// restoreFromHistory rebuilds turn, approval and usage state from the
// durable log so a resumed TUI shows what the service is doing now.
func (m *Model) restoreFromHistory() {
	m.openTurnID = lastOpenDisplayTurnID(m.history)
	m.thinking = m.openTurnID != ""
	if m.thinking {
		m.turnStartTime = time.Now()
	}
	for _, item := range m.history {
		if item.Type == session.DisplayItemTurnCompleted && item.Turn != nil && item.Turn.Usage != nil {
			m.addTurnUsage(*item.Turn.Usage)
		}
		if item.Type == session.DisplayItemAssistantMessage {
			// Already printed from history; ignore deltas still in flight.
			m.finalized[item.ID] = true
		}
	}
	// A pending approval is only actionable while a live worker holds it.
	// An offline session's replacement worker denies it on restart.
	if pending := pendingApproval(m.history, m.openTurnID); pending != nil && m.session.Live {
		m.showApproval(pending)
	}
}

func (m *Model) Init() tea.Cmd {
	m.events = m.service.Subscribe(m.ctx, m.session.ID, m.cursor, zotigodclient.StreamOptions{})
	return tea.Batch(textarea.Blink, waitForServiceEvent(m.events, m.session.ID))
}

// SessionID reports the bound service session.
func (m *Model) SessionID() string { return m.session.ID }

// OpenTurnID reports the turn the service is still running, if any.
func (m *Model) OpenTurnID() string { return m.openTurnID }

// Close stops the event stream.
func (m *Model) Close() { m.cancel() }

func (m *Model) printInitialHistory() tea.Cmd {
	headerText := "Welcome to Zotigo CLI"
	if len(m.history) > 0 {
		headerText = "Welcome back to Zotigo CLI"
	}
	header := headerStyle.Render("── " + headerText + " ──")

	var cmds []tea.Cmd
	cmds = append(cmds, m.commitLine(header))
	cmds = append(cmds, m.commitLine(renderSessionBanner(m.session, m.location, m.embedded)))
	if m.historyTruncated {
		cmds = append(cmds, m.commitLine(headerStyle.Render("── (...earlier messages truncated...) ──")))
	}
	cmds = append(cmds, m.commitLine(""))
	for _, item := range m.history {
		if item.Subagent != nil {
			continue
		}
		if str, ok := renderDisplayItem(item); ok {
			cmds = append(cmds, m.commitLine(str))
		}
	}
	if m.thinking && !m.approving {
		cmds = append(cmds, m.commitLine(timingStyle.Render("… a turn is still running in the session service")))
	}
	cmds = append(cmds, m.commitLine(""))
	return tea.Sequence(cmds...)
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case transcriptMsg:
		m.appendTranscript(string(msg))
		return m, nil

	case serviceEventMsg:
		if msg.sessionID != m.session.ID {
			return m, nil
		}
		return m, tea.Batch(m.handleServiceEvent(msg.event), m.nextServiceEvent())

	case submitResultMsg:
		return m, m.handleSubmitResult(msg)

	case sessionRefreshedMsg:
		m.session = zotigodclient.Session(msg)
		m.contextUsage = m.session.ContextUsage
		m.approvalPolicy = m.session.ApprovalPolicy
		return m, nil

	case serviceNoticeMsg:
		return m, m.commitLine(string(msg))

	case approvalFailedMsg:
		m.approvalSubmitted = false
		return m, m.commitLine(string(msg))

	case exitStoppedMsg:
		log.Printf("cli_stop_exit session=%s result=%d err=%v", m.session.ID, msg.result, msg.err)
		if msg.err != nil {
			// Do not exit claiming a stop that did not happen.
			return m, m.commitLine(errorStyle.Render("✗ ") + "Could not stop the turn; still attached: " + msg.err.Error())
		}
		m.stopping = true
		return m, tea.Sequence(m.commitLine(timingStyle.Render(stopResultLine(msg.result))), m.quit())

	case stopFinishedMsg:
		log.Printf("cli_stop session=%s result=%d err=%v", m.session.ID, msg.result, msg.err)
		if msg.err != nil {
			m.stopping = false
			return m, m.commitLine(errorStyle.Render("✗ ") + "Stop failed: " + msg.err.Error())
		}
		for _, id := range msg.inputs {
			delete(m.pendingInputs, id)
			delete(m.steeringTurns, id)
		}
		if msg.result != zotigodclient.StopRequested {
			m.stopping = false
			return m, m.commitLine(timingStyle.Render(stopResultLine(msg.result)))
		}
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
			return m, m.printInitialHistory()
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
			if !m.input.Focused() {
				return m, nil
			}
		}

		if m.exitPrompt {
			return m.handleExitPromptKey(keyStr)
		}
		if keyStr == "ctrl+c" {
			if m.mayBeRunning() && !m.embedded {
				// With the shared daemon the turn can outlive the TUI; ask
				// instead of choosing silently.
				m.exitPrompt = true
				return m, nil
			}
			return m, m.quit()
		}
		if keyStr == "esc" {
			// Esc interrupts a running turn, including one an unconfirmed
			// input may have started; with nothing running it exits.
			if m.mayBeRunning() && !m.approving {
				return m, m.requestPause()
			}
			if !m.approving {
				return m, m.quit()
			}
		}

		if keyStr == "shift+tab" {
			return m, tea.Batch(m.cycleApprovalPolicy(), m.input.Focus())
		}

		if m.approving {
			switch keyStr {
			case "esc":
				return m.denyApproval("")
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
				case 0:
					return m.acceptApproval()
				case 1:
					return m.denyApproval("")
				case 2:
					v := m.input.Value()
					if strings.TrimSpace(v) == "" {
						return m, nil
					}
					m.input.Reset()
					return m.denyApproval(v)
				}
			}
			if m.approvalChoice != 2 {
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

		if keyStr == "enter" && !m.approving {
			v := m.input.Value()
			if strings.TrimSpace(v) == "" {
				return m, nil
			}
			if commands.IsCommand(v) {
				cmdName, args, _ := commands.Parse(v)
				if command, ok := m.cmdRegistry.Get(cmdName); ok {
					m.input.Reset()
					var output strings.Builder
					env := m.buildCmdEnv(&output)
					err := command.Execute(m.ctx, env, args)
					scheduled := m.pendingCmds
					m.pendingCmds = nil
					if err != nil {
						scheduled = append(scheduled, m.commitLine(errorStyle.Render("✗ ")+"Error: "+err.Error()))
					} else if output.Len() > 0 {
						scheduled = append([]tea.Cmd{m.commitLine(output.String())}, scheduled...)
					}
					return m, tea.Sequence(scheduled...)
				}
				// Other slash input (skills, /compact) goes to the session
				// service, which owns its interpretation.
			}
			m.input.Reset()
			return m, m.submitInput(v)
		}
	}

	if !m.approving || m.approvalChoice == 2 {
		// Filter IME composition events: key press with no printable text
		// and not a special/functional key.
		if k, ok := msg.(tea.KeyPressMsg); ok {
			if len(k.Text) == 0 && !isSpecialKey(k) {
				return m, nil
			}
		}

		if k, ok := msg.(tea.KeyPressMsg); ok && len(k.Text) > 0 {
			w := m.input.Width()
			if w > 0 {
				val := m.input.Value()
				lastLineLen := 0
				if idx := strings.LastIndex(val, "\n"); idx >= 0 {
					lastLineLen = lipgloss.Width(val[idx+1:])
				} else {
					lastLineLen = lipgloss.Width(val)
				}
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

func (m *Model) quit() tea.Cmd {
	var lines []tea.Cmd
	if summary := renderUsageSummary(m.sessionUsage, m.turns, m.session.Profile); summary != "" {
		lines = append(lines, m.commitLine("\n"+summary))
	}
	if m.thinking && !m.stopping {
		if m.embedded {
			lines = append(lines, m.commitLine(warningStyle.Render("⚠ ")+"Stopping the running turn (--no-daemon sessions end with this process)."))
		} else {
			lines = append(lines, m.commitLine(timingStyle.Render(fmt.Sprintf("The running turn continues in zotigod. Reattach with: zotigo --resume (session %s)", m.session.ID))))
		}
	}
	m.cancel()
	lines = append(lines, tea.Quit)
	return tea.Sequence(lines...)
}

// exitStoppedMsg ends the TUI once the running turn is known to stop.
type exitStoppedMsg struct {
	result zotigodclient.StopResult
	err    error
}

// stopFinishedMsg reports an Esc stop that did not exit the TUI.
type stopFinishedMsg struct {
	// inputs were the stop's input targets; a confirmed stop has placed
	// all of them, so they no longer keep the TUI busy.
	inputs []string
	result zotigodclient.StopResult
	err    error
}

const stopTimeout = 15 * time.Second

func (m *Model) handleExitPromptKey(keyStr string) (tea.Model, tea.Cmd) {
	switch keyStr {
	case "s", "S":
		m.exitPrompt = false
		return m, tea.Batch(m.commitLine(timingStyle.Render("Stopping the turn…")), m.stopTurnAndQuit())
	case "b", "B", "ctrl+c":
		m.exitPrompt = false
		return m, m.quit()
	case "esc", "c", "C":
		m.exitPrompt = false
		return m, m.input.Focus()
	}
	return m, nil
}

// mayBeRunning reports whether a turn is running or an input of this TUI
// may have started one the event stream has not reported yet (in flight,
// or its submission result was lost).
func (m *Model) mayBeRunning() bool {
	return m.thinking || len(m.pendingInputs) > 0
}

// stopTarget is the turn this TUI shows as running plus its own inputs
// whose turn the event stream has not reported yet.
func (m *Model) stopTarget() zotigodclient.StopTarget {
	inputs := make([]string, 0, len(m.pendingInputs))
	for id := range m.pendingInputs {
		inputs = append(inputs, id)
	}
	return zotigodclient.StopTarget{
		TurnID:      m.openTurnID,
		InputIDs:    inputs,
		Submissions: m.submissions,
	}
}

func (m *Model) stopTurnCmd(done func(zotigodclient.StopResult, error) tea.Msg) tea.Cmd {
	service, sessionID, target, parent := m.service, m.session.ID, m.stopTarget(), m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, stopTimeout)
		defer cancel()
		result, err := zotigodclient.StopTurn(ctx, service, sessionID, target)
		return done(result, err)
	}
}

// stopTurnAndQuit exits only after the service accepted the stop or the
// turn is known to have finished; a failed stop keeps the TUI open.
func (m *Model) stopTurnAndQuit() tea.Cmd {
	return m.stopTurnCmd(func(result zotigodclient.StopResult, err error) tea.Msg {
		return exitStoppedMsg{result: result, err: err}
	})
}

func stopResultLine(result zotigodclient.StopResult) string {
	switch result {
	case zotigodclient.StopRequested:
		return "Stopped the running turn."
	case zotigodclient.StopAlreadyEnded:
		return "The turn had already finished."
	default:
		return "No turn was running."
	}
}

// imageReferencePattern matches @path references in the prompt.
var imageReferencePattern = regexp.MustCompile(`(?:^|\s)@([^\s]+)`)

// buildMessageRequest turns prompt text into a service message. Readable
// @image references become inline images; other references stay as text
// for the agent to resolve against the session cwd.
func buildMessageRequest(text string) zotigodclient.MessageRequest {
	req := zotigodclient.MessageRequest{ClientMessageID: "tui_" + uuid.NewString()}
	var body strings.Builder
	lastIndex := 0
	for _, match := range imageReferencePattern.FindAllStringSubmatchIndex(text, -1) {
		start, end := match[0], match[1]
		path := text[match[2]:match[3]]
		body.WriteString(text[lastIndex:start])
		if image, ok := readImageReference(path); ok {
			req.Images = append(req.Images, image)
		} else {
			body.WriteString(text[start:end])
		}
		lastIndex = end
	}
	body.WriteString(text[lastIndex:])
	req.Text = strings.TrimSpace(body.String())
	return req
}

func readImageReference(path string) (zotigodclient.Image, bool) {
	if !isImagePath(path) {
		return zotigodclient.Image{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return zotigodclient.Image{}, false
	}
	mime := "image/png"
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		mime = "image/jpeg"
	case ".webp":
		mime = "image/webp"
	}
	return zotigodclient.Image{MimeType: mime, DataBase64: base64.StdEncoding.EncodeToString(data)}, true
}

// submitInput sends input to the service. The service decides whether it
// starts a turn or steers the open one; the TUI renders the input at once
// and recognizes the durable copy by its client message id.
func (m *Model) submitInput(text string) tea.Cmd {
	req := buildMessageRequest(text)
	m.ownInputs[req.ClientMessageID] = true
	m.pendingInputs[req.ClientMessageID] = true
	submissions := m.submissions
	if !m.thinking {
		m.thinking = true
		m.turnStartTime = time.Now()
	}
	rendered := "\n" + userMarkerStyle.Render("❯ ") + text
	service, sessionID, ctx := m.service, m.session.ID, m.ctx
	return tea.Batch(m.finishStreamBlock(), m.commitLine(rendered), func() tea.Msg {
		command, err := zotigodclient.SubmitTracked(ctx, service, sessionID, req, submissions)
		return submitResultMsg{inputID: req.ClientMessageID, command: command, err: err}
	})
}

func (m *Model) handleSubmitResult(msg submitResultMsg) tea.Cmd {
	if msg.err != nil && !zotigodclient.SubmissionRejectedErr(msg.err) {
		// The service may have recorded it and started a turn; keep it as
		// a stop target until the event stream shows where it went.
		if m.openTurnID == "" {
			m.thinking = false
		}
		return m.commitLine(warningStyle.Render("⚠ ") + "Could not confirm the message was accepted; if it started a turn it will appear here and Ctrl+C can still stop it (" + msg.err.Error() + ")")
	}
	if msg.err != nil {
		delete(m.ownInputs, msg.inputID)
		delete(m.pendingInputs, msg.inputID)
		if m.openTurnID == "" {
			m.thinking = false
		}
		return m.commitLine(errorStyle.Render("✗ ") + "Message not accepted: " + msg.err.Error())
	}
	if msg.command.Type == "steering" {
		switch msg.command.TurnID {
		case "":
		case m.openTurnID:
			delete(m.pendingInputs, msg.inputID)
		default:
			// Joined a turn this TUI has not seen start; StopTurn finds it
			// from the input until that turn's events arrive.
			m.steeringTurns[msg.inputID] = msg.command.TurnID
		}
		return m.commitLine(timingStyle.Render("↳ added to the running turn"))
	}
	return nil
}

func (m *Model) requestPause() tea.Cmd {
	if m.stopping {
		return nil
	}
	m.stopping = true
	inputs := m.stopTarget().InputIDs
	return tea.Batch(m.commitLine(timingStyle.Render("Stopping…")), m.stopTurnCmd(func(result zotigodclient.StopResult, err error) tea.Msg {
		return stopFinishedMsg{inputs: inputs, result: result, err: err}
	}))
}

// nextApprovalPolicy is the Shift+Tab order: Manual → Auto → Full access.
func nextApprovalPolicy(current string) string {
	switch current {
	case "manual":
		return "auto"
	case "bypass_permissions":
		return "manual"
	default:
		return "bypass_permissions"
	}
}

// cycleApprovalPolicy moves the session to the next approval policy. The
// service only changes policy while the session is idle.
func (m *Model) cycleApprovalPolicy() tea.Cmd {
	target := nextApprovalPolicy(m.approvalPolicy)
	service, sessionID, ctx := m.service, m.session.ID, m.ctx
	return func() tea.Msg {
		if err := service.SetApprovalPolicy(ctx, sessionID, target); err != nil {
			return serviceNoticeMsg(errorStyle.Render("✗ ") + "Approval policy unchanged: " + err.Error())
		}
		session, err := service.GetSession(ctx, sessionID)
		if err != nil {
			return serviceNoticeMsg(errorStyle.Render("✗ ") + "Refresh session: " + err.Error())
		}
		return sessionRefreshedMsg(session)
	}
}

func (m *Model) refreshSession() tea.Cmd {
	service, sessionID, ctx := m.service, m.session.ID, m.ctx
	return func() tea.Msg {
		session, err := service.GetSession(ctx, sessionID)
		if err != nil {
			return nil
		}
		return sessionRefreshedMsg(session)
	}
}

func (m *Model) buildCmdEnv(output *strings.Builder) *commands.Environment {
	service, sessionID, workDir := m.service, m.session.ID, m.session.WorkingDirectory
	return &commands.Environment{
		Output: func(format string, args ...interface{}) {
			fmt.Fprintf(output, format+"\n", args...)
		},
		NewSession: m.startNewSession,
		Compact: func() error {
			if m.thinking {
				return fmt.Errorf("compaction runs between turns; wait for the running turn or stop it first")
			}
			m.pendingCmds = append(m.pendingCmds, m.submitInput("/compact"))
			return nil
		},
		Skills: func(ctx context.Context, reload bool) ([]commands.SkillInfo, error) {
			listed, err := service.Skills(ctx, sessionID, reload)
			if err != nil {
				return nil, err
			}
			result := make([]commands.SkillInfo, 0, len(listed))
			for _, skill := range listed {
				if skill.Enabled {
					result = append(result, commands.SkillInfo{Name: skill.Name, Description: skill.Description, Scope: skill.Scope})
				}
			}
			return result, nil
		},
		GetModels: func() []string {
			profiles, err := service.Profiles(m.ctx, workDir)
			if err != nil {
				return nil
			}
			names := make([]string, 0, len(profiles.Profiles))
			for _, profile := range profiles.Profiles {
				names = append(names, fmt.Sprintf("%s (%s/%s)", profile.Name, profile.Provider, profile.Model))
			}
			return names
		},
		SetModel: func(profile string) error {
			return service.SetProfile(m.ctx, sessionID, profile)
		},
		Usage: func() commands.UsageReport {
			report := commands.UsageReport{Session: m.sessionUsage, Turns: m.turns}
			if usage := m.contextUsage; usage != nil && usage.Status == "available" {
				report.ContextTokens, report.ContextWindow = usage.Tokens, usage.Window
			}
			return report
		},
	}
}

func waitForServiceEvent(events <-chan zotigodclient.Event, sessionID string) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-events
		if !ok {
			return nil
		}
		return serviceEventMsg{sessionID: sessionID, event: event}
	}
}

func (m *Model) nextServiceEvent() tea.Cmd {
	if m.streamClosed {
		return nil
	}
	return waitForServiceEvent(m.events, m.session.ID)
}

// startNewSession replaces the bound session with a fresh one in the same
// directory, keeping the policy and profile. The service decides ownership
// again from the directory, exactly as at startup.
func (m *Model) startNewSession() error {
	if m.thinking {
		return fmt.Errorf("a turn is still running; wait for it or stop it with Esc first")
	}
	created, err := m.service.CreateSession(m.ctx, zotigodclient.CreateSessionRequest{
		WorkingDirectory: m.session.WorkingDirectory,
		Profile:          m.session.Profile,
		ApprovalPolicy:   m.approvalPolicy,
	})
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	m.cancel()
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.session = created
	m.history, m.historyTruncated, m.cursor = nil, false, 0
	m.streamClosed, m.reconnecting = false, false
	m.streamed = make(map[string]map[string]string)
	m.finalized = make(map[string]bool)
	m.ownInputs = make(map[string]bool)
	m.pendingInputs = make(map[string]bool)
	m.steeringTurns = make(map[string]string)
	m.endTurn()
	m.sessionUsage, m.lastTurnUsage, m.turns = protocol.Usage{}, nil, 0
	m.contextUsage = created.ContextUsage
	m.approvalPolicy = created.ApprovalPolicy
	m.events = m.service.Subscribe(m.ctx, m.session.ID, 0, zotigodclient.StreamOptions{})
	m.pendingCmds = append(m.pendingCmds,
		m.commitLine("\n"+headerStyle.Render("── New session ──")),
		m.commitLine(renderSessionBanner(m.session, m.location, m.embedded)),
		waitForServiceEvent(m.events, m.session.ID))
	return nil
}
