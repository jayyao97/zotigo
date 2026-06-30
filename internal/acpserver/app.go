package acpserver

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"time"

	"github.com/jayyao97/zotigo/core/acp"
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	_ "github.com/jayyao97/zotigo/core/providers/anthropic"
	_ "github.com/jayyao97/zotigo/core/providers/gemini"
	_ "github.com/jayyao97/zotigo/core/providers/openai"
	"github.com/jayyao97/zotigo/core/runner"
	"github.com/jayyao97/zotigo/internal/wiring"
)

type sessionState struct {
	mu      sync.Mutex
	runner  *runner.Runner
	session *acp.Session

	cancelFn context.CancelFunc
}

// Run starts the ACP server and returns a process exit code.
func Run(args []string) int {
	_ = args // reserved for symmetry with cliapp.Run

	log.SetOutput(os.Stderr)
	log.SetPrefix("[zotigo-acp] ")
	log.SetFlags(log.LstdFlags)
	logger := log.New(os.Stderr, "[zotigo-acp] ", log.LstdFlags)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cm := config.NewManager()
	cfg, err := cm.Load()
	if err != nil {
		logger.Printf("Failed to load config: %v", err)
		return 1
	}

	profile, ok := cfg.Profiles[cfg.DefaultProfile]
	if !ok {
		logger.Printf("Profile %q not found", cfg.DefaultProfile)
		return 1
	}

	state := newServerState(cfg, profile, logger)
	acpServer := state.newServer(&stdioRWC{r: os.Stdin, w: os.Stdout})
	state.server = acpServer

	logger.Println("Starting ACP server on stdio...")
	if err := acpServer.Run(ctx); err != nil {
		logger.Printf("ACP server error: %v", err)
		return 1
	}
	return 0
}

type serverState struct {
	cfg     *config.Config
	profile config.ProfileConfig
	logger  *log.Logger

	server *acp.Server

	mu       sync.RWMutex
	sessions map[string]*sessionState
}

func newServerState(cfg *config.Config, profile config.ProfileConfig, logger *log.Logger) *serverState {
	return &serverState{
		cfg:      cfg,
		profile:  profile,
		logger:   logger,
		sessions: make(map[string]*sessionState),
	}
}

func (s *serverState) newServer(rwc io.ReadWriteCloser) *acp.Server {
	return acp.NewServer(rwc,
		acp.OnInitialized(func(caps acp.ClientCapabilities) {
			s.logger.Printf("Client connected, fs.read=%v, fs.write=%v, terminal=%v",
				caps.FS.ReadTextFile, caps.FS.WriteTextFile, caps.Terminal)
		}),
		acp.OnSessionNew(s.onSessionNew),
		acp.OnSessionPrompt(s.onSessionPrompt),
		acp.OnSessionCancel(s.onSessionCancel),
	)
}

func (s *serverState) onSessionNew(ctx context.Context, params acp.SessionNewParams) (string, error) {
	workDir := params.Cwd
	if workDir == "" {
		workDir, _ = os.Getwd()
	}

	sessionID := fmt.Sprintf("acp-%d", time.Now().UnixNano())
	exec := acp.NewRemoteExecutor(s.server, sessionID, workDir)

	home, _ := os.UserHomeDir()
	transcriptDir := filepath.Join(home, ".zotigo", "sessions", "compacted")

	sm, err := wiring.NewSkillManager(workDir)
	if err != nil {
		s.logger.Printf("Warning: failed to load skills: %v", err)
	}
	pb := wiring.NewSystemPromptBuilder(wiring.PromptConfig{
		WorkDir:      workDir,
		SkillManager: sm,
	})
	ucb := wiring.NewUserContextBuilder(wiring.PromptConfig{
		WorkDir:                    workDir,
		Transport:                  "ACP (Editor Integration)",
		IncludeProjectInstructions: true,
	})

	ag, err := wiring.NewAgent(wiring.AgentConfig{
		Profile:            s.profile,
		Executor:           exec,
		PromptBuilder:      pb,
		UserContextBuilder: ucb,
		ApprovalPolicy:     agent.ApprovalPolicyManual,
		TranscriptDir:      transcriptDir,
	})
	if err != nil {
		return "", fmt.Errorf("failed to create agent: %w", err)
	}

	// ACP does not own an LSP manager or subagent scope here, so LSP and
	// spawn tools stay disabled by leaving those host options unset.
	if err := wiring.RegisterDefaultTools(ag, wiring.ToolSetConfig{
		Config:  s.cfg,
		Profile: s.profile,
	}); err != nil {
		return "", fmt.Errorf("failed to register tools: %w", err)
	}
	ag.SetSkillManager(sm)

	acpTransport := acp.NewTransport(s.server, sessionID)
	r := runner.New(ag, acpTransport)

	sess := acp.NewSession(sessionID, workDir)
	s.server.RegisterSession(sessionID, sess)

	s.mu.Lock()
	s.sessions[sessionID] = &sessionState{
		runner:  r,
		session: sess,
	}
	s.mu.Unlock()

	s.logger.Printf("Session created: %s (workDir=%s)", sessionID, workDir)
	return sessionID, nil
}

func (s *serverState) onSessionPrompt(ctx context.Context, sessionID string, text string, images []acp.ContentBlock) acp.PromptResult {
	state, ok := s.session(sessionID)
	if !ok {
		return acp.PromptResult{Err: fmt.Errorf("session %q not found", sessionID)}
	}

	cancelPrompt(state)

	// Derive promptCtx from both the server ctx and the request ctx, so
	// cancellation from either side (session/cancel or transport close)
	// propagates to the agent.
	promptCtx, cancel := context.WithCancel(ctx)

	state.mu.Lock()
	state.cancelFn = cancel
	state.mu.Unlock()
	state.session.SetCancel(cancel)

	msg := protocol.NewUserMessage(text)
	for _, img := range images {
		if img.Data != "" {
			msg.Content = append(msg.Content, protocol.ContentPart{
				Type: protocol.ContentTypeImage,
				Image: &protocol.MediaPart{
					Data:      []byte(img.Data),
					MediaType: img.MimeType,
				},
			})
		}
	}

	err := state.runner.RunFullTurn(promptCtx, msg)

	cancelled := promptCtx.Err() != nil
	cancel()

	if err != nil {
		if cancelled {
			return acp.PromptResult{StopReason: acp.StopReasonCancelled}
		}
		return acp.PromptResult{Err: err}
	}

	return acp.PromptResult{StopReason: acp.StopReasonEndTurn}
}

func (s *serverState) onSessionCancel(_ context.Context, sessionID string) {
	state, ok := s.session(sessionID)
	if ok {
		cancelPrompt(state)
	}
}

func (s *serverState) session(sessionID string) (*sessionState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.sessions[sessionID]
	return state, ok
}

func cancelPrompt(s *sessionState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelFn != nil {
		s.cancelFn()
		s.cancelFn = nil
	}
}

type stdioRWC struct {
	r io.Reader
	w io.Writer
}

func (s *stdioRWC) Read(p []byte) (int, error)  { return s.r.Read(p) }
func (s *stdioRWC) Write(p []byte) (int, error) { return s.w.Write(p) }
func (s *stdioRWC) Close() error                { return nil }
