package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/jayyao97/zotigo/cli/commands"
)

// ModelCommand manages model selection.
type ModelCommand struct{}

func NewModelCommand() *ModelCommand {
	return &ModelCommand{}
}

func (c *ModelCommand) Name() string        { return "model" }
func (c *ModelCommand) Aliases() []string   { return []string{"m"} }
func (c *ModelCommand) Description() string { return "Show or change the current AI model" }
func (c *ModelCommand) Usage() string       { return "/model [model_name]" }

func (c *ModelCommand) Execute(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) == 0 {
		// Show available models
		return c.showModels(env)
	}

	// Set model
	modelName := args[0]
	if env.SetModel != nil {
		if err := env.SetModel(modelName); err != nil {
			return fmt.Errorf("failed to set model: %w", err)
		}
		env.Output("Model changed to: %s", modelName)
	} else {
		env.Output("Model switching is not supported in this context.")
	}
	return nil
}

func (c *ModelCommand) showModels(env *commands.Environment) error {
	if env.GetModels == nil {
		env.Output("Model listing is not available.")
		return nil
	}

	models := env.GetModels()
	if len(models) == 0 {
		env.Output("No models available.")
		return nil
	}

	var sb strings.Builder
	sb.WriteString("Available models:\n")
	for _, model := range models {
		sb.WriteString(fmt.Sprintf("  - %s\n", model))
	}
	sb.WriteString("\nUsage: /model <model_name>")
	env.Output("%s", sb.String())
	return nil
}
