package wiring

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	var opts []prompt.SystemPromptOption

	if cfg.SkillManager != nil {
		opts = append(opts, prompt.WithDynamicSection("available_skills", func(_ prompt.PromptContext) string {
			_ = cfg.SkillManager.Load()
			return cfg.SkillManager.BuildSkillIndex()
		}))
	}

	return prompt.NewSystemPromptBuilder(opts...)
}

func NewUserContextBuilder(cfg PromptConfig) *prompt.UserContextBuilder {
	opts := []prompt.UserContextOption{
		prompt.WithContext("environment", func(ctx prompt.PromptContext) string {
			lines := []string{
				fmt.Sprintf("working_directory: %s", ctx.WorkDir),
				fmt.Sprintf("platform: %s", ctx.Platform),
			}
			if cfg.Transport != "" {
				lines = append(lines, fmt.Sprintf("transport: %s", cfg.Transport))
			}
			now := time.Now()
			lines = append(lines,
				fmt.Sprintf("current_date: %s", now.Format("2006-01-02")),
				fmt.Sprintf("timezone: %s", currentTimezone()),
			)
			return strings.Join(lines, "\n")
		}),
	}

	if cfg.IncludeProjectInstructions {
		for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
			path := filepath.Join(cfg.WorkDir, name)
			if data, err := os.ReadFile(path); err == nil {
				content := string(data)
				source := fmt.Sprintf(`source="%s"`, name)
				opts = append(opts, prompt.WithAttributedContext("project_instructions", source, func(_ prompt.PromptContext) string {
					return content
				}))
			}
		}
	}

	return prompt.NewUserContextBuilder(opts...)
}

func NewSkillManager(workDir string) (*skills.SkillManager, error) {
	sm := skills.NewSkillManager(workDir)
	if err := sm.Load(); err != nil {
		return sm, err
	}
	return sm, nil
}

func currentTimezone() string {
	if tz := strings.TrimSpace(os.Getenv("TZ")); tz != "" {
		return tz
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if idx := strings.LastIndex(target, "zoneinfo/"); idx >= 0 {
			if name := strings.TrimSpace(target[idx+len("zoneinfo/"):]); name != "" {
				return name
			}
		}
	}
	name := time.Now().Location().String()
	if name == "" || name == "Local" {
		return time.Now().Format("-07:00")
	}
	return name
}
