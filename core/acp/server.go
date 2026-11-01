package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/jayyao97/zotigo/internal/buildinfo"
	"go.lsp.dev/jsonrpc2"
)

const (
	// ProtocolVersion is the ACP protocol version we support.
	ProtocolVersion = 1

	agentName = "zotigo"
)

// PromptResult carries the result of a session/prompt callback so the
// JSON-RPC response can include the stopReason.
type PromptResult struct {
	StopReason string // one of the StopReason* constants
	Err        error
}

// Server handles ACP JSON-RPC communication over stdio.
type Server struct {
	conn jsonrpc2.Conn

	mu       sync.RWMutex
	sessions map[string]*Session

	// serverCtx is the long-lived context for the server's lifetime.
	serverCtx context.Context

	// callbacks
	onSessionNew    func(ctx context.Context, params SessionNewParams) (string, error)
	onSessionPrompt func(ctx context.Context, sessionID string, text string, images []ContentBlock) PromptResult
	onSessionCancel func(ctx context.Context, sessionID string)
	onSessionLoad   func(ctx context.Context, params SessionLoadParams) (afterReply func(), err error)
	onSessionList   func(ctx context.Context, params SessionListParams) (SessionListResult, error)
	onInitialized   func(caps ClientCapabilities)
	sessionModes    func(ctx context.Context, sessionID string) *SessionModeState
	onSetMode       func(ctx context.Context, sessionID string, modeID string) error

	clientCaps ClientCapabilities
}

// ServerOption configures the Server.
type ServerOption func(*Server)

// OnSessionNew sets the callback invoked when a new session is requested.
func OnSessionNew(fn func(ctx context.Context, params SessionNewParams) (string, error)) ServerOption {
	return func(s *Server) { s.onSessionNew = fn }
}

// OnSessionPrompt sets the callback invoked when the client sends a prompt.
// The callback must block until the turn is complete and return a PromptResult
// with the appropriate StopReason, because the JSON-RPC response is deferred
// until this callback returns (per ACP spec).
func OnSessionPrompt(fn func(ctx context.Context, sessionID string, text string, images []ContentBlock) PromptResult) ServerOption {
	return func(s *Server) { s.onSessionPrompt = fn }
}

// OnSessionCancel sets the callback invoked when the client cancels.
func OnSessionCancel(fn func(ctx context.Context, sessionID string)) ServerOption {
	return func(s *Server) { s.onSessionCancel = fn }
}

// OnSessionLoad enables session/load and advertises the loadSession
// capability. Per the ACP spec the callback replays the conversation with
// session/update notifications before returning; the reply is sent after it
// returns, and afterReply (if non-nil) runs once the reply is written, for
// work that must follow it such as permission requests for the session.
func OnSessionLoad(fn func(ctx context.Context, params SessionLoadParams) (afterReply func(), err error)) ServerOption {
	return func(s *Server) { s.onSessionLoad = fn }
}

// OnSessionList replaces the default session/list answer, which only knows
// sessions registered in this process.
func OnSessionList(fn func(ctx context.Context, params SessionListParams) (SessionListResult, error)) ServerOption {
	return func(s *Server) { s.onSessionList = fn }
}

// OnSessionModes advertises session modes. state is consulted after
// session/new and session/load to fill the response's modes; set handles
// session/set_mode and returns an error for an unknown or rejected mode.
// Agents report later changes with SendCurrentMode.
func OnSessionModes(state func(ctx context.Context, sessionID string) *SessionModeState, set func(ctx context.Context, sessionID string, modeID string) error) ServerOption {
	return func(s *Server) {
		s.sessionModes = state
		s.onSetMode = set
	}
}

// OnInitialized sets the callback invoked after initialize handshake.
func OnInitialized(fn func(caps ClientCapabilities)) ServerOption {
	return func(s *Server) { s.onInitialized = fn }
}

// NewServer creates a new ACP server over the given reader/writer (typically stdin/stdout).
func NewServer(rwc io.ReadWriteCloser, opts ...ServerOption) *Server {
	s := &Server{
		sessions: make(map[string]*Session),
	}
	for _, opt := range opts {
		opt(s)
	}

	stream := jsonrpc2.NewRawStream(rwc)
	s.conn = jsonrpc2.NewConn(stream)
	return s
}

// Conn returns the underlying JSON-RPC connection for direct calls.
func (s *Server) Conn() jsonrpc2.Conn {
	return s.conn
}

// ClientCaps returns the negotiated client capabilities.
func (s *Server) ClientCaps() ClientCapabilities {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.clientCaps
}

// Run starts the JSON-RPC message loop. Blocks until the connection is closed.
func (s *Server) Run(ctx context.Context) error {
	s.serverCtx = ctx
	// Each incoming message runs in its own goroutine. jsonrpc2.AsyncHandler
	// is not suitable: it starts a request only after the previous one has
	// replied, so a long-running session/prompt would block session/cancel
	// and every other session until its turn ended.
	handler := jsonrpc2.ReplyHandler(s.handle)
	s.conn.Go(ctx, func(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
		go func() { _ = handler(ctx, reply, req) }()
		return nil
	})
	select {
	case <-s.conn.Done():
		return s.conn.Err()
	case <-ctx.Done():
		_ = s.conn.Close()
		return ctx.Err()
	}
}

// ErrConnectionClosed is returned by agent-to-client requests that were
// still waiting when the client connection closed.
var ErrConnectionClosed = errors.New("acp: client connection closed")

// call issues a request to the client. jsonrpc2 never completes a pending
// call when the stream ends, so the wait is also bounded by the connection.
func (s *Server) call(ctx context.Context, method string, params, result any) (jsonrpc2.ID, error) {
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-s.conn.Done():
			cancel()
		case <-callCtx.Done():
		}
	}()
	id, err := s.conn.Call(callCtx, method, params, result)
	if err != nil && ctx.Err() == nil && callCtx.Err() != nil {
		return id, ErrConnectionClosed
	}
	return id, err
}

// Done is closed when the client connection is closed.
func (s *Server) Done() <-chan struct{} {
	return s.conn.Done()
}

// --- Outgoing helpers (Agent → Client) ---

// SendUpdate sends a session/update notification to the client.
func (s *Server) SendUpdate(ctx context.Context, sessionID string, update SessionUpdate) error {
	return s.conn.Notify(ctx, MethodSessionUpdate, SessionUpdateParams{
		SessionID: sessionID,
		Update:    update,
	})
}

// SendTextChunk sends an agent_message_chunk update.
func (s *Server) SendTextChunk(ctx context.Context, sessionID string, text string) error {
	return s.SendUpdate(ctx, sessionID, SessionUpdate{
		"sessionUpdate": "agent_message_chunk",
		"content": ContentBlock{
			Type: "text",
			Text: text,
		},
	})
}

// SendUserChunk sends a user_message_chunk update, used when replaying a
// loaded session and for input submitted by other clients.
func (s *Server) SendUserChunk(ctx context.Context, sessionID string, text string) error {
	return s.SendUpdate(ctx, sessionID, SessionUpdate{
		"sessionUpdate": "user_message_chunk",
		"content": ContentBlock{
			Type: "text",
			Text: text,
		},
	})
}

// SendCurrentMode reports a session mode change (current_mode_update).
func (s *Server) SendCurrentMode(ctx context.Context, sessionID string, modeID string) error {
	return s.SendUpdate(ctx, sessionID, SessionUpdate{
		"sessionUpdate": "current_mode_update",
		"currentModeId": modeID,
	})
}

// SendThoughtChunk sends an agent_thought_chunk update.
func (s *Server) SendThoughtChunk(ctx context.Context, sessionID string, text string) error {
	return s.SendUpdate(ctx, sessionID, SessionUpdate{
		"sessionUpdate": "agent_thought_chunk",
		"content": ContentBlock{
			Type: "text",
			Text: text,
		},
	})
}

// SendToolCall sends a tool_call update (initial call start).
func (s *Server) SendToolCall(ctx context.Context, sessionID string, toolCallID, title, kind, status string) error {
	return s.SendUpdate(ctx, sessionID, SessionUpdate{
		"sessionUpdate": "tool_call",
		"toolCallId":    toolCallID,
		"title":         title,
		"kind":          kind,
		"status":        status,
	})
}

// SendToolCallUpdate sends a tool_call_update notification.
func (s *Server) SendToolCallUpdate(ctx context.Context, sessionID string, toolCallID string, fields map[string]any) error {
	update := SessionUpdate{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    toolCallID,
	}
	for k, v := range fields {
		update[k] = v
	}
	return s.SendUpdate(ctx, sessionID, update)
}

// RequestPermission asks the client for permission to execute a tool call.
func (s *Server) RequestPermission(ctx context.Context, sessionID string, toolCall ToolCallData, options []PermissionOption) (PermissionOutcome, error) {
	var result RequestPermissionResult
	_, err := s.call(ctx, MethodRequestPerm, RequestPermissionParams{
		SessionID: sessionID,
		ToolCall:  toolCall,
		Options:   options,
	}, &result)
	if err != nil {
		return PermissionOutcome{}, err
	}
	return result.Outcome, nil
}

// ReadTextFile requests the client to read a file.
func (s *Server) ReadTextFile(ctx context.Context, sessionID, path string) (string, error) {
	var result FSReadTextFileResult
	_, err := s.call(ctx, MethodFSReadTextFile, FSReadTextFileParams{
		SessionID: sessionID,
		Path:      path,
	}, &result)
	if err != nil {
		return "", err
	}
	return result.Content, nil
}

// WriteTextFile requests the client to write a file.
func (s *Server) WriteTextFile(ctx context.Context, sessionID, path, content string) error {
	var result FSWriteTextFileResult
	_, err := s.call(ctx, MethodFSWriteTextFile, FSWriteTextFileParams{
		SessionID: sessionID,
		Path:      path,
		Content:   content,
	}, &result)
	return err
}

// TerminalExec creates a terminal, waits for exit, and returns the output.
func (s *Server) TerminalExec(ctx context.Context, sessionID, command string, args []string, cwd string, env []EnvVariable) (output string, exitCode int, err error) {
	// Create terminal
	var createResult TerminalCreateResult
	_, err = s.call(ctx, MethodTerminalCreate, TerminalCreateParams{
		SessionID: sessionID,
		Command:   command,
		Args:      args,
		Cwd:       cwd,
		Env:       env,
	}, &createResult)
	if err != nil {
		return "", -1, fmt.Errorf("terminal/create: %w", err)
	}

	tid := createResult.TerminalID

	// Ensure terminal is released even if wait or output fails.
	defer func() {
		_ = s.conn.Notify(ctx, MethodTerminalRelease, TerminalReleaseParams{
			SessionID:  sessionID,
			TerminalID: tid,
		})
	}()

	// Wait for exit
	var waitResult TerminalWaitResult
	_, err = s.call(ctx, MethodTerminalWait, TerminalWaitParams{
		SessionID:  sessionID,
		TerminalID: tid,
	}, &waitResult)
	if err != nil {
		return "", -1, fmt.Errorf("terminal/wait_for_exit: %w", err)
	}

	exitCode = -1
	if waitResult.ExitCode != nil {
		exitCode = *waitResult.ExitCode
	}

	// Get output
	var outResult TerminalOutputResult
	_, err = s.call(ctx, MethodTerminalOutput, TerminalOutputParams{
		SessionID:  sessionID,
		TerminalID: tid,
	}, &outResult)
	if err != nil {
		return "", exitCode, fmt.Errorf("terminal/output: %w", err)
	}

	return outResult.Output, exitCode, nil
}

// --- Incoming dispatch ---

func (s *Server) handle(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	switch req.Method() {
	case MethodInitialize:
		return s.handleInitialize(ctx, reply, req)
	case MethodSessionNew:
		return s.handleSessionNew(ctx, reply, req)
	case MethodSessionLoad:
		return s.handleSessionLoad(ctx, reply, req)
	case MethodSessionPrompt:
		return s.handleSessionPrompt(ctx, reply, req)
	case MethodSessionCancel:
		return s.handleSessionCancel(ctx, reply, req)
	case MethodSessionList:
		return s.handleSessionList(ctx, reply, req)
	case MethodSessionSetMode:
		return s.handleSessionSetMode(ctx, reply, req)
	default:
		return reply(ctx, nil, fmt.Errorf("method %q: %w", req.Method(), jsonrpc2.ErrMethodNotFound))
	}
}

func (s *Server) handleInitialize(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params InitializeParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		return reply(ctx, nil, fmt.Errorf("invalid params: %w", err))
	}

	s.mu.Lock()
	s.clientCaps = params.ClientCapabilities
	s.mu.Unlock()

	if s.onInitialized != nil {
		s.onInitialized(params.ClientCapabilities)
	}

	return reply(ctx, InitializeResult{
		ProtocolVersion: ProtocolVersion,
		AgentInfo: &Implementation{
			Name:    agentName,
			Version: buildinfo.Version,
		},
		AgentCapabilities: AgentCapabilities{
			LoadSession: s.onSessionLoad != nil,
			PromptCapabilities: &PromptCapabilities{
				Image: true,
			},
		},
	}, nil)
}

func (s *Server) handleSessionNew(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params SessionNewParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		return reply(ctx, nil, fmt.Errorf("invalid params: %w", err))
	}

	if s.onSessionNew == nil {
		return reply(ctx, nil, fmt.Errorf("session creation not supported"))
	}

	sessionID, err := s.onSessionNew(ctx, params)
	if err != nil {
		return reply(ctx, nil, err)
	}

	result := SessionNewResult{SessionID: sessionID}
	if s.sessionModes != nil {
		result.Modes = s.sessionModes(ctx, sessionID)
	}
	return reply(ctx, result, nil)
}

func (s *Server) handleSessionLoad(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	if s.onSessionLoad == nil {
		return reply(ctx, nil, fmt.Errorf("method %q: %w", req.Method(), jsonrpc2.ErrMethodNotFound))
	}
	var params SessionLoadParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		return reply(ctx, nil, fmt.Errorf("invalid params: %w", err))
	}
	afterReply, err := s.onSessionLoad(ctx, params)
	if err != nil {
		return reply(ctx, nil, err)
	}
	result := SessionLoadResult{}
	if s.sessionModes != nil {
		result.Modes = s.sessionModes(ctx, params.SessionID)
	}
	replyErr := reply(ctx, result, nil)
	if afterReply != nil {
		afterReply()
	}
	return replyErr
}

// handleSessionPrompt blocks until the turn completes, then replies with stopReason.
func (s *Server) handleSessionPrompt(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params SessionPromptParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		return reply(ctx, nil, fmt.Errorf("invalid params: %w", err))
	}

	text, images, err := promptInput(params.Prompt)
	if err != nil {
		return reply(ctx, nil, err)
	}

	if s.onSessionPrompt == nil {
		return reply(ctx, nil, fmt.Errorf("prompt handling not supported"))
	}

	// Per ACP spec, the session/prompt request stays open until the turn
	// finishes. Run dispatches each request in its own goroutine, so blocking
	// here does not stall cancel or other sessions.
	//
	// We derive a context that cancels if either the server shuts down
	// (serverCtx) or the request is cancelled (ctx, e.g. client disconnects).
	promptCtx, promptCancel := context.WithCancel(s.serverCtx)
	go func() {
		select {
		case <-ctx.Done():
			promptCancel()
		case <-promptCtx.Done():
		}
	}()
	defer promptCancel()

	result := s.onSessionPrompt(promptCtx, params.SessionID, text, images)
	if result.Err != nil {
		_ = s.SendTextChunk(promptCtx, params.SessionID, fmt.Sprintf("Error: %v", result.Err))
		return reply(ctx, SessionPromptResult{StopReason: StopReasonEndTurn}, nil)
	}
	stopReason := result.StopReason
	if stopReason == "" {
		stopReason = StopReasonEndTurn
	}
	return reply(ctx, SessionPromptResult{StopReason: stopReason}, nil)
}

// handleSessionCancel handles the cancel notification. Nothing is written
// on the wire for a notification, but jsonrpc2.ReplyHandler panics unless
// every handled message calls reply, so it is still invoked.
func (s *Server) handleSessionCancel(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params SessionCancelParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		return reply(ctx, nil, fmt.Errorf("invalid params: %w", err))
	}

	if s.onSessionCancel != nil {
		s.onSessionCancel(s.serverCtx, params.SessionID)
	}

	return reply(ctx, nil, nil)
}

// promptInput flattens prompt blocks into text plus images. Resource links
// (which every agent must accept) become text references so the agent can
// open them; block types this agent did not advertise are rejected rather
// than silently dropped.
func promptInput(blocks []ContentBlock) (string, []ContentBlock, error) {
	var text string
	var images []ContentBlock
	appendText := func(value string) {
		if text != "" {
			text += "\n"
		}
		text += value
	}
	for _, block := range blocks {
		switch block.Type {
		case "text":
			appendText(block.Text)
		case "image":
			images = append(images, block)
		case "resource_link":
			name := block.Name
			if name == "" {
				name = block.URI
			}
			appendText(fmt.Sprintf("[%s](%s)", name, block.URI))
		default:
			return "", nil, fmt.Errorf("unsupported prompt content type %q", block.Type)
		}
	}
	return text, images, nil
}

func (s *Server) handleSessionList(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params SessionListParams
	if req.Params() != nil {
		_ = json.Unmarshal(req.Params(), &params)
	}
	if s.onSessionList != nil {
		result, err := s.onSessionList(ctx, params)
		if err != nil {
			return reply(ctx, nil, err)
		}
		return reply(ctx, result, nil)
	}

	s.mu.RLock()
	var sessions []SessionInfo
	for id, sess := range s.sessions {
		sessions = append(sessions, SessionInfo{
			SessionID: id,
			Cwd:       sess.WorkingDir,
		})
	}
	s.mu.RUnlock()

	return reply(ctx, SessionListResult{Sessions: sessions}, nil)
}

func (s *Server) handleSessionSetMode(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params SessionSetModeParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		return reply(ctx, nil, fmt.Errorf("invalid params: %w", err))
	}

	if s.onSetMode != nil {
		if err := s.onSetMode(ctx, params.SessionID, params.ModeID); err != nil {
			return reply(ctx, nil, err)
		}
	}
	return reply(ctx, SessionSetModeResult{}, nil)
}

// RegisterSession adds a session to the server's session map.
func (s *Server) RegisterSession(id string, sess *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = sess
}
