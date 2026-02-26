package builtin

import (
	"context"
	"fmt"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/skills"
	"github.com/jayyao97/zotigo/core/tools"
)

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
	return "Lists all available skills. All skills are automatically enabled and their instructions are injected into the system prompt."
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
		}
	}

	return map[string]any{
		"skills": result,
		"count":  len(result),
	}, nil
}
