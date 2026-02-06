package commands

import (
	"context"
	"strings"
	"testing"
)

// MockCommand for testing
type MockCommand struct {
	name        string
	aliases     []string
	description string
	executed    bool
	lastArgs    []string
}

func (c *MockCommand) Name() string        { return c.name }
func (c *MockCommand) Aliases() []string   { return c.aliases }
func (c *MockCommand) Description() string { return c.description }
func (c *MockCommand) Usage() string       { return "/" + c.name }

func (c *MockCommand) Execute(ctx context.Context, env *Environment, args []string) error {
	c.executed = true
	c.lastArgs = args
	return nil
}

func TestRegistry(t *testing.T) {
	registry := NewRegistry()

	cmd := &MockCommand{
		name:        "test",
		aliases:     []string{"t", "tst"},
		description: "A test command",
	}
	registry.Register(cmd)

	t.Run("get by name", func(t *testing.T) {
		found, ok := registry.Get("test")
		if !ok {
			t.Error("Command not found by name")
		}
		if found.Name() != "test" {
			t.Errorf("Wrong command returned")
		}
	})

	t.Run("get by alias", func(t *testing.T) {
		found, ok := registry.Get("t")
		if !ok {
			t.Error("Command not found by alias")
		}
		if found.Name() != "test" {
			t.Errorf("Wrong command returned")
		}
	})

	t.Run("case insensitive", func(t *testing.T) {
		found, ok := registry.Get("TEST")
		if !ok {
			t.Error("Command not found with uppercase name")
		}
		if found.Name() != "test" {
			t.Errorf("Wrong command returned")
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, ok := registry.Get("nonexistent")
		if ok {
			t.Error("Should not find nonexistent command")
		}
	})

	t.Run("list", func(t *testing.T) {
		cmds := registry.List()
		if len(cmds) != 1 {
			t.Errorf("Expected 1 command, got %d", len(cmds))
		}
	})
}

func TestParse(t *testing.T) {
	tests := []struct {
		input   string
		name    string
		args    []string
		ok      bool
	}{
		{"/help", "help", []string{}, true},
		{"/model gpt-4", "model", []string{"gpt-4"}, true},
		{"/test arg1 arg2 arg3", "test", []string{"arg1", "arg2", "arg3"}, true},
		{"  /help  ", "help", []string{}, true},
		{"hello", "", nil, false},
		{"/", "", nil, false},
		{"", "", nil, false},
	}

	for _, tc := range tests {
		name, args, ok := Parse(tc.input)
		if ok != tc.ok {
			t.Errorf("Parse(%q): ok = %v, want %v", tc.input, ok, tc.ok)
		}
		if name != tc.name {
			t.Errorf("Parse(%q): name = %q, want %q", tc.input, name, tc.name)
		}
		if ok && len(args) != len(tc.args) {
			t.Errorf("Parse(%q): len(args) = %d, want %d", tc.input, len(args), len(tc.args))
		}
	}
}

func TestIsCommand(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"/help", true},
		{"/model gpt-4", true},
		{"  /help", true},
		{"hello", false},
		{"/", false},
		{"", false},
		{"//", true}, // Edge case: starts with / and has content
	}

	for _, tc := range tests {
		result := IsCommand(tc.input)
		if result != tc.expected {
			t.Errorf("IsCommand(%q) = %v, want %v", tc.input, result, tc.expected)
		}
	}
}

func TestRegistryExecute(t *testing.T) {
	registry := NewRegistry()
	cmd := &MockCommand{name: "test", aliases: []string{"t"}}
	registry.Register(cmd)

	var output strings.Builder
	env := &Environment{
		Output: func(format string, args ...interface{}) {
			output.WriteString(format)
		},
	}

	ctx := context.Background()

	t.Run("execute valid command", func(t *testing.T) {
		cmd.executed = false
		err := registry.Execute(ctx, env, "/test arg1 arg2")
		if err != nil {
			t.Errorf("Execute failed: %v", err)
		}
		if !cmd.executed {
			t.Error("Command was not executed")
		}
		if len(cmd.lastArgs) != 2 {
			t.Errorf("Wrong args: %v", cmd.lastArgs)
		}
	})

	t.Run("execute by alias", func(t *testing.T) {
		cmd.executed = false
		err := registry.Execute(ctx, env, "/t")
		if err != nil {
			t.Errorf("Execute failed: %v", err)
		}
		if !cmd.executed {
			t.Error("Command was not executed")
		}
	})

	t.Run("execute unknown command", func(t *testing.T) {
		err := registry.Execute(ctx, env, "/unknown")
		if err == nil {
			t.Error("Expected error for unknown command")
		}
	})

	t.Run("execute invalid input", func(t *testing.T) {
		err := registry.Execute(ctx, env, "not a command")
		if err == nil {
			t.Error("Expected error for invalid input")
		}
	})
}
