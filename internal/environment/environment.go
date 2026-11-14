// Package environment provides a unified abstraction for execution environments.
// It combines Executor (code execution) and Store (state persistence) together,
// making it easier to manage different deployment scenarios.
package environment

import (
	"context"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/session"
)

// Environment represents a complete execution environment combining
// code execution capabilities and state persistence.
type Environment interface {
	// Executor returns the executor for running commands and file operations.
	Executor() executor.Executor

	// Store returns the session store for state persistence.
	Store() session.Store

	// Init initializes the environment (e.g., start sandbox, connect to services).
	Init(ctx context.Context) error

	// Close releases all resources held by the environment.
	Close() error

	// Type returns the environment type identifier.
	Type() Type
}

// Type identifies the environment type.
type Type string

const (
	TypeLocal  Type = "local"
	TypeE2B    Type = "e2b"
	TypeDocker Type = "docker"
	TypeCustom Type = "custom"
)

// Config holds configuration for creating an environment.
type Config struct {
	// Type specifies the environment type.
	Type Type

	// WorkDir is the working directory (for local/docker).
	WorkDir string

	// DataDir is the directory for storing session data (for local).
	// If empty, defaults to ~/.zotigo
	DataDir string

	// E2B configuration
	E2B *E2BConfig

	// Docker configuration
	Docker *DockerConfig

	// Custom components for flexible composition
	Custom *CustomConfig
}

// E2BConfig holds E2B-specific configuration.
type E2BConfig struct {
	APIKey   string
	Template string
	RedisURL string // For session storage
}

// DockerConfig holds Docker-specific configuration.
type DockerConfig struct {
	Image     string
	Volumes   []string
	Network   string
	StoreType string // "file" or "redis"
	RedisURL  string // If StoreType is "redis"
}

// CustomConfig allows custom combination of executor and store.
type CustomConfig struct {
	Executor executor.Executor
	Store    session.Store
}
