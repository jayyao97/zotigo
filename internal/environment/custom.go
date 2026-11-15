package environment

import (
	"context"
	"fmt"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/session"
)

// CustomEnvironment allows flexible combination of any executor and store.
// Use this when you need a non-standard combination.
type CustomEnvironment struct {
	executor executor.Executor
	store    session.Store
	envType  Type
}

// NewCustom creates a custom environment with the given executor and store.
func NewCustom(exec executor.Executor, store session.Store) *CustomEnvironment {
	return &CustomEnvironment{
		executor: exec,
		store:    store,
		envType:  TypeCustom,
	}
}

// NewCustomWithType creates a custom environment with a specific type identifier.
func NewCustomWithType(exec executor.Executor, store session.Store, envType Type) *CustomEnvironment {
	return &CustomEnvironment{
		executor: exec,
		store:    store,
		envType:  envType,
	}
}

// Executor returns the executor.
func (e *CustomEnvironment) Executor() executor.Executor {
	return e.executor
}

// Store returns the store.
func (e *CustomEnvironment) Store() session.Store {
	return e.store
}

// Init initializes the environment.
func (e *CustomEnvironment) Init(ctx context.Context) error {
	return e.executor.Init(ctx)
}

// Close releases resources.
func (e *CustomEnvironment) Close() error {
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
func (e *CustomEnvironment) Type() Type {
	return e.envType
}

// Ensure CustomEnvironment implements Environment
var _ Environment = (*CustomEnvironment)(nil)
