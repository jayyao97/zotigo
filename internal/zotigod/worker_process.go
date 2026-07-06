package zotigod

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
)

type workerLauncher interface {
	Start(ctx context.Context, sessionID string) error
}

type workerLauncherFunc func(ctx context.Context, sessionID string) error

func (fn workerLauncherFunc) Start(ctx context.Context, sessionID string) error {
	return fn(ctx, sessionID)
}

type processWorkerLauncher struct {
	executable string
	daemonURL  string
	workDir    string
	env        []string
	output     io.Writer
	logger     *log.Logger
}

func newProcessWorkerLauncher(daemonURL string, logger *log.Logger) (*processWorkerLauncher, error) {
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
		workDir:    workDir,
		env:        os.Environ(),
		output:     os.Stderr,
		logger:     logger,
	}, nil
}

func (l *processWorkerLauncher) Start(_ context.Context, sessionID string) error {
	if l == nil {
		return nil
	}
	cmd := exec.Command(l.executable,
		"--worker",
		"--daemon-url", l.daemonURL,
		"--session-id", sessionID,
	)
	cmd.Dir = l.workDir
	cmd.Env = l.env
	cmd.Stdout = l.output
	cmd.Stderr = l.output
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start worker: %w", err)
	}
	if l.logger != nil {
		l.logger.Printf("Started worker pid=%d session=%s", cmd.Process.Pid, sessionID)
	}
	go func() {
		err := cmd.Wait()
		if l.logger != nil && err != nil {
			l.logger.Printf("Worker exited session=%s err=%v", sessionID, err)
		}
	}()
	return nil
}
