// Package commands provides a framework for slash commands in the CLI.
package commands

import (
	"context"
	"fmt"
	"strings"
)

// Command represents a slash command that can be executed in the CLI.
type Command interface {
	// Name returns the command name (without slash).
	Name() string

	// Aliases returns alternative names for the command.
	Aliases() []string

	// Description returns a short description of the command.
	Description() string

	// Usage returns usage information (optional).
	Usage() string

	// Execute runs the command with the given arguments.
	Execute(ctx context.Context, env *Environment, args []string) error
}

// Environment provides access to the CLI environment for commands.
type Environment struct {
	// Agent is the current agent instance.
	Agent interface{}

	// Session is the current session.
	Session interface{}

	// Config holds the current configuration.
	Config interface{}

	// SkillManager manages skills.
	SkillManager interface{}

	// Output is a function to display output to the user.
	Output func(format string, args ...interface{})

	// ClearHistory clears the conversation history.
	ClearHistory func()

	// GetModels returns available model names.
	GetModels func() []string

	// SetModel sets the current model.
	SetModel func(model string) error

	// Exec executes a shell command and returns the output.
	Exec func(ctx context.Context, cmd string) (string, error)
}

// Registry manages command registration and lookup.
type Registry struct {
	commands map[string]Command
	aliases  map[string]string
}

// NewRegistry creates a new command registry.
func NewRegistry() *Registry {
	return &Registry{
		commands: make(map[string]Command),
		aliases:  make(map[string]string),
	}
}

// Register adds a command to the registry.
func (r *Registry) Register(cmd Command) {
	name := strings.ToLower(cmd.Name())
	r.commands[name] = cmd

	for _, alias := range cmd.Aliases() {
		r.aliases[strings.ToLower(alias)] = name
	}
}

// Get retrieves a command by name or alias.
func (r *Registry) Get(name string) (Command, bool) {
	name = strings.ToLower(name)

	// Check direct name
	if cmd, ok := r.commands[name]; ok {
		return cmd, true
	}

	// Check aliases
	if realName, ok := r.aliases[name]; ok {
		return r.commands[realName], true
	}

	return nil, false
}

// List returns all registered commands.
func (r *Registry) List() []Command {
	var cmds []Command
	for _, cmd := range r.commands {
		cmds = append(cmds, cmd)
	}
	return cmds
}

// Parse parses a slash command string into command name and arguments.
// Returns empty string if not a slash command.
func Parse(input string) (name string, args []string, ok bool) {
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, "/") {
		return "", nil, false
	}

	parts := strings.Fields(input[1:]) // Remove leading slash
	if len(parts) == 0 {
		return "", nil, false
	}

	return parts[0], parts[1:], true
}

// IsCommand checks if the input is a slash command.
func IsCommand(input string) bool {
	input = strings.TrimSpace(input)
	return strings.HasPrefix(input, "/") && len(input) > 1
}

// Execute parses and executes a command string.
func (r *Registry) Execute(ctx context.Context, env *Environment, input string) error {
	name, args, ok := Parse(input)
	if !ok {
		return fmt.Errorf("not a valid command")
	}

	cmd, found := r.Get(name)
	if !found {
		return fmt.Errorf("unknown command: /%s", name)
	}

	return cmd.Execute(ctx, env, args)
}
