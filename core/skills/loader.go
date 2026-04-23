package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// SkillFileName is the required file name for skill definitions
	SkillFileName = "SKILL.md"

	// MaxScanDepth is the maximum directory depth to scan for skills
	MaxScanDepth = 3
)

// yamlFrontMatterRegex matches YAML front matter: ---\n...\n---
var yamlFrontMatterRegex = regexp.MustCompile(`(?s)^---\r?\n(.*?)\r?\n---\r?\n?(.*)$`)

// ParseSkillFile parses a SKILL.md file and returns a SkillDefinition
func ParseSkillFile(path string) (*SkillDefinition, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read skill file: %w", err)
	}

	return ParseSkillContent(string(content), path)
}

// ParseSkillContent parses skill content (YAML front matter + markdown)
func ParseSkillContent(content string, path string) (*SkillDefinition, error) {
	matches := yamlFrontMatterRegex.FindStringSubmatch(content)
	if matches == nil {
		return nil, fmt.Errorf("invalid skill format: missing YAML front matter")
	}

	yamlContent := matches[1]
	instructions := strings.TrimSpace(matches[2])

	var skill SkillDefinition
	if err := yaml.Unmarshal([]byte(yamlContent), &skill); err != nil {
		return nil, fmt.Errorf("failed to parse YAML front matter: %w", err)
	}

	if skill.Name == "" {
		return nil, fmt.Errorf("skill name is required")
	}

	skill.Instructions = instructions
	skill.Path = path

	return &skill, nil
}

// DiscoverSkills discovers all skills in a directory
func DiscoverSkills(dir string, source SkillSource) ([]*SkillDefinition, error) {
	var skills []*SkillDefinition

	// Check if directory exists
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return skills, nil // Directory doesn't exist, return empty
	}
	if err != nil {
		return nil, fmt.Errorf("failed to stat directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", dir)
	}

	// BFS traversal with depth limit
	err = walkSkillsDir(dir, 0, func(skillPath string) error {
		skill, err := ParseSkillFile(skillPath)
		if err != nil {
			// Log warning but continue
			return nil
		}
		skill.Source = source
		skills = append(skills, skill)
		return nil
	})

	if err != nil {
		return nil, err
	}

	return skills, nil
}

// walkSkillsDir walks the directory looking for SKILL.md files
func walkSkillsDir(dir string, depth int, fn func(string) error) error {
	if depth > MaxScanDepth {
		return nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("failed to read directory: %w", err)
	}

	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())

		if entry.IsDir() {
			// Check if this directory contains SKILL.md
			skillPath := filepath.Join(path, SkillFileName)
			if _, err := os.Stat(skillPath); err == nil {
				if err := fn(skillPath); err != nil {
					return err
				}
			}
			// Continue scanning subdirectories
			if err := walkSkillsDir(path, depth+1, fn); err != nil {
				return err
			}
		} else if entry.Name() == SkillFileName {
			// SKILL.md directly in this directory
			if err := fn(path); err != nil {
				return err
			}
		}
	}

	return nil
}

// GetUserSkillsDir returns the user skills directory path (Zotigo-native).
func GetUserSkillsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(home, ".zotigo", "skills"), nil
}

// GetAgentsUserSkillsDir returns the ~/.agents/skills/ directory path
// (shared across agent CLI tools like Claude Code, Aider, etc.).
//
// Always returns the path so that (a) directories created after process
// start are picked up by /skills --reload, and (b) non-ENOENT Stat errors
// (permission denied, symlink loops, stale NFS) surface through the normal
// DiscoverSkills warning path instead of being silently ignored.
func GetAgentsUserSkillsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(home, ".agents", "skills"), nil
}

// GetProjectSkillsDir returns the project skills directory path
func GetProjectSkillsDir(projectDir string) string {
	return filepath.Join(projectDir, ".zotigo", "skills")
}
