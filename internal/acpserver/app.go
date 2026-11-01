// Package acpserver adapts the Agent Client Protocol to the zotigod session
// service. Editors speak ACP over stdio; sessions, tool execution, approvals
// and persistence belong to the service, exactly as for the TUI and Desktop.
//
// Mapping:
//   - ACP sessionId is the zotigod session ID, so session/load can reattach
//     to any zotigo session whose cwd matches.
//   - session/new with only a cwd creates a directory session; the service
//     binds it to the Workspace containing that cwd, if any. Starting with
//     --workspace binds every new session to that Workspace and requires the
//     cwd to be inside it.
//   - session/prompt submits a message (start-or-steer) and waits for the
//     turn that consumes it; session/cancel pauses that turn.
//   - Approval requests become session/request_permission; the first durable
//     decision wins across all clients.
//
// Tools run in the session runtime on the service host, not through the
// client's fs/* and terminal/* methods, so they work with clients that do
// not implement those methods. core/acp.RemoteExecutor remains available to
// library users who embed an agent directly.
package acpserver

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/jayyao97/zotigo/core/acp"
	"github.com/jayyao97/zotigo/internal/zotigod"
	"github.com/jayyao97/zotigo/internal/zotigodclient"
)

// Run starts the ACP server and returns a process exit code.
func Run(args []string) int {
	if len(args) > 0 && args[0] == zotigod.WorkerSubcommand {
		// Session worker launched by an embedded (--no-daemon) service.
		return zotigod.Run(args[1:])
	}
	log.SetOutput(os.Stderr)
	log.SetPrefix("[zotigo-acp] ")
	log.SetFlags(log.LstdFlags)
	logger := log.New(os.Stderr, "[zotigo-acp] ", log.LstdFlags)

	fs := flag.NewFlagSet("zotigo-acp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	noDaemon := fs.Bool("no-daemon", false, "Run the session service inside this process (requires --state-dir); sessions stop when the editor disconnects")
	stateDir := fs.String("state-dir", "", "Isolated state directory for --no-daemon; must not be ~/.zotigo")
	workspaceID := fs.String("workspace", "", "Bind new sessions to this Zotigo Workspace; session cwd must be inside it")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	conn, err := zotigodclient.Connect(ctx, zotigodclient.ConnectOptions{
		NoDaemon: *noDaemon,
		StateDir: *stateDir,
		Logger:   log.New(os.Stderr, "[zotigod-embedded] ", log.LstdFlags),
	})
	if err != nil {
		logger.Printf("Failed to connect to the session service: %v", err)
		return 1
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := conn.Close(closeCtx); err != nil {
			logger.Printf("Closing session service: %v", err)
		}
	}()

	adapter, err := newAdapter(ctx, conn.Client, conn.Embedded, *workspaceID, logger)
	if err != nil {
		logger.Printf("%v", err)
		return 1
	}
	server := adapter.newServer(&stdioRWC{r: os.Stdin, w: os.Stdout})

	logger.Printf("Starting ACP server on stdio (service=%s embedded=%v workspace=%q)", conn.BaseURL(), conn.Embedded, *workspaceID)
	runErr := server.Run(ctx)
	adapter.shutdown()
	if runErr != nil && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, io.EOF) {
		logger.Printf("ACP server error: %v", runErr)
		return 1
	}
	return 0
}

// newAdapter validates the optional Workspace binding up front so a
// misconfigured editor fails at startup instead of at the first session.
func newAdapter(ctx context.Context, client *zotigodclient.Client, embedded bool, workspaceID string, logger *log.Logger) (*adapter, error) {
	a := &adapter{
		client:   client,
		embedded: embedded,
		logger:   logger,
		sessions: make(map[string]*acpSession),
	}
	if workspaceID != "" {
		workspace, err := client.GetWorkspace(ctx, workspaceID)
		if err != nil {
			return nil, fmt.Errorf("--workspace %s: %w", workspaceID, err)
		}
		if workspace.Status != "ready" {
			return nil, fmt.Errorf("--workspace %s is %s, not ready", workspaceID, workspace.Status)
		}
		a.workspace = &workspace
	}
	return a, nil
}

func (a *adapter) newServer(rwc io.ReadWriteCloser) *acp.Server {
	a.server = acp.NewServer(rwc,
		acp.OnInitialized(func(caps acp.ClientCapabilities) {
			a.logger.Printf("Client connected, fs.read=%v, fs.write=%v, terminal=%v (tools run in the session runtime)",
				caps.FS.ReadTextFile, caps.FS.WriteTextFile, caps.Terminal)
		}),
		acp.OnSessionNew(a.onSessionNew),
		acp.OnSessionLoad(a.onSessionLoad),
		acp.OnSessionList(a.onSessionList),
		acp.OnSessionPrompt(a.onSessionPrompt),
		acp.OnSessionCancel(a.onSessionCancel),
		acp.OnSessionModes(a.sessionModes, a.onSetMode),
	)
	return a.server
}

// validateCwd enforces an absolute, existing cwd. ACP clients must send
// one; the adapter never substitutes its own directory.
func validateCwd(cwd string) (string, error) {
	if cwd == "" {
		return "", errors.New("cwd is required")
	}
	if !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("cwd must be absolute: %q", cwd)
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return "", fmt.Errorf("cwd: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("cwd is not a directory: %q", cwd)
	}
	return filepath.Clean(cwd), nil
}

type stdioRWC struct {
	r io.Reader
	w io.Writer
}

func (s *stdioRWC) Read(p []byte) (int, error)  { return s.r.Read(p) }
func (s *stdioRWC) Write(p []byte) (int, error) { return s.w.Write(p) }
func (s *stdioRWC) Close() error                { return nil }
