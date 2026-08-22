package codexapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

const defaultStartTimeout = 10 * time.Second

type Host struct {
	mu           sync.Mutex
	binaryPath   string
	runtimeDir   string
	output       io.Writer
	command      *exec.Cmd
	client       *Client
	socketPath   string
	version      string
	leases       int
	stopWhenIdle bool
	closed       bool
}

type HostOptions struct {
	StopWhenIdle bool
}

type Lease struct {
	RPC        RPC
	Version    string
	SocketPath string
	release    func() error
	once       sync.Once
}

func (l *Lease) Release() (err error) {
	if l == nil {
		return nil
	}
	l.once.Do(func() { err = l.release() })
	return err
}

func NewHost(binaryPath string, runtimeDir string, output io.Writer, options HostOptions) (*Host, error) {
	if binaryPath == "" {
		var err error
		binaryPath, err = exec.LookPath("codex")
		if err != nil {
			return nil, fmt.Errorf("find codex binary: %w", err)
		}
	}
	if runtimeDir == "" {
		return nil, fmt.Errorf("codex runtime directory is required")
	}
	return &Host{binaryPath: binaryPath, runtimeDir: runtimeDir, output: output, stopWhenIdle: options.StopWhenIdle}, nil
}

func Discover() (string, string, error) {
	binaryPath, err := exec.LookPath("codex")
	if err != nil {
		return "", "", err
	}
	output, err := exec.Command(binaryPath, "--version").Output()
	if err != nil {
		return "", "", fmt.Errorf("read codex version: %w", err)
	}
	return binaryPath, string(bytesTrimSpace(output)), nil
}

func (h *Host) Ensure(ctx context.Context) (RPC, string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ensureLocked(ctx)
}

func (h *Host) Acquire(ctx context.Context) (*Lease, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	client, version, err := h.ensureLocked(ctx)
	if err != nil {
		return nil, err
	}
	h.leases++
	return &Lease{
		RPC: client, Version: version, SocketPath: h.socketPath,
		release: h.release,
	}, nil
}

func (h *Host) ensureLocked(ctx context.Context) (RPC, string, error) {
	if h.closed {
		return nil, "", fmt.Errorf("codex app-server host is closed")
	}
	if h.client != nil {
		select {
		case <-h.client.Done():
			_ = h.stopLocked()
		default:
			return h.client, h.version, nil
		}
	}
	if err := os.MkdirAll(h.runtimeDir, 0o700); err != nil {
		return nil, "", fmt.Errorf("create codex runtime directory: %w", err)
	}
	if err := os.Chmod(h.runtimeDir, 0o700); err != nil {
		return nil, "", fmt.Errorf("protect codex runtime directory: %w", err)
	}
	h.socketPath = filepath.Join(h.runtimeDir, "app-server.sock")
	if err := os.Remove(h.socketPath); err != nil && !os.IsNotExist(err) {
		return nil, "", fmt.Errorf("remove stale codex socket: %w", err)
	}
	h.command = exec.Command(h.binaryPath, "app-server", "--listen", "unix://"+h.socketPath)
	h.command.Stdout = h.output
	h.command.Stderr = h.output
	if err := h.command.Start(); err != nil {
		return nil, "", fmt.Errorf("start codex app-server: %w", err)
	}
	startCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		startCtx, cancel = context.WithTimeout(ctx, defaultStartTimeout)
		defer cancel()
	}
	client, err := h.waitForClient(startCtx)
	if err != nil {
		_ = h.command.Process.Kill()
		_ = h.command.Wait()
		h.command = nil
		return nil, "", err
	}
	var initialized struct {
		UserAgent string `json:"userAgent"`
	}
	if err := client.Call(startCtx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "zotigod", "title": "Zotigo", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": true},
	}, &initialized); err != nil {
		_ = client.Close()
		_ = h.command.Process.Kill()
		_ = h.command.Wait()
		h.command = nil
		return nil, "", fmt.Errorf("initialize codex app-server: %w", err)
	}
	if err := client.Notify("initialized", map[string]any{}); err != nil {
		_ = client.Close()
		_ = h.stopLocked()
		return nil, "", fmt.Errorf("notify codex app-server initialized: %w", err)
	}
	h.client = client
	h.version = initialized.UserAgent
	go drainControlNotifications(client)
	return h.client, h.version, nil
}

func (h *Host) release() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.leases > 0 {
		h.leases--
	}
	if h.leases != 0 || h.closed || !h.stopWhenIdle {
		return nil
	}
	return h.stopLocked()
}

func drainControlNotifications(client *Client) {
	for message := range client.Notifications() {
		if len(message.ID) > 0 && message.Method != "" {
			_ = client.RespondError(message.ID, -32601, "zotigod control connection does not handle server requests")
		}
	}
}

func (h *Host) SocketPath() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.socketPath
}

func (h *Host) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	return h.stopLocked()
}

func (h *Host) stopLocked() error {
	var closeErrors []error
	if h.client != nil {
		closeErrors = append(closeErrors, h.client.Close())
		h.client = nil
	}
	if h.command != nil && h.command.Process != nil {
		if err := h.command.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
			closeErrors = append(closeErrors, err)
		}
		wait := make(chan error, 1)
		go func() { wait <- h.command.Wait() }()
		select {
		case <-time.After(2 * time.Second):
			_ = h.command.Process.Kill()
			<-wait
		case <-wait:
		}
		h.command = nil
	}
	return errors.Join(closeErrors...)
}

func (h *Host) waitForClient(ctx context.Context) (*Client, error) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		client, err := Dial(ctx, h.socketPath)
		if err == nil {
			return client, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for codex app-server socket: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func bytesTrimSpace(value []byte) []byte {
	start := 0
	for start < len(value) && (value[start] == ' ' || value[start] == '\n' || value[start] == '\r' || value[start] == '\t') {
		start++
	}
	end := len(value)
	for end > start && (value[end-1] == ' ' || value[end-1] == '\n' || value[end-1] == '\r' || value[end-1] == '\t') {
		end--
	}
	return value[start:end]
}
