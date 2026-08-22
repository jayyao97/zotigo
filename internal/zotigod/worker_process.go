package zotigod

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"

	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

type workerLauncher interface {
	Start(ctx context.Context, sessionID string, workingDirectory string) error
}

func (l *processWorkerLauncher) StartCodex(_ context.Context, spec zotigoruntime.WorkerLaunchSpec, socketPath string, onExit func()) error {
	if l == nil {
		return fmt.Errorf("codex worker launcher is not configured")
	}
	args := []string{
		"--codex-worker",
		"--daemon-url", l.daemonURL,
		"--session-id", spec.SessionID,
		"--session-store-root", spec.SessionStoreRoot,
		"--codex-socket", socketPath,
		"--codex-working-directory", spec.WorkingDirectory,
		"--codex-model", spec.Settings.Model,
		"--codex-reasoning-effort", spec.Settings.ReasoningEffort,
	}
	if spec.SessionBinding != nil && spec.SessionBinding.ConversationID != "" {
		args = append(args, "--codex-thread-id", spec.SessionBinding.ConversationID)
	}
	cmd := exec.Command(l.executable, args...)
	cmd.Dir = spec.WorkingDirectory
	cmd.Env = workerProcessEnv(l.env, l.authToken)
	cmd.Stdout = l.output
	cmd.Stderr = l.output
	if err := l.startTracked(spec.SessionID, "Codex worker", cmd, onExit); err != nil {
		return fmt.Errorf("start codex worker: %w", err)
	}
	return nil
}

type workerLauncherFunc func(ctx context.Context, sessionID string, workingDirectory string) error

func (fn workerLauncherFunc) Start(ctx context.Context, sessionID string, workingDirectory string) error {
	return fn(ctx, sessionID, workingDirectory)
}

type processWorkerLauncher struct {
	executable string
	daemonURL  string
	authToken  string
	workDir    string
	env        []string
	output     io.Writer
	logger     *log.Logger
	mu         sync.Mutex
	commands   map[string]*exec.Cmd
	wait       sync.WaitGroup
	closed     bool
}

func newProcessWorkerLauncher(daemonURL string, authToken string, logger *log.Logger) (*processWorkerLauncher, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable: %w", err)
	}
	workDir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolve workdir: %w", err)
	}
	return &processWorkerLauncher{
		executable: executable,
		daemonURL:  daemonURL,
		authToken:  authToken,
		workDir:    workDir,
		env:        os.Environ(),
		output:     os.Stderr,
		logger:     logger,
		commands:   make(map[string]*exec.Cmd),
	}, nil
}

func (l *processWorkerLauncher) Start(_ context.Context, sessionID string, workingDirectory string) error {
	if l == nil {
		return nil
	}
	cmd := exec.Command(l.executable,
		"--worker",
		"--daemon-url", l.daemonURL,
		"--session-id", sessionID,
	)
	cmd.Dir = l.workDir
	if workingDirectory != "" {
		cmd.Dir = workingDirectory
	}
	cmd.Env = workerProcessEnv(l.env, l.authToken)
	cmd.Stdout = l.output
	cmd.Stderr = l.output
	if err := l.startTracked(sessionID, "Worker", cmd, nil); err != nil {
		return fmt.Errorf("start worker: %w", err)
	}
	return nil
}

func (l *processWorkerLauncher) startTracked(sessionID string, label string, cmd *exec.Cmd, onExit func()) error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return fmt.Errorf("worker launcher is closed")
	}
	if err := cmd.Start(); err != nil {
		l.mu.Unlock()
		return err
	}
	if previous := l.commands[sessionID]; previous != nil && previous.Process != nil {
		_ = previous.Process.Kill()
	}
	l.commands[sessionID] = cmd
	l.wait.Add(1)
	l.mu.Unlock()
	if l.logger != nil {
		l.logger.Printf("Started %s pid=%d session=%s", label, cmd.Process.Pid, sessionID)
	}
	go func() {
		defer l.wait.Done()
		defer func() {
			if onExit != nil {
				onExit()
			}
		}()
		err := cmd.Wait()
		l.mu.Lock()
		if l.commands[sessionID] == cmd {
			delete(l.commands, sessionID)
		}
		l.mu.Unlock()
		if l.logger != nil && err != nil {
			l.logger.Printf("%s exited session=%s err=%v", label, sessionID, err)
		}
	}()
	return nil
}

func (l *processWorkerLauncher) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		l.wait.Wait()
		return
	}
	l.closed = true
	for _, cmd := range l.commands {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
	l.mu.Unlock()
	l.wait.Wait()
}

func workerProcessEnv(env []string, authToken string) []string {
	result := make([]string, 0, len(env)+1)
	prefix := workerAuthTokenEnv + "="
	for _, value := range env {
		if !strings.HasPrefix(value, prefix) {
			result = append(result, value)
		}
	}
	if authToken != "" {
		result = append(result, prefix+authToken)
	}
	return result
}
