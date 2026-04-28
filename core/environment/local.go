package environment

import (
	"context"
	"fmt"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/session"
)

// LocalEnvironment is an environment for local CLI usage.
// It uses LocalExecutor for command execution and FileStore for session storage.
type LocalEnvironment struct {
	executor executor.Executor
	store    session.Store
	workDir  string
	dataDir  string
}

// NewLocal creates a new local environment.
// workDir is the working directory for code execution.
// dataDir is the directory for session storage (defaults to ~/.zotigo if empty).
func NewLocal(workDir string, dataDir string) (*LocalEnvironment, error) {
	exec, err := executor.NewLocalExecutor(workDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create executor: %w", err)
	}

	store, err := session.NewFileStore(dataDir)
	if err != nil {
		_ = exec.Close()
		return nil, fmt.Errorf("failed to create store: %w", err)
	}

	return &LocalEnvironment{
		executor: exec,
		store:    store,
		workDir:  workDir,
		dataDir:  dataDir,
	}, nil
}

// Executor returns the local executor.
func (e *LocalEnvironment) Executor() executor.Executor {
	return e.executor
}

// Store returns the file store.
func (e *LocalEnvironment) Store() session.Store {
	return e.store
}

// Init initializes the local environment (no-op for local).
func (e *LocalEnvironment) Init(ctx context.Context) error {
	return e.executor.Init(ctx)
}

// Close releases resources.
func (e *LocalEnvironment) Close() error {
	var errs []error
	if err := e.executor.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := e.store.Close(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("close errors: %v", errs)
	}
	return nil
}

// Type returns the environment type.
func (e *LocalEnvironment) Type() Type {
	return TypeLocal
}

// Ensure LocalEnvironment implements Environment
var _ Environment = (*LocalEnvironment)(nil)
