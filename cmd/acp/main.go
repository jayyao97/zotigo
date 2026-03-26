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
		runner   *runner.Runner
		session  *acp.Session
		cancelFn context.CancelFunc
	}
	sessions := make(map[string]*sessionState)

	// stdio as ReadWriteCloser
	rwc := &stdioRWC{}

	var acpServer *acp.Server

	acpServer = acp.NewServer(rwc,
		acp.OnInitialized(func(caps acp.ClientCapabilities) {
			log.Printf("Client connected, fs=%v, terminal=%v",
				caps.Filesystem != nil, caps.Terminal != nil)
		}),

		acp.OnSessionNew(func(ctx context.Context, params acp.SessionNewParams) (string, error) {
			workDir := params.WorkingDirectory
			if workDir == "" {
				workDir, _ = os.Getwd()
			}

			sessionID := fmt.Sprintf("acp-%d", time.Now().UnixNano())

			// Create remote executor that delegates to the editor
			exec := acp.NewRemoteExecutor(acpServer, workDir)

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

			sessions[sessionID] = &sessionState{
				runner:  r,
				session: sess,
			}

			log.Printf("Session created: %s (workDir=%s)", sessionID, workDir)
			return sessionID, nil
		}),

		acp.OnSessionPrompt(func(ctx context.Context, sessionID string, text string, images []acp.ContentBlock) error {
			state, ok := sessions[sessionID]
			if !ok {
				return fmt.Errorf("session %q not found", sessionID)
			}

			// Cancel previous processing if any
			if state.cancelFn != nil {
				state.cancelFn()
			}
			promptCtx, cancel := context.WithCancel(ctx)
			state.cancelFn = cancel
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
			// transport automatically — no manual channel relay needed.
			if err := state.runner.RunFullTurn(promptCtx, msg); err != nil {
				log.Printf("RunFullTurn error: %v", err)
			}

			cancel()
			return nil
		}),

		acp.OnSessionCancel(func(_ context.Context, sessionID string) {
			if state, ok := sessions[sessionID]; ok {
				state.session.Cancel()
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
	ag.RegisterTool(&builtin.ListDirTool{})
	ag.RegisterTool(&builtin.EditTool{})
	ag.RegisterTool(&builtin.PatchTool{})

	ag.RegisterTool(&builtin.ShellTool{})
	ag.RegisterTool(&builtin.GrepTool{})
	ag.RegisterTool(&builtin.GlobTool{})

	ag.RegisterTool(&builtin.GitStatusTool{})
	ag.RegisterTool(&builtin.GitDiffTool{})
	ag.RegisterTool(&builtin.GitCommitTool{})
	ag.RegisterTool(&builtin.GitAddTool{})

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
