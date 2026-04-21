// Command acp runs zotigo as an ACP (Agent Client Protocol) server over stdio.
// Editors spawn this process and communicate via JSON-RPC 2.0 on stdin/stdout.
//
// Usage:
//
//	zotigo-acp
//
// The process reads JSON-RPC messages from stdin and writes responses to stdout.
// Stderr is used for logging.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"time"

	"github.com/jayyao97/zotigo/core/acp"
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/agent/prompt"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	_ "github.com/jayyao97/zotigo/core/providers/anthropic"
	_ "github.com/jayyao97/zotigo/core/providers/gemini"
	_ "github.com/jayyao97/zotigo/core/providers/openai"
	"github.com/jayyao97/zotigo/core/runner"
	"github.com/jayyao97/zotigo/core/skills"
	"github.com/jayyao97/zotigo/core/tools/builtin"
)

func main() {
	// All log output goes to stderr (stdout is reserved for JSON-RPC)
	log.SetOutput(os.Stderr)
	log.SetPrefix("[zotigo-acp] ")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Load config
	cm := config.NewManager()
	cfg, err := cm.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	profile, ok := cfg.Profiles[cfg.DefaultProfile]
	if !ok {
		log.Fatalf("Profile %q not found", cfg.DefaultProfile)
	}

	// State: maps sessionID → runner + session
	type sessionState struct {
		mu      sync.Mutex // protects cancelFn
		runner  *runner.Runner
		session *acp.Session

		cancelFn context.CancelFunc
	}

	// cancelPrompt safely cancels any in-flight prompt for a session.
	cancelPrompt := func(s *sessionState) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.cancelFn != nil {
			s.cancelFn()
			s.cancelFn = nil
		}
	}

	var sessionsMu sync.RWMutex
	sessions := make(map[string]*sessionState)

	// stdio as ReadWriteCloser
	rwc := &stdioRWC{}

	var acpServer *acp.Server

	acpServer = acp.NewServer(rwc,
		acp.OnInitialized(func(caps acp.ClientCapabilities) {
			log.Printf("Client connected, fs.read=%v, fs.write=%v, terminal=%v",
				caps.FS.ReadTextFile, caps.FS.WriteTextFile, caps.Terminal)
		}),

		acp.OnSessionNew(func(ctx context.Context, params acp.SessionNewParams) (string, error) {
			workDir := params.Cwd
			if workDir == "" {
				workDir, _ = os.Getwd()
			}

			sessionID := fmt.Sprintf("acp-%d", time.Now().UnixNano())

			// Create remote executor that delegates to the editor
			exec := acp.NewRemoteExecutor(acpServer, sessionID, workDir)

			// System prompt
			pb := prompt.NewSystemPromptBuilder(
				prompt.WithDynamicSection("environment", func(pctx prompt.PromptContext) string {
					return fmt.Sprintf("Working directory: %s\nPlatform: %s\nTransport: ACP (Editor Integration)",
						pctx.WorkDir, pctx.Platform)
				}),
			)

			// Transcript dir
			home, _ := os.UserHomeDir()
			transcriptDir := filepath.Join(home, ".zotigo", "sessions", "compacted")

			// Create agent
			ag, err := agent.New(profile, exec,
				agent.WithSystemPromptBuilder(pb),
				agent.WithApprovalPolicy(agent.ApprovalPolicyManual),
				agent.WithTranscriptDir(transcriptDir),
			)
			if err != nil {
				return "", fmt.Errorf("failed to create agent: %w", err)
			}

			// Register tools (skip LSP — not available in ACP mode)
			registerTools(ag, cfg)

			// Skills
			sm := skills.NewSkillManager(workDir)
			_ = sm.Load()
			ag.SetSkillManager(sm)

			// Create ACP transport and Runner for this session
			acpTransport := acp.NewTransport(acpServer, sessionID)
			r := runner.New(ag, acpTransport)

			// Create ACP session
			sess := acp.NewSession(sessionID, workDir)
			acpServer.RegisterSession(sessionID, sess)

			sessionsMu.Lock()
			sessions[sessionID] = &sessionState{
				runner:  r,
				session: sess,
			}
			sessionsMu.Unlock()

			log.Printf("Session created: %s (workDir=%s)", sessionID, workDir)
			return sessionID, nil
		}),

		// OnSessionPrompt blocks until the turn finishes and returns a PromptResult
		// with stopReason, so the JSON-RPC response carries the correct stop reason.
		acp.OnSessionPrompt(func(ctx context.Context, sessionID string, text string, images []acp.ContentBlock) acp.PromptResult {
			sessionsMu.RLock()
			state, ok := sessions[sessionID]
			sessionsMu.RUnlock()
			if !ok {
				return acp.PromptResult{Err: fmt.Errorf("session %q not found", sessionID)}
			}

			// Cancel previous processing if any (mutex-protected)
			cancelPrompt(state)

			// Derive promptCtx from both the server ctx and the request ctx,
			// so cancellation from either side (session/cancel or transport close)
			// propagates to the agent.
			promptCtx, cancel := context.WithCancel(ctx)

			state.mu.Lock()
			state.cancelFn = cancel
			state.mu.Unlock()
			state.session.SetCancel(cancel)

			// Build user message
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

			// RunFullTurn handles the entire conversation turn including
			// approval loops. Events are sent to the editor via the ACP
			// transport automatically.
			err := state.runner.RunFullTurn(promptCtx, msg)

			// Check promptCtx (not the parent ctx) to detect cancellation,
			// since session/cancel calls cancel() on promptCtx.
			cancelled := promptCtx.Err() != nil
			cancel()

			if err != nil {
				if cancelled {
					return acp.PromptResult{StopReason: acp.StopReasonCancelled}
				}
				return acp.PromptResult{Err: err}
			}

			return acp.PromptResult{StopReason: acp.StopReasonEndTurn}
		}),

		acp.OnSessionCancel(func(_ context.Context, sessionID string) {
			sessionsMu.RLock()
			state, ok := sessions[sessionID]
			sessionsMu.RUnlock()
			if ok {
				cancelPrompt(state)
			}
		}),
	)

	log.Println("Starting ACP server on stdio...")
	if err := acpServer.Run(ctx); err != nil {
		log.Fatalf("ACP server error: %v", err)
	}
}

func registerTools(ag *agent.Agent, cfg *config.Config) {
	ag.RegisterTool(&builtin.ReadFileTool{})
	ag.RegisterTool(&builtin.WriteFileTool{})
	ag.RegisterTool(&builtin.EditTool{})

	ag.RegisterTool(&builtin.ShellTool{})
	ag.RegisterTool(&builtin.GrepTool{})
	ag.RegisterTool(&builtin.GlobTool{})

	webClient := builtin.NewWebClient(builtin.WebConfig{
		TavilyAPIKey: cfg.Tools.Web.TavilyAPIKey,
		UserAgent:    cfg.Tools.Web.UserAgent,
		Timeout:      time.Duration(cfg.Tools.Web.TimeoutSec) * time.Second,
		MaxPageSize:  cfg.Tools.Web.MaxPageSize,
	})
	if sp := builtin.NewSearchProvider(webClient); sp != nil {
		ag.RegisterTool(builtin.NewWebSearchTool(sp))
	}
	ag.RegisterTool(builtin.NewWebFetchTool(webClient))
}

// stdioRWC wraps stdin/stdout as io.ReadWriteCloser for JSON-RPC.
type stdioRWC struct{}

func (s *stdioRWC) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (s *stdioRWC) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (s *stdioRWC) Close() error                { return nil }
