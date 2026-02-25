package builtin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/skills"
	"github.com/jayyao97/zotigo/core/tools"
)

// ActivateSkillTool allows the LLM to activate skills during conversation
type ActivateSkillTool struct {
	manager *skills.SkillManager
}

// NewActivateSkillTool creates a new activate_skill tool
func NewActivateSkillTool(manager *skills.SkillManager) *ActivateSkillTool {
	return &ActivateSkillTool{manager: manager}
}

func (t *ActivateSkillTool) Name() string {
	return "activate_skill"
}

func (t *ActivateSkillTool) Description() string {
	return "Activates a skill to provide specialized instructions for a task. " +
		"Use this when you need specific expertise or workflows that a skill provides."
}

func (t *ActivateSkillTool) Schema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "The name or alias of the skill to activate",
			},
		},
		"required": []string{"name"},
	}
}

type activateSkillArgs struct {
	Name string `json:"name"`
}

func (t *ActivateSkillTool) Safety() tools.ToolSafety {
	return tools.ToolSafety{ReadOnly: true}
}

func (t *ActivateSkillTool) Execute(ctx context.Context, exec executor.Executor, argsJSON string) (any, error) {
	if t.manager == nil {
		return nil, fmt.Errorf("skill manager not configured")
	}

	var args activateSkillArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if args.Name == "" {
		return nil, fmt.Errorf("skill name is required")
	}

	// Get the skill first to check if it exists
	skill, ok := t.manager.Get(args.Name)
	if !ok {
		// List available skills
		available := t.manager.List()
		names := make([]string, len(available))
		for i, s := range available {
			names[i] = s.Name
		}
		return map[string]any{
			"success":          false,
			"error":            fmt.Sprintf("skill not found: %s", args.Name),
			"available_skills": names,
		}, nil
	}

	// Check if already activated
	if t.manager.IsActivated(skill.Name) {
		return map[string]any{
			"success": true,
			"message": fmt.Sprintf("skill '%s' is already activated", skill.Name),
			"skill": map[string]any{
				"name":        skill.Name,
				"description": skill.Description,
			},
		}, nil
	}

	// Activate the skill
	if err := t.manager.Activate(skill.Name); err != nil {
		return map[string]any{
			"success": false,
			"error":   err.Error(),
		}, nil
	}

	// Return success with skill info and instructions
	return map[string]any{
		"success": true,
		"message": fmt.Sprintf("skill '%s' activated", skill.Name),
		"skill": map[string]any{
			"name":         skill.Name,
			"description":  skill.Description,
			"source":       skill.Source.String(),
			"instructions": skill.Instructions,
		},
	}, nil
}

// ListSkillsTool allows the LLM to list available skills
type ListSkillsTool struct {
	manager *skills.SkillManager
}

// NewListSkillsTool creates a new list_skills tool
func NewListSkillsTool(manager *skills.SkillManager) *ListSkillsTool {
	return &ListSkillsTool{manager: manager}
}

func (t *ListSkillsTool) Name() string {
	return "list_skills"
}

func (t *ListSkillsTool) Description() string {
	return "Lists all available skills that can be activated for specialized tasks."
}

func (t *ListSkillsTool) Schema() any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t *ListSkillsTool) Safety() tools.ToolSafety {
	return tools.ToolSafety{ReadOnly: true}
}

func (t *ListSkillsTool) Execute(ctx context.Context, exec executor.Executor, argsJSON string) (any, error) {
	if t.manager == nil {
		return nil, fmt.Errorf("skill manager not configured")
	}

	allSkills := t.manager.List()
	result := make([]map[string]any, len(allSkills))

	for i, skill := range allSkills {
		result[i] = map[string]any{
			"name":        skill.Name,
			"description": skill.Description,
			"aliases":     skill.Aliases,
			"source":      skill.Source.String(),
			"activated":   t.manager.IsActivated(skill.Name),
		}
	}

	return map[string]any{
		"skills": result,
		"count":  len(result),
	}, nil
}
