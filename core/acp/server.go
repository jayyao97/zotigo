package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"go.lsp.dev/jsonrpc2"
)

const (
	// ProtocolVersion is the ACP protocol version we support.
	ProtocolVersion = 1

	agentName    = "zotigo"
	agentVersion = "0.1.0"
)

// Server handles ACP JSON-RPC communication over stdio.
type Server struct {
	conn jsonrpc2.Conn

	mu       sync.RWMutex
	sessions map[string]*Session

	// callbacks
	onSessionNew    func(ctx context.Context, params SessionNewParams) (string, error)
	onSessionPrompt func(ctx context.Context, sessionID string, text string, images []ContentBlock) error
	onSessionCancel func(ctx context.Context, sessionID string)
	onInitialized   func(caps ClientCapabilities)

	clientCaps ClientCapabilities
}

// ServerOption configures the Server.
type ServerOption func(*Server)

// OnSessionNew sets the callback invoked when a new session is requested.
func OnSessionNew(fn func(ctx context.Context, params SessionNewParams) (string, error)) ServerOption {
	return func(s *Server) { s.onSessionNew = fn }
}

// OnSessionPrompt sets the callback invoked when the client sends a prompt.
func OnSessionPrompt(fn func(ctx context.Context, sessionID string, text string, images []ContentBlock) error) ServerOption {
	return func(s *Server) { s.onSessionPrompt = fn }
}

// OnSessionCancel sets the callback invoked when the client cancels.
func OnSessionCancel(fn func(ctx context.Context, sessionID string)) ServerOption {
	return func(s *Server) { s.onSessionCancel = fn }
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

// ClientCapabilities returns the negotiated client capabilities.
func (s *Server) ClientCapabilities() ClientCapabilities {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.clientCaps
}

// Run starts the JSON-RPC message loop. Blocks until the connection is closed.
func (s *Server) Run(ctx context.Context) error {
	handler := jsonrpc2.AsyncHandler(jsonrpc2.ReplyHandler(s.handle))
	s.conn.Go(ctx, handler)
	select {
	case <-s.conn.Done():
		return s.conn.Err()
	case <-ctx.Done():
		s.conn.Close()
		return ctx.Err()
	}
}

// SendUpdate sends a session/update notification to the client.
func (s *Server) SendUpdate(ctx context.Context, sessionID string, update SessionUpdate) error {
	return s.conn.Notify(ctx, MethodSessionUpdate, SessionUpdateParams{
		SessionID: sessionID,
		Update:    update,
	})
}

// SendTextChunk sends a text content chunk to the client.
func (s *Server) SendTextChunk(ctx context.Context, sessionID string, text string) error {
	return s.SendUpdate(ctx, sessionID, SessionUpdate{
		Type: "agent_message_chunk",
		MessageChunk: &MessageChunk{
			Role:    "assistant",
			Content: text,
		},
	})
}

// SendToolCallUpdate sends a tool call status update to the client.
func (s *Server) SendToolCallUpdate(ctx context.Context, sessionID string, update ToolCallUpdate) error {
	return s.SendUpdate(ctx, sessionID, SessionUpdate{
		Type:           "tool_call_update",
		ToolCallUpdate: &update,
	})
}

// RequestPermission asks the client for permission to execute actions.
func (s *Server) RequestPermission(ctx context.Context, sessionID string, perms []PermissionDetail) ([]PermissionDecision, error) {
	var result RequestPermissionResult
	_, err := s.conn.Call(ctx, MethodRequestPerm, RequestPermissionParams{
		SessionID:   sessionID,
		Permissions: perms,
	}, &result)
	if err != nil {
		return nil, err
	}
	return result.Decisions, nil
}

// ReadTextFile requests the client to read a file.
func (s *Server) ReadTextFile(ctx context.Context, path string) (string, error) {
	var result FSReadTextFileResult
	_, err := s.conn.Call(ctx, MethodFSReadTextFile, FSReadTextFileParams{Path: path}, &result)
	if err != nil {
		return "", err
	}
	return result.Content, nil
}

// WriteTextFile requests the client to write a file.
func (s *Server) WriteTextFile(ctx context.Context, path, content string) error {
	var result FSWriteTextFileResult
	_, err := s.conn.Call(ctx, MethodFSWriteTextFile, FSWriteTextFileParams{
		Path:    path,
		Content: content,
	}, &result)
	return err
}

// TerminalExec creates a terminal, waits for exit, and returns the output.
func (s *Server) TerminalExec(ctx context.Context, command string, args []string, cwd string, env []string) (output string, exitCode int, err error) {
	// Create terminal
	var createResult TerminalCreateResult
	_, err = s.conn.Call(ctx, MethodTerminalCreate, TerminalCreateParams{
		Command: command,
		Args:    args,
		Cwd:     cwd,
		Env:     env,
	}, &createResult)
	if err != nil {
		return "", -1, fmt.Errorf("terminal/create: %w", err)
	}

	tid := createResult.TerminalID

	// Wait for exit
	var waitResult TerminalWaitResult
	_, err = s.conn.Call(ctx, MethodTerminalWait, TerminalWaitParams{TerminalID: tid}, &waitResult)
	if err != nil {
		return "", -1, fmt.Errorf("terminal/wait_for_exit: %w", err)
	}

	// Get output
	var outResult TerminalOutputResult
	_, err = s.conn.Call(ctx, MethodTerminalOutput, TerminalOutputParams{TerminalID: tid}, &outResult)
	if err != nil {
		return "", waitResult.ExitCode, fmt.Errorf("terminal/output: %w", err)
	}

	// Release
	_ = s.conn.Notify(ctx, MethodTerminalRelease, TerminalReleaseParams{TerminalID: tid})

	return outResult.Output, waitResult.ExitCode, nil
}

// handle dispatches incoming JSON-RPC requests.
func (s *Server) handle(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	switch req.Method() {
	case MethodInitialize:
		return s.handleInitialize(ctx, reply, req)
	case MethodSessionNew:
		return s.handleSessionNew(ctx, reply, req)
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
	s.clientCaps = params.Capabilities
	s.mu.Unlock()

	if s.onInitialized != nil {
		s.onInitialized(params.Capabilities)
	}

	return reply(ctx, InitializeResult{
		ProtocolVersion: ProtocolVersion,
		AgentInfo: AgentInfo{
			Name:    agentName,
			Version: agentVersion,
		},
		Capabilities: AgentCapabilities{
			LoadSession:      true,
			AvailableModes:   []string{"code", "ask", "architect"},
			SupportedPrompts: []string{"text", "image"},
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

	return reply(ctx, SessionNewResult{SessionID: sessionID}, nil)
}

func (s *Server) handleSessionPrompt(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params SessionPromptParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		return reply(ctx, nil, fmt.Errorf("invalid params: %w", err))
	}

	// Extract text and images from content blocks
	var text string
	var images []ContentBlock
	for _, block := range params.Content {
		switch block.Type {
		case "text":
			if text != "" {
				text += "\n"
			}
			text += block.Text
		case "image":
			images = append(images, block)
		}
	}

	if s.onSessionPrompt == nil {
		return reply(ctx, nil, fmt.Errorf("prompt handling not supported"))
	}

	// Reply immediately — actual response comes via session/update notifications
	if err := reply(ctx, SessionPromptResult{}, nil); err != nil {
		return err
	}

	// Process asynchronously
	go func() {
		if err := s.onSessionPrompt(ctx, params.SessionID, text, images); err != nil {
			// Send error as session update
			_ = s.SendUpdate(ctx, params.SessionID, SessionUpdate{
				Type: "agent_message_chunk",
				MessageChunk: &MessageChunk{
					Role:    "assistant",
					Content: fmt.Sprintf("Error: %v", err),
				},
			})
		}
	}()

	return nil
}

func (s *Server) handleSessionCancel(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params SessionCancelParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		return reply(ctx, nil, fmt.Errorf("invalid params: %w", err))
	}

	if s.onSessionCancel != nil {
		s.onSessionCancel(ctx, params.SessionID)
	}

	return reply(ctx, nil, nil)
}

func (s *Server) handleSessionList(ctx context.Context, reply jsonrpc2.Replier, _ jsonrpc2.Request) error {
	s.mu.RLock()
	var sessions []SessionInfo
	for id, sess := range s.sessions {
		sessions = append(sessions, SessionInfo{
			SessionID:  id,
			WorkingDir: sess.WorkingDir,
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

	// For now, acknowledge mode change
	return reply(ctx, SessionSetModeResult{Mode: params.Mode}, nil)
}

// RegisterSession adds a session to the server's session map.
func (s *Server) RegisterSession(id string, sess *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = sess
}
