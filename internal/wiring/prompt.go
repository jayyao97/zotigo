package wiring

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/jayyao97/zotigo/core/agent/prompt"
	"github.com/jayyao97/zotigo/core/skills"
)

type PromptConfig struct {
	WorkDir                    string
	Transport                  string
	IncludeProjectInstructions bool
	SkillManager               *skills.SkillManager
}

func NewSystemPromptBuilder(cfg PromptConfig) *prompt.SystemPromptBuilder {
	opts := []prompt.SystemPromptOption{
		prompt.WithDynamicSection("environment", func(ctx prompt.PromptContext) string {
			if cfg.Transport != "" {
				return fmt.Sprintf("Working directory: %s\nPlatform: %s\nTransport: %s",
					ctx.WorkDir, ctx.Platform, cfg.Transport)
			}
			return fmt.Sprintf("Working directory: %s\nPlatform: %s",
				ctx.WorkDir, ctx.Platform)
		}),
	}

	if cfg.IncludeProjectInstructions {
		if data, err := os.ReadFile(filepath.Join(cfg.WorkDir, "AGENTS.md")); err == nil {
			content := string(data)
			opts = append(opts, prompt.WithDynamicSection("project_instructions", func(_ prompt.PromptContext) string {
				return content
			}))
		}
	}

	if cfg.SkillManager != nil {
		opts = append(opts, prompt.WithDynamicSection("available_skills", func(_ prompt.PromptContext) string {
			_ = cfg.SkillManager.Load()
			return cfg.SkillManager.BuildSkillIndex()
		}))
	}

	return prompt.NewSystemPromptBuilder(opts...)
}

func NewSkillManager(workDir string) (*skills.SkillManager, error) {
	sm := skills.NewSkillManager(workDir)
	if err := sm.Load(); err != nil {
		return sm, err
	}
	return sm, nil
}
