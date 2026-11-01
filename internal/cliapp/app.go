package cliapp

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/jayyao97/zotigo/cli/commands"
	cmdbuiltin "github.com/jayyao97/zotigo/cli/commands/builtin"
	"github.com/jayyao97/zotigo/cli/tui"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/internal/buildinfo"
	"github.com/jayyao97/zotigo/internal/diagnostics"
	"github.com/jayyao97/zotigo/internal/zotigod"
	"github.com/jayyao97/zotigo/internal/zotigodclient"
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

const historyPageSize = 100

// Run starts the interactive CLI and returns a process exit code.
//
// The TUI is a client of the zotigod session service. By default it uses
// the shared local daemon (starting it when needed), so sessions keep
// running and stay visible to Desktop, ACP and other terminals. With
// --no-daemon the same service runs inside this process and its sessions
// stop when the TUI exits.
func Run(args []string) (exitCode int) {
	if len(args) > 0 && args[0] == zotigod.WorkerSubcommand {
		// Session worker launched by an embedded (--no-daemon) service.
		return zotigod.Run(args[1:])
	}
	fs := flag.NewFlagSet("zotigo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	versionFlag := fs.Bool("version", false, "Print program release version")

	resumeFlag := fs.Bool("resume", false, "Resume a previous session in this directory")
	rFlag := fs.Bool("r", false, "Resume a previous session (shorthand)")
	resumeAllFlag := fs.Bool("resume-all", false, "Resume a session across all directories and workspaces")
	noDaemon := fs.Bool("no-daemon", false, "Run the session service inside this process (requires --state-dir); sessions stop when zotigo exits")
	stateDir := fs.String("state-dir", "", "Isolated state directory for --no-daemon (sessions, catalog, logs); must not be ~/.zotigo")
	workspaceID := fs.String("workspace", "", "Bind the new session to this Zotigo Workspace (the current directory must be inside it)")
	bypassPermissions := fs.Bool(
		"dangerously-skip-permissions",
		false,
		"Execute all registered tools without safety checks or approval (Full access)",
	)
	if err := fs.Parse(args); err != nil {
		stopLogging := diagnostics.Start("cli")
		log.Printf("argument_error: %v", err)
		stopLogging()
		return 2
	}
	if *versionFlag {
		fmt.Printf("zotigo %s\n", buildinfo.Version)
		return 0
	}
	if *noDaemon {
		// Export the isolated state root before anything writes state,
		// including this process's diagnostics log.
		if _, err := zotigodclient.UseIsolatedStateDir(*stateDir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	stopLogging := diagnostics.Start("cli")
	defer stopLogging()
	log.Printf("cli_start version=%s pid=%d no_daemon=%v", buildinfo.Version, os.Getpid(), *noDaemon)
	defer func() {
		if value := recover(); value != nil {
			log.Printf("cli_panic: %v", value)
			panic(value)
		}
		log.Printf("cli_exit code=%d", exitCode)
	}()
	fail := func(format string, args ...any) int {
		message := fmt.Sprintf(format, args...)
		log.Print(message)
		fmt.Fprintln(os.Stderr, message)
		return 1
	}

	doResume := *resumeFlag || *rFlag || *resumeAllFlag
	if doResume && *workspaceID != "" {
		return fail("--workspace applies to new sessions; a resumed session keeps its ownership")
	}

	configPath, created, err := config.NewManager().EnsureGlobalConfig()
	if err != nil {
		return fail("Error creating config file: %v", err)
	}
	if created {
		fmt.Printf("Config file not found. Creating default template at: %s\n", configPath)
		fmt.Println("Config created. Please add a profile, set default_profile, and configure its API key before running again.")
		return 0
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fail("Error resolving current directory: %v", err)
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return fail("Error resolving current directory: %v", err)
	}

	ctx := context.Background()
	conn, err := zotigodclient.Connect(ctx, zotigodclient.ConnectOptions{
		NoDaemon: *noDaemon,
		StateDir: *stateDir,
		Logger:   log.New(log.Writer(), "[zotigod-embedded] ", log.LstdFlags),
	})
	if err != nil {
		return fail("Error connecting to the session service: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := conn.Close(shutdownCtx); err != nil {
			log.Printf("cli_service_close_error: %v", err)
		}
	}()
	log.Printf("cli_service url=%s embedded=%v", conn.BaseURL(), conn.Embedded)

	policy := ""
	if *bypassPermissions {
		policy = "bypass_permissions"
	}

	var current zotigodclient.Session
	if doResume {
		chosen, err := chooseSession(ctx, conn.Client, cwd, *resumeAllFlag)
		if err != nil {
			return fail("Error listing sessions: %v", err)
		}
		if chosen == "" {
			fmt.Println("No session selected. Exiting.")
			return 0
		}
		if current, err = conn.GetSession(ctx, chosen); err != nil {
			return fail("Error loading session: %v", err)
		}
		if policy != "" && current.ApprovalPolicy != policy {
			if err := conn.SetApprovalPolicy(ctx, current.ID, policy); err != nil {
				return fail("Error enabling Full access: %v", err)
			}
			if current, err = conn.GetSession(ctx, chosen); err != nil {
				return fail("Error loading session: %v", err)
			}
		}
	} else {
		current, err = conn.CreateSession(ctx, zotigodclient.CreateSessionRequest{
			WorkingDirectory: cwd,
			WorkspaceID:      *workspaceID,
			ApprovalPolicy:   policy,
		})
		if err != nil {
			return fail("Error creating session: %v", err)
		}
	}
	log.Printf("cli_session session=%s profile=%s cwd=%s", current.ID, current.Profile, current.WorkingDirectory)

	page, err := conn.Items(ctx, current.ID, 0, 0, historyPageSize)
	if err != nil {
		return fail("Error loading session history: %v", err)
	}
	var cursor uint64
	if len(page.Items) > 0 {
		cursor = page.Items[len(page.Items)-1].Sequence
	}

	cmdRegistry := commands.NewRegistry()
	cmdbuiltin.RegisterAll(cmdRegistry)
	model := tui.NewModel(tui.ModelOptions{
		Service:          conn.Client,
		Session:          current,
		Registry:         cmdRegistry,
		History:          page.Items,
		HistoryTruncated: page.PrevCursor != "",
		Cursor:           cursor,
		Location:         describeLocation(ctx, conn.Client, current.ID),
		Embedded:         conn.Embedded,
	})
	defer model.Close()

	p := tea.NewProgram(model, tea.WithOutput(&KittyFilterWriter{File: os.Stdout}))
	if _, err := p.Run(); err != nil {
		return fail("Error running program: %v", err)
	}
	if conn.Embedded {
		stopEmbeddedTurn(conn.Client, model.SessionID(), model.OpenTurnID())
	}
	return 0
}

// stopEmbeddedTurn interrupts a turn that would otherwise be killed with the
// embedded service, giving the worker a chance to record the interruption
// and save its snapshot before shutdown.
func stopEmbeddedTurn(client *zotigodclient.Client, sessionID string, turnID string) {
	if turnID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Pause(ctx, sessionID, turnID); err != nil {
		log.Printf("cli_embedded_pause_error session=%s err=%v", sessionID, err)
		return
	}
	for ctx.Err() == nil {
		session, err := client.GetSession(ctx, sessionID)
		if err != nil || !session.Working {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// describeLocation labels session ownership for the banner. A failure to
// read the catalog only loses the label, never the session.
func describeLocation(ctx context.Context, client *zotigodclient.Client, sessionID string) string {
	projection, err := client.CatalogSession(ctx, sessionID)
	if err != nil {
		log.Printf("cli_location_error session=%s err=%v", sessionID, err)
		return ""
	}
	workspaceID := projection.WorkspaceID()
	if workspaceID == "" {
		return "Directory session (no Workspace)"
	}
	workspace, err := client.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return "Workspace " + workspaceID
	}
	label := "Workspace " + workspace.Title
	if project, err := client.GetProject(ctx, workspace.ProjectID); err == nil {
		label = project.Name + " / " + workspace.Title
	}
	return label
}

func chooseSession(ctx context.Context, client *zotigodclient.Client, cwd string, all bool) (string, error) {
	entries, err := loadResumeSessions(ctx, client, cwd, all)
	if err != nil {
		return "", err
	}
	archive := func(id string) error {
		archiveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return client.ArchiveSession(archiveCtx, id)
	}
	selection := tui.NewSessionSelectionModel(entries.sessions, archive, entries.descriptions, entries.disabled)
	finalModel, err := tea.NewProgram(selection).Run()
	if err != nil {
		return "", fmt.Errorf("run selection: %w", err)
	}
	chosen, ok := finalModel.(tui.SessionSelectionModel)
	if !ok {
		return "", errors.New("unexpected selection model")
	}
	return chosen.ChosenID, nil
}
