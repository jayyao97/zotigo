package app

import (
	"bytes"
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
	"github.com/jayyao97/zotigo/core/providers"
	_ "github.com/jayyao97/zotigo/core/providers/anthropic"
	_ "github.com/jayyao97/zotigo/core/providers/gemini"
	_ "github.com/jayyao97/zotigo/core/providers/openai"
	"github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/skills"
	"github.com/jayyao97/zotigo/core/tools"
	"github.com/jayyao97/zotigo/core/tools/builtin"
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
	defer sessMgr.Unlock(currentSession.ID)

	localExec, err := executor.NewLocalExecutor(cwd)
	if err != nil {
		fmt.Printf("Error creating executor: %v\n", err)
		return 1
	}
	defer localExec.Close()

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

	ag, err := agent.New(profile, exec,
		agent.WithSystemPromptBuilder(pb),
		agent.WithApprovalPolicy(agent.ApprovalPolicyAuto),
		agent.WithTranscriptDir(transcriptDir),
		agent.WithHook(middleware.ReadTracker(readTracker)),
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
				classifier := agent.NewProviderSafetyClassifier(classifierProvider, profile.Safety.Classifier)
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
	defer lspManager.StopAll()
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

	sm := skills.NewSkillManager(cwd)
	if err := sm.Load(); err != nil {
		fmt.Printf("Warning: failed to load skills: %v\n", err)
	}
	ag.SetSkillManager(sm)

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
