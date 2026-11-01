package zotigodclient

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/internal/zotigod"
)

const (
	defaultDaemonURL   = "http://127.0.0.1:8766"
	apiProtocolVersion = "1"
	daemonStartTimeout = 10 * time.Second
)

// Connection is a client bound to the session service that owns its
// sessions for the lifetime of the host process.
type Connection struct {
	*Client
	// Embedded is true when the service runs inside this process
	// (--no-daemon). Work started through it stops when Close returns.
	Embedded bool
	embedded *zotigod.EmbeddedService
}

// Close releases the connection. For an embedded service this stops the
// service and its workers; for the shared daemon it only drops the client.
func (c *Connection) Close(ctx context.Context) error {
	if c == nil || c.embedded == nil {
		return nil
	}
	return c.embedded.Close(ctx)
}

// ConnectOptions selects where sessions run.
type ConnectOptions struct {
	// NoDaemon runs the session service inside this process.
	NoDaemon bool
	// StateDir is the isolated state root an embedded service must use.
	// It is required with NoDaemon and must not be the daemon's ~/.zotigo,
	// so an in-process service never shares sessions or the Workspace
	// catalog with zotigod. Configuration is still read from ~/.zotigo.
	StateDir string
	// Logger receives embedded service and worker output.
	Logger *log.Logger
}

// Connect returns a connection to the shared daemon, starting a local daemon
// when the configured loopback endpoint is not running, or to an embedded
// service when NoDaemon is set. It never falls back from one to the other:
// a daemon that cannot be reached or started is an error, so execution
// ownership does not change silently.
func Connect(ctx context.Context, opts ConnectOptions) (*Connection, error) {
	if opts.NoDaemon {
		if _, err := UseIsolatedStateDir(opts.StateDir); err != nil {
			return nil, err
		}
		service, err := zotigod.StartEmbedded(opts.Logger)
		if err != nil {
			return nil, fmt.Errorf("start in-process session service: %w", err)
		}
		return &Connection{Client: New(service.BaseURL, service.Token), Embedded: true, embedded: service}, nil
	}
	if strings.TrimSpace(opts.StateDir) != "" {
		return nil, errors.New("--state-dir only applies with --no-daemon; the daemon always uses ~/.zotigo")
	}
	baseURL, token, err := daemonEndpoint()
	if err != nil {
		return nil, err
	}
	client := New(baseURL, token)
	if err := ensureDaemon(ctx, client); err != nil {
		return nil, fmt.Errorf("%w (use --no-daemon to run sessions inside this process)", err)
	}
	return &Connection{Client: client}, nil
}

// UseIsolatedStateDir validates dir as the state root of an in-process
// service and exports it (config.StateDirEnv) so this process and the
// session workers it launches use it. It returns the absolute directory.
// The default ~/.zotigo belongs to zotigod and is rejected.
func UseIsolatedStateDir(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", errors.New("--no-daemon requires --state-dir <dir>: an in-process service keeps its sessions apart from zotigod; to use your existing sessions, run without --no-daemon")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve --state-dir: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", fmt.Errorf("create --state-dir: %w", err)
	}
	defaultDir, err := config.DefaultStateDir()
	if err != nil {
		return "", err
	}
	if sameDirectory(abs, defaultDir) {
		return "", fmt.Errorf("--state-dir must not be %s, which belongs to zotigod; run without --no-daemon to use it", defaultDir)
	}
	if err := os.Setenv(config.StateDirEnv, abs); err != nil {
		return "", err
	}
	return abs, nil
}

func sameDirectory(left, right string) bool {
	if filepath.Clean(left) == filepath.Clean(right) {
		return true
	}
	resolvedLeft, leftErr := filepath.EvalSymlinks(left)
	resolvedRight, rightErr := filepath.EvalSymlinks(right)
	return leftErr == nil && rightErr == nil && resolvedLeft == resolvedRight
}

// daemonEndpoint follows the same environment contract as Desktop:
// ZOTIGOD_URL selects the endpoint and ZOTIGOD_AUTH_TOKEN_FILE its token.
func daemonEndpoint() (string, string, error) {
	baseURL := strings.TrimSpace(os.Getenv("ZOTIGOD_URL"))
	if baseURL == "" {
		baseURL = defaultDaemonURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", "", fmt.Errorf("invalid ZOTIGOD_URL %q", baseURL)
	}
	token := ""
	if path := strings.TrimSpace(os.Getenv("ZOTIGOD_AUTH_TOKEN_FILE")); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", "", fmt.Errorf("read ZOTIGOD_AUTH_TOKEN_FILE: %w", err)
		}
		token = strings.TrimSpace(string(data))
		if token == "" {
			return "", "", errors.New("ZOTIGOD_AUTH_TOKEN_FILE is empty")
		}
	}
	return strings.TrimRight(baseURL, "/"), token, nil
}

func checkHealth(ctx context.Context, client *Client) error {
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	health, err := client.Health(probe)
	if err != nil {
		return err
	}
	if health.Status != "ok" || health.ProtocolVersion != apiProtocolVersion {
		return fmt.Errorf("service at %s is not a compatible zotigod (status=%q protocol=%q)", client.BaseURL(), health.Status, health.ProtocolVersion)
	}
	return nil
}

func ensureDaemon(ctx context.Context, client *Client) error {
	err := checkHealth(ctx, client)
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) || !isConnectionRefused(err) {
		return fmt.Errorf("zotigod at %s is unavailable: %w", client.BaseURL(), err)
	}
	parsed, _ := url.Parse(client.BaseURL())
	if !isLoopbackHost(parsed.Hostname()) {
		return fmt.Errorf("zotigod at %s is not reachable: %w", client.BaseURL(), err)
	}
	exited, startErr := startLocalDaemon(parsed.Host)
	if startErr != nil {
		return fmt.Errorf("zotigod at %s is not running and could not be started: %w", client.BaseURL(), startErr)
	}
	deadline := time.Now().Add(daemonStartTimeout)
	for {
		if err = checkHealth(ctx, client); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case exitErr := <-exited:
			// A concurrent starter may have won the port; zotigod exits 0
			// after confirming a compatible daemon is already listening.
			if exitErr == nil {
				if err = checkHealth(ctx, client); err == nil {
					return nil
				}
			}
			return fmt.Errorf("started zotigod exited before becoming ready (%v); see ~/.zotigo/logs/daemon", exitErr)
		case <-time.After(150 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("started zotigod did not become ready within %s: %w", daemonStartTimeout, err)
		}
	}
}

func withoutEnv(env []string, name string) []string {
	out := env[:0:0]
	for _, entry := range env {
		if !strings.HasPrefix(entry, name+"=") {
			out = append(out, entry)
		}
	}
	return out
}

func isConnectionRefused(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// startLocalDaemon launches zotigod detached from this process so the
// sessions it owns survive the TUI or ACP host exiting.
func startLocalDaemon(addr string) (<-chan error, error) {
	binary, err := findDaemonBinary()
	if err != nil {
		return nil, err
	}
	args := []string{"--addr", addr}
	if tokenFile := strings.TrimSpace(os.Getenv("ZOTIGOD_AUTH_TOKEN_FILE")); tokenFile != "" {
		args = append(args, "--auth-token-file", tokenFile)
	}
	cmd := exec.Command(binary, args...)
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}
	// The shared daemon always owns ~/.zotigo, whatever this shell exports.
	cmd.Env = withoutEnv(os.Environ(), config.StateDirEnv)
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = devNull.Close() }()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, devNull
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", binary, err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return exited, nil
}

// findDaemonBinary prefers the zotigod installed next to this executable
// (the installer links both into the same bin directory), then PATH.
// ZOTIGOD_BINARY overrides both for development builds.
func findDaemonBinary() (string, error) {
	if explicit := strings.TrimSpace(os.Getenv("ZOTIGOD_BINARY")); explicit != "" {
		return explicit, nil
	}
	if executable, err := os.Executable(); err == nil {
		for _, dir := range candidateDirs(executable) {
			candidate := filepath.Join(dir, "zotigod")
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
				return candidate, nil
			}
		}
	}
	path, err := exec.LookPath("zotigod")
	if err != nil {
		return "", errors.New("zotigod binary not found next to this executable or on PATH (set ZOTIGOD_BINARY)")
	}
	return path, nil
}

func candidateDirs(executable string) []string {
	dirs := []string{filepath.Dir(executable)}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil && filepath.Dir(resolved) != dirs[0] {
		dirs = append(dirs, filepath.Dir(resolved))
	}
	return dirs
}
