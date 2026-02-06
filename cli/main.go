package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"regexp"

	tea "charm.land/bubbletea/v2"
	"github.com/jayyao97/zotigo/cli/tui"
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/lsp"
	_ "github.com/jayyao97/zotigo/core/providers/anthropic"
	_ "github.com/jayyao97/zotigo/core/providers/openai"
	"github.com/jayyao97/zotigo/core/sandbox"
	"github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/tools/builtin"
)

// KittyFilterWriter wraps *os.File to filter out unsupported Kitty keyboard protocol sequences.
type KittyFilterWriter struct {
	*os.File
}

// Regex to match Kitty protocol response sequences:
// \x1b[=...u - mode setting responses (e.g., =1;1u, =0u)
// \x1b[?...u - query responses
var kittyResponseRegex = regexp.MustCompile(`\x1b\[[=?][0-9;]*u`)

func (k *KittyFilterWriter) Write(p []byte) (n int, err error) {
	if !bytes.Contains(p, []byte("\x1b")) {
		return k.File.Write(p)
	}
	// Filter Kitty protocol response sequences that cause artifacts in JetBrains terminal
	data := kittyResponseRegex.ReplaceAll(p, nil)
	_, err = k.File.Write(data)
	return len(p), err
}

func main() {
	resumeFlag := flag.Bool("resume", false, "Resume a previous session")
	rFlag := flag.Bool("r", false, "Resume a previous session (shorthand)")
	flag.Parse()
	doResume := *resumeFlag || *rFlag

	// 1. Config Manager
	cm := config.NewManager()
	configPath, err := cm.GetConfigPath()
	if err == nil {
		if _, err := os.Stat(configPath); os.IsNotExist(err) {
			fmt.Printf("⚠️  Config file not found. Creating default template at: %s\n", configPath)
			if err := cm.Save(config.DefaultConfig()); err != nil {
				fmt.Printf("Error creating config file: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("✅ Config created. Please edit the file to set your API_KEY before running again.")
			os.Exit(0)
		}
	}

	cfg, err := cm.Load()
	if err != nil {
		fmt.Println("Error loading config:", err)
		os.Exit(1)
	}

	profileName := cfg.DefaultProfile
	profile, ok := cfg.Profiles[profileName]
	if !ok {
		fmt.Printf("Profile '%s' not found in config.\n", profileName)
		os.Exit(1)
	}

	// 2. Session Manager
	sessMgr, err := session.NewManager()
	if err != nil {
		fmt.Printf("Error initializing session manager: %v\n", err)
		os.Exit(1)
	}

	cwd, _ := os.Getwd()
	var currentSession *session.Session

	if doResume {
		sessions, err := sessMgr.ListByDir(cwd)
		if err != nil {
			fmt.Printf("Error listing sessions: %v\n", err)
			os.Exit(1)
		}

		// Run Selection TUI
		selModel := tui.NewSessionSelectionModel(sessions, sessMgr)
		p := tea.NewProgram(selModel)
		m, err := p.Run()
		if err != nil {
			fmt.Println("Error running selection:", err)
			os.Exit(1)
		}

		finalModel := m.(tui.SessionSelectionModel)
		if finalModel.ChosenID == "" {
			fmt.Println("No session selected. Exiting.")
			os.Exit(0)
		}

		currentSession, err = sessMgr.Load(finalModel.ChosenID)
		if err != nil {
			fmt.Printf("Error loading session: %v\n", err)
			os.Exit(1)
		}
	} else {
		// New Session
		currentSession, err = sessMgr.CreateNew(cwd)
		if err != nil {
			fmt.Printf("Error creating session: %v\n", err)
			os.Exit(1)
		}
	}

	// 3. Lock Session
	if err := sessMgr.Lock(currentSession.ID); err != nil {
		fmt.Printf("Error locking session: %v\n", err)
		os.Exit(1)
	}

	// Ensure unlock on exit
	defer sessMgr.Unlock(currentSession.ID)

	// 4. Init Executor with Sandbox Guard
	localExec, err := executor.NewLocalExecutor(cwd)
	if err != nil {
		fmt.Printf("Error creating executor: %v\n", err)
		os.Exit(1)
	}
	defer localExec.Close()

	// Wrap executor with security guard
	exec, err := sandbox.NewGuard(localExec, nil) // nil = use default policy
	if err != nil {
		fmt.Printf("Error creating security guard: %v\n", err)
		os.Exit(1)
	}

	// 5. Init Agent
	ag, err := agent.New(profile, exec, "You are Zotigo, a helpful CLI software engineering agent.")
	if err != nil {
		fmt.Println("Error creating agent:", err)
		os.Exit(1)
	}
	ag.SetApprovalPolicy(agent.ApprovalPolicyManual)

	// Restore state if needed
	if doResume {
		ag.Restore(currentSession.AgentSnapshot)
	}

	// 6. Register Tools
	// Filesystem tools
	ag.RegisterTool(&builtin.ReadFileTool{})
	ag.RegisterTool(&builtin.WriteFileTool{})
	ag.RegisterTool(&builtin.ListDirTool{})
	ag.RegisterTool(&builtin.EditTool{})
	ag.RegisterTool(&builtin.PatchTool{})

	// Search tools
	ag.RegisterTool(&builtin.ShellTool{})
	ag.RegisterTool(&builtin.GrepTool{})
	ag.RegisterTool(&builtin.GlobTool{})

	// Git tools
	ag.RegisterTool(&builtin.GitStatusTool{})
	ag.RegisterTool(&builtin.GitDiffTool{})
	ag.RegisterTool(&builtin.GitCommitTool{})
	ag.RegisterTool(&builtin.GitAddTool{})

	// LSP tools
	lspManager := lsp.NewManager(cwd)
	defer lspManager.StopAll()
	ag.RegisterTool(builtin.NewLSPTool(lspManager))

	// 7. Run Main TUI
	p := tea.NewProgram(
		tui.NewModel(ag, sessMgr, currentSession.ID),
		tea.WithOutput(&KittyFilterWriter{File: os.Stdout}),
	)
	if _, err := p.Run(); err != nil {
		fmt.Println("Error running program:", err)
		os.Exit(1)
	}
}

func listProfiles(profiles map[string]config.ProfileConfig) string {
	var names string
	for k := range profiles {
		names += k + ", "
	}
	if len(names) > 2 {
		return names[:len(names)-2]
	}
	return ""
}
