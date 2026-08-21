package cliapp

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
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/lsp"
	"github.com/jayyao97/zotigo/core/middleware"
	_ "github.com/jayyao97/zotigo/core/providers/anthropic"
	_ "github.com/jayyao97/zotigo/core/providers/deepseek"
	_ "github.com/jayyao97/zotigo/core/providers/gemini"
	_ "github.com/jayyao97/zotigo/core/providers/openai"
	"github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/tools"
	"github.com/jayyao97/zotigo/core/tools/builtin"
	"github.com/jayyao97/zotigo/internal/wiring"
)

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
	resumeAllFlag := fs.Bool("resume-all", false, "Resume a session across all projects")
	bypassPermissions := fs.Bool(
		"dangerously-skip-permissions",
		false,
		"Execute all registered tools without safety checks or approval",
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	doResume := *resumeFlag || *rFlag || *resumeAllFlag
	approvalPolicy := agent.ApprovalPolicyAuto
	if *bypassPermissions {
		approvalPolicy = agent.ApprovalPolicyBypass
	}

	cm := config.NewManager()
	configPath, created, err := cm.EnsureGlobalConfig()
	if err != nil {
		fmt.Printf("Error creating config file: %v\n", err)
		return 1
	}
	if created {
		fmt.Printf("Config file not found. Creating default template at: %s\n", configPath)
		fmt.Println("Config created. Please add a profile, set default_profile, and configure its API key before running again.")
		return 0
	}

	sessMgr, err := session.NewManager()
	if err != nil {
		fmt.Printf("Error initializing session manager: %v\n", err)
		return 1
	}

	cwd, _ := os.Getwd()
	var currentSession *session.Session

	if doResume {
		var sessions []session.Metadata
		var descriptions, disabled map[string]string
		if *resumeAllFlag {
			sessions, descriptions, disabled, err = loadGlobalResumeSessions(sessMgr)
		} else {
			sessions, err = sessMgr.ListByDir(cwd)
		}
		if err != nil {
			fmt.Printf("Error listing sessions: %v\n", err)
			return 1
		}

		selModel := tui.NewSessionSelectionModel(sessions, sessMgr)
		if *resumeAllFlag {
			selModel = tui.NewGlobalSessionSelectionModel(sessions, sessMgr, descriptions, disabled)
		}
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
		cwd = currentSession.WorkingDirectory
	} else {
		currentSession, err = sessMgr.CreateNew(cwd)
		if err != nil {
			fmt.Printf("Error creating session: %v\n", err)
			return 1
		}
	}

	cfg, err := cm.LoadForDir(cwd)
	if err != nil {
		fmt.Println("Error loading config:", err)
		return 1
	}
	profileName, profile, err := cfg.ResolveProfile(currentSession.ProfileName)
	if err != nil {
		fmt.Println("Error resolving profile:", err)
		return 1
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

	home, _ := os.UserHomeDir()
	transcriptDir := filepath.Join(home, ".zotigo", "sessions", "compacted")

	readTracker := tools.NewReadTracker(cwd)

	sm, err := wiring.NewSkillManager(cwd)
	if err != nil {
		fmt.Printf("Warning: failed to load skills: %v\n", err)
	}

	pb := wiring.NewSystemPromptBuilder(wiring.PromptConfig{
		WorkDir:      cwd,
		SkillManager: sm,
	})
	ucb := wiring.NewUserContextBuilder(wiring.PromptConfig{
		WorkDir:                    cwd,
		IncludeProjectInstructions: true,
	})

	// Build observability backend before constructing the agent so it
	// can be wired in as a single option. newObserver returns Noop when
	// langfuse credentials are absent; the agent never sees nil.
	//
	// Each turn gets its own Langfuse session (so cost/usage stay
	// turn-bounded) but the per-turn sessionIds share the zotigo
	// session.ID as a prefix — that keeps every turn of one zotigo
	// invocation grouped in Langfuse's Sessions list under a common
	// search prefix. Static metadata adds cross-startup grouping for
	// --resume runs (where the prefix changes).
	processStart := time.Now()
	staticMeta := map[string]any{
		"zotigo_session": currentSession.ID,
		"process_start":  processStart.Format(time.RFC3339),
		"resumed":        doResume,
	}
	observer := wiring.NewObserver(cfg.Observability, currentSession.ID, staticMeta)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = observer.Close(ctx)
	}()

	ag, err := wiring.NewAgent(wiring.AgentConfig{
		Config:             cfg,
		ProfileName:        profileName,
		Profile:            profile,
		Executor:           exec,
		PromptBuilder:      pb,
		UserContextBuilder: ucb,
		ApprovalPolicy:     approvalPolicy,
		TranscriptDir:      transcriptDir,
		Observer:           observer,
		// ToolSpan goes outermost so it observes every tool call,
		// including ones that ReadTracker short-circuits with a
		// "file changed on disk" rejection — without seeing those,
		// the trace tree skips the rejected call and the next-gen
		// "retry after error" looks unmotivated.
		Middleware: []agent.Middleware{
			middleware.ToolSpan(observer),
			middleware.ReadTracker(readTracker),
		},
		ConfigureClassifier: true,
	})
	if err != nil {
		fmt.Println("Error creating agent:", err)
		return 1
	}
	// Preserve the CLI's construction-time skill semantics: skill files are
	// listed in prompts, but skill dirs are not added to auto-approved read
	// scope. ACP intentionally uses SetSkillManager for the broader scope.
	agent.WithSkillManager(sm)(ag)

	if doResume {
		ag.Restore(currentSession.AgentSnapshot)
	}

	lspManager := lsp.NewManager(cwd)
	defer func() { _ = lspManager.StopAll() }()
	spawnApprovalBroker := tui.NewSpawnApprovalBroker()
	if err := wiring.RegisterDefaultTools(ag, wiring.ToolSetConfig{
		Config:                 cfg,
		Profile:                profile,
		ShellPolicy:            builtin.DefaultShellPolicy(),
		LSPManager:             lspManager,
		Spawn:                  true,
		SpawnApprovalRequester: spawnApprovalBroker,
	}); err != nil {
		fmt.Println("Error registering tools:", err)
		return 1
	}

	cmdRegistry := commands.NewRegistry()
	cmdbuiltin.RegisterAll(cmdRegistry)

	p := tea.NewProgram(
		tui.NewModel(ag, sessMgr, currentSession.ID, cmdRegistry, tui.WithSpawnApprovalBroker(spawnApprovalBroker)),
		tea.WithOutput(&KittyFilterWriter{File: os.Stdout}),
	)
	if _, err := p.Run(); err != nil {
		fmt.Println("Error running program:", err)
		return 1
	}

	return 0
}
