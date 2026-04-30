package app

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/jayyao97/zotigo/cli/commands"
	cmdbuiltin "github.com/jayyao97/zotigo/cli/commands/builtin"
	"github.com/jayyao97/zotigo/cli/tui"
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/agent/prompt"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/lsp"
	"github.com/jayyao97/zotigo/core/middleware"
	"github.com/jayyao97/zotigo/core/observability"
	"github.com/jayyao97/zotigo/core/observability/langfuse"
	"github.com/jayyao97/zotigo/core/providers"
	_ "github.com/jayyao97/zotigo/core/providers/anthropic"
	_ "github.com/jayyao97/zotigo/core/providers/gemini"
	_ "github.com/jayyao97/zotigo/core/providers/openai"
	"github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/skills"
	"github.com/jayyao97/zotigo/core/tools"
	"github.com/jayyao97/zotigo/core/tools/builtin"
)

// buildObserver wires up an observability.Observer from config.
// Returns Noop when no backend is enabled — callers don't have to
// special-case the disabled path. sessionID groups all traces this
// run produces under one Langfuse session.
func buildObserver(cfg config.ObservabilityConfig, sessionID string) observability.Observer {
	if cfg.Langfuse.IsEnabled() {
		return langfuse.New(langfuse.Config{
			Host:          cfg.Langfuse.Host,
			PublicKey:     cfg.Langfuse.PublicKey,
			SecretKey:     cfg.Langfuse.SecretKey,
			FlushInterval: time.Duration(cfg.Langfuse.FlushInterval) * time.Second,
			SessionID:     sessionID,
		})
	}
	return observability.Noop{}
}

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// KittyFilterWriter filters unsupported Kitty keyboard protocol responses.
type KittyFilterWriter struct {
	*os.File
}

var kittyResponseRegex = regexp.MustCompile(`\x1b\[[=?][0-9;]*u`)

func (k *KittyFilterWriter) Write(p []byte) (n int, err error) {
	if !bytes.Contains(p, []byte("\x1b")) {
		return k.File.Write(p)
	}

	data := kittyResponseRegex.ReplaceAll(p, nil)
	_, err = k.File.Write(data)
	return len(p), err
}

// Run starts the interactive CLI and returns a process exit code.
func Run(args []string) int {
	fs := flag.NewFlagSet("zotigo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	resumeFlag := fs.Bool("resume", false, "Resume a previous session")
	rFlag := fs.Bool("r", false, "Resume a previous session (shorthand)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	doResume := *resumeFlag || *rFlag

	cm := config.NewManager()
	configPath, err := cm.GetConfigPath()
	if err == nil {
		if _, err := os.Stat(configPath); os.IsNotExist(err) {
			fmt.Printf("Config file not found. Creating default template at: %s\n", configPath)
			if err := cm.Save(config.DefaultConfig()); err != nil {
				fmt.Printf("Error creating config file: %v\n", err)
				return 1
			}
			fmt.Println("Config created. Please edit the file to set your API key before running again.")
			return 0
		}
	}

	cfg, err := cm.Load()
	if err != nil {
		fmt.Println("Error loading config:", err)
		return 1
	}

	profileName := cfg.DefaultProfile
	profile, ok := cfg.Profiles[profileName]
	if !ok {
		fmt.Printf("Profile '%s' not found in config.\n", profileName)
		return 1
	}

	sessMgr, err := session.NewManager()
	if err != nil {
		fmt.Printf("Error initializing session manager: %v\n", err)
		return 1
	}

	cwd, _ := os.Getwd()
	var currentSession *session.Session

	if doResume {
		sessions, err := sessMgr.ListByDir(cwd)
		if err != nil {
			fmt.Printf("Error listing sessions: %v\n", err)
			return 1
		}

		selModel := tui.NewSessionSelectionModel(sessions, sessMgr)
		p := tea.NewProgram(selModel)
		m, err := p.Run()
		if err != nil {
			fmt.Println("Error running selection:", err)
			return 1
		}

		finalModel := m.(tui.SessionSelectionModel)
		if finalModel.ChosenID == "" {
			fmt.Println("No session selected. Exiting.")
			return 0
		}

		currentSession, err = sessMgr.Load(finalModel.ChosenID)
		if err != nil {
			fmt.Printf("Error loading session: %v\n", err)
			return 1
		}
	} else {
		currentSession, err = sessMgr.CreateNew(cwd)
		if err != nil {
			fmt.Printf("Error creating session: %v\n", err)
			return 1
		}
	}

	if err := sessMgr.Lock(currentSession.ID); err != nil {
		fmt.Printf("Error locking session: %v\n", err)
		return 1
	}
	defer func() { _ = sessMgr.Unlock(currentSession.ID) }()

	localExec, err := executor.NewLocalExecutor(cwd)
	if err != nil {
		fmt.Printf("Error creating executor: %v\n", err)
		return 1
	}
	defer func() { _ = localExec.Close() }()

	// Executor is used raw now — command-safety is enforced by ShellTool's
	// ShellPolicy in Classify; file-path safety by tools' in-workdir checks.
	exec := localExec

	pbOpts := []prompt.SystemPromptOption{
		prompt.WithDynamicSection("environment", func(ctx prompt.PromptContext) string {
			return fmt.Sprintf("Working directory: %s\nPlatform: %s",
				ctx.WorkDir, ctx.Platform)
		}),
	}

	if data, err := os.ReadFile(filepath.Join(cwd, "AGENTS.md")); err == nil {
		content := string(data)
		pbOpts = append(pbOpts, prompt.WithDynamicSection("project_instructions", func(_ prompt.PromptContext) string {
			return content
		}))
	}

	pb := prompt.NewSystemPromptBuilder(pbOpts...)

	home, _ := os.UserHomeDir()
	transcriptDir := filepath.Join(home, ".zotigo", "sessions", "compacted")

	readTracker := tools.NewReadTracker(cwd)

	sm := skills.NewSkillManager(cwd)
	if err := sm.Load(); err != nil {
		fmt.Printf("Warning: failed to load skills: %v\n", err)
	}

	// Fossilize the current skill listing into each user message as a
	// <system-reminder>. Each message is byte-identical once stored, so
	// Anthropic's ephemeral prompt cache keeps hitting on the history
	// prefix; small open models (Qwen, Llama) see the listing because it
	// rides on the user message rather than a third system block.
	uw := prompt.NewUserPromptWrapper(
		prompt.WithContext("system-reminder", func(_ prompt.PromptContext) string {
			_ = sm.Load() // re-scan disk so new skill files are picked up
			return sm.BuildSkillIndex()
		}),
	)

	// Build observability backend before constructing the agent so it
	// can be wired in as a single option. NewObserver returns Noop
	// when langfuse credentials are absent; the agent never sees nil.
	// session ID flows in so all traces of this run share a Langfuse
	// session.
	observer := buildObserver(cfg.Observability, currentSession.ID)
	defer func() {
		ctx, cancel := contextWithTimeout(2 * time.Second)
		defer cancel()
		_ = observer.Close(ctx)
	}()

	ag, err := agent.New(profile, exec,
		agent.WithSystemPromptBuilder(pb),
		agent.WithUserPromptWrapper(uw),
		agent.WithSkillManager(sm),
		agent.WithApprovalPolicy(agent.ApprovalPolicyAuto),
		agent.WithTranscriptDir(transcriptDir),
		agent.WithObserver(observer),
		// ToolSpan goes outermost so it observes every tool call,
		// including ones that ReadTracker short-circuits with a
		// "file changed on disk" rejection — without seeing those,
		// the trace tree skips the rejected call and the next-gen
		// "retry after error" looks unmotivated.
		agent.WithMiddleware(middleware.ToolSpan(observer)),
		agent.WithMiddleware(middleware.ReadTracker(readTracker)),
	)
	if err != nil {
		fmt.Println("Error creating agent:", err)
		return 1
	}

	if doResume {
		ag.Restore(currentSession.AgentSnapshot)
	}

	if profile.Safety.Classifier.IsEnabled() {
		classifierProfileName, classifierProfile, err := cfg.ResolveClassifierProfile(profileName)
		if err != nil {
			ag.SetApprovalPolicy(agent.ApprovalPolicyManual)
			agent.WithClassifierUnavailableReason(err.Error())(ag)
		} else {
			agent.WithClassifierProfile(classifierProfileName, classifierProfile)(ag)
			if classifierProvider, err := providers.NewProvider(classifierProfile); err != nil {
				agent.WithClassifierUnavailableReason(
					fmt.Sprintf("failed to create classifier provider %q: %v", classifierProfileName, err),
				)(ag)
			} else {
				classifier := agent.NewProviderSafetyClassifier(
					classifierProvider,
					profile.Safety.Classifier,
					agent.WithClassifierObserver(observer, classifierProfile.Model),
				)
				agent.WithSafetyClassifier(classifier)(ag)
			}
		}
	}

	ag.RegisterTool(&builtin.ReadFileTool{})
	ag.RegisterTool(&builtin.WriteFileTool{})
	ag.RegisterTool(&builtin.EditTool{})

	shellTool, err := builtin.NewShellTool(builtin.WithPolicy(builtin.DefaultShellPolicy()))
	if err != nil {
		fmt.Println("Error creating shell tool:", err)
		return 1
	}
	ag.RegisterTool(shellTool)
	ag.RegisterTool(&builtin.GrepTool{})
	ag.RegisterTool(&builtin.GlobTool{})

	lspManager := lsp.NewManager(cwd)
	defer func() { _ = lspManager.StopAll() }()
	ag.RegisterTool(builtin.NewLSPTool(lspManager))

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

	cmdRegistry := commands.NewRegistry()
	cmdbuiltin.RegisterAll(cmdRegistry)

	p := tea.NewProgram(
		tui.NewModel(ag, sessMgr, currentSession.ID, cmdRegistry),
		tea.WithOutput(&KittyFilterWriter{File: os.Stdout}),
	)
	if _, err := p.Run(); err != nil {
		fmt.Println("Error running program:", err)
		return 1
	}

	return 0
}
