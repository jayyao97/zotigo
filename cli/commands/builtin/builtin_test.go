package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/cli/commands"
)

func newTestEnv() (*commands.Environment, *strings.Builder) {
	output := &strings.Builder{}
	env := &commands.Environment{
		Output: func(format string, args ...interface{}) {
			if len(args) > 0 {
				fmt.Fprintf(output, format, args...)
			} else {
				output.WriteString(format)
			}
		},
	}
	return env, output
}

func TestHelpCommand(t *testing.T) {
	registry := commands.NewRegistry()
	helpCmd := NewHelpCommand(registry)
	registry.Register(helpCmd)
	registry.Register(NewClearCommand())

	ctx := context.Background()

	t.Run("list all commands", func(t *testing.T) {
		env, output := newTestEnv()
		err := helpCmd.Execute(ctx, env, nil)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		result := output.String()
		if !strings.Contains(result, "help") {
			t.Error("Should list help command")
		}
		if !strings.Contains(result, "clear") {
			t.Error("Should list clear command")
		}
	})

	t.Run("help for specific command", func(t *testing.T) {
		env, output := newTestEnv()
		err := helpCmd.Execute(ctx, env, []string{"clear"})
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		result := output.String()
		if !strings.Contains(result, "clear") {
			t.Error("Should show clear command info")
		}
	})

	t.Run("help for unknown command", func(t *testing.T) {
		env, _ := newTestEnv()
		err := helpCmd.Execute(ctx, env, []string{"nonexistent"})
		if err == nil {
			t.Error("Should return error for unknown command")
		}
	})
}

func TestClearCommand(t *testing.T) {
	cmd := NewClearCommand()
	ctx := context.Background()

	t.Run("name and aliases", func(t *testing.T) {
		if cmd.Name() != "clear" {
			t.Errorf("Expected name 'clear', got %s", cmd.Name())
		}
		aliases := cmd.Aliases()
		if len(aliases) == 0 || aliases[0] != "reset" {
			t.Error("Expected 'reset' alias")
		}
	})

	t.Run("execute with callback", func(t *testing.T) {
		env, output := newTestEnv()
		cleared := false
		env.ClearHistory = func() {
			cleared = true
		}

		err := cmd.Execute(ctx, env, nil)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !cleared {
			t.Error("ClearHistory should have been called")
		}
		if !strings.Contains(output.String(), "cleared") {
			t.Error("Should output confirmation message")
		}
	})

	t.Run("execute without callback", func(t *testing.T) {
		env, output := newTestEnv()
		// No ClearHistory callback set

		err := cmd.Execute(ctx, env, nil)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		// Should still output message
		if !strings.Contains(output.String(), "cleared") {
			t.Error("Should output confirmation message")
		}
	})
}

func TestModelCommand(t *testing.T) {
	cmd := NewModelCommand()
	ctx := context.Background()

	t.Run("name and aliases", func(t *testing.T) {
		if cmd.Name() != "model" {
			t.Errorf("Expected name 'model', got %s", cmd.Name())
		}
		aliases := cmd.Aliases()
		if len(aliases) == 0 || aliases[0] != "m" {
			t.Error("Expected 'm' alias")
		}
	})

	t.Run("list models", func(t *testing.T) {
		env, output := newTestEnv()
		env.GetModels = func() []string {
			return []string{"gpt-4", "gpt-3.5-turbo", "claude-3"}
		}

		err := cmd.Execute(ctx, env, nil)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		result := output.String()
		if !strings.Contains(result, "gpt-4") {
			t.Error("Should list gpt-4")
		}
		if !strings.Contains(result, "claude-3") {
			t.Error("Should list claude-3")
		}
	})

	t.Run("list models without callback", func(t *testing.T) {
		env, output := newTestEnv()
		// No GetModels callback

		err := cmd.Execute(ctx, env, nil)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(output.String(), "not available") {
			t.Error("Should indicate listing not available")
		}
	})

	t.Run("set model", func(t *testing.T) {
		env, output := newTestEnv()
		setModelCalled := ""
		env.SetModel = func(model string) error {
			setModelCalled = model
			return nil
		}

		err := cmd.Execute(ctx, env, []string{"gpt-4"})
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if setModelCalled != "gpt-4" {
			t.Errorf("Expected SetModel called with 'gpt-4', got %s", setModelCalled)
		}
		if !strings.Contains(output.String(), "gpt-4") {
			t.Error("Should confirm model change")
		}
	})

	t.Run("set model without callback", func(t *testing.T) {
		env, output := newTestEnv()
		// No SetModel callback

		err := cmd.Execute(ctx, env, []string{"gpt-4"})
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(output.String(), "not supported") {
			t.Error("Should indicate switching not supported")
		}
	})
}

func TestCostCommand(t *testing.T) {
	cmd := NewCostCommand()
	ctx := context.Background()

	t.Run("name and aliases", func(t *testing.T) {
		if cmd.Name() != "cost" {
			t.Errorf("Expected name 'cost', got %s", cmd.Name())
		}
		aliases := cmd.Aliases()
		hasUsage := false
		for _, a := range aliases {
			if a == "usage" {
				hasUsage = true
				break
			}
		}
		if !hasUsage {
			t.Error("Expected 'usage' alias")
		}
	})

	t.Run("execute", func(t *testing.T) {
		env, output := newTestEnv()

		err := cmd.Execute(ctx, env, nil)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		// Currently just outputs "not yet implemented"
		if output.Len() == 0 {
			t.Error("Should output something")
		}
	})
}

func TestRegisterAll(t *testing.T) {
	registry := commands.NewRegistry()
	RegisterAll(registry)

	// Check all commands are registered
	expectedCommands := []string{"help", "clear", "model", "cost", "compress", "stats", "snapshot", "rewind", "snapshots"}
	for _, name := range expectedCommands {
		cmd, ok := registry.Get(name)
		if !ok {
			t.Errorf("Command %s should be registered", name)
		}
		if cmd.Name() != name {
			t.Errorf("Command name mismatch: got %s, want %s", cmd.Name(), name)
		}
	}

	// Check aliases work
	aliases := map[string]string{
		"h":         "help",
		"?":         "help",
		"reset":     "clear",
		"m":         "model",
		"usage":     "cost",
		"summarize": "compress",
		"compact":   "compress",
		"context":   "stats",
		"tokens":    "stats",
		"snap":      "snapshot",
		"save":      "snapshot",
		"restore":   "rewind",
		"undo":      "rewind",
		"snaps":     "snapshots",
		"history":   "snapshots",
	}
	for alias, expected := range aliases {
		cmd, ok := registry.Get(alias)
		if !ok {
			t.Errorf("Alias %s should resolve to a command", alias)
			continue
		}
		if cmd.Name() != expected {
			t.Errorf("Alias %s should resolve to %s, got %s", alias, expected, cmd.Name())
		}
	}
}

func TestCompressCommand(t *testing.T) {
	ctx := context.Background()

	t.Run("without agent", func(t *testing.T) {
		env, output := newTestEnv()
		cmd := NewCompressCommand()

		err := cmd.Execute(ctx, env, nil)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(output.String(), "requires") {
			t.Error("Should indicate agent required")
		}
	})

	t.Run("name and aliases", func(t *testing.T) {
		cmd := NewCompressCommand()
		if cmd.Name() != "compress" {
			t.Errorf("Expected name 'compress', got %s", cmd.Name())
		}
		aliases := cmd.Aliases()
		hasSummarize := false
		hasCompact := false
		for _, a := range aliases {
			if a == "summarize" {
				hasSummarize = true
			}
			if a == "compact" {
				hasCompact = true
			}
		}
		if !hasSummarize || !hasCompact {
			t.Error("Expected 'summarize' and 'compact' aliases")
		}
	})
}

func TestStatsCommand(t *testing.T) {
	ctx := context.Background()

	t.Run("without agent", func(t *testing.T) {
		env, output := newTestEnv()
		cmd := NewStatsCommand()

		err := cmd.Execute(ctx, env, nil)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(output.String(), "requires") {
			t.Error("Should indicate agent required")
		}
	})

	t.Run("name and aliases", func(t *testing.T) {
		cmd := NewStatsCommand()
		if cmd.Name() != "stats" {
			t.Errorf("Expected name 'stats', got %s", cmd.Name())
		}
		aliases := cmd.Aliases()
		hasContext := false
		hasTokens := false
		for _, a := range aliases {
			if a == "context" {
				hasContext = true
			}
			if a == "tokens" {
				hasTokens = true
			}
		}
		if !hasContext || !hasTokens {
			t.Error("Expected 'context' and 'tokens' aliases")
		}
	})
}

func TestSnapshotCommands(t *testing.T) {
	ctx := context.Background()

	t.Run("snapshot without exec", func(t *testing.T) {
		env, output := newTestEnv()
		cmd := NewSnapshotCommand()

		err := cmd.Execute(ctx, env, nil)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(output.String(), "requires") {
			t.Error("Should indicate exec capability required")
		}
	})

	t.Run("rewind without exec", func(t *testing.T) {
		env, output := newTestEnv()
		cmd := NewRewindCommand()

		err := cmd.Execute(ctx, env, nil)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(output.String(), "requires") {
			t.Error("Should indicate exec capability required")
		}
	})

	t.Run("snapshots without exec", func(t *testing.T) {
		env, output := newTestEnv()
		cmd := NewSnapshotsCommand()

		err := cmd.Execute(ctx, env, nil)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(output.String(), "requires") {
			t.Error("Should indicate exec capability required")
		}
	})
}
