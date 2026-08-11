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

	// Traverse with a depth limit and canonical-directory deduplication so
	// directory symlinks work without allowing cycles.
	err = walkSkillsDir(dir, func(skillPath string) error {
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
func walkSkillsDir(root string, fn func(string) error) error {
	type pendingDir struct {
		path     string
		depth    int
		optional bool
	}

	queue := []pendingDir{{path: root}}
	visited := make(map[string]bool)
	seenSkillFiles := make(map[string]struct{})
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		resolvedDir, err := filepath.EvalSymlinks(current.path)
		if err != nil {
			if current.optional {
				continue
			}
			return fmt.Errorf("failed to resolve directory: %w", err)
		}
		resolvedDir, err = filepath.Abs(resolvedDir)
		if err != nil {
			if current.optional {
				continue
			}
			return fmt.Errorf("failed to resolve absolute directory path: %w", err)
		}
		visitedRequired, seen := visited[resolvedDir]
		if seen && (visitedRequired || current.optional) {
			continue
		}

		entries, err := os.ReadDir(current.path)
		if err != nil {
			if current.optional {
				continue
			}
			return fmt.Errorf("failed to read directory: %w", err)
		}
		if current.optional {
			if !seen {
				visited[resolvedDir] = false
			}
		} else {
			visited[resolvedDir] = true
		}
		for _, entry := range entries {
			path := filepath.Join(current.path, entry.Name())
			entryInfo, err := entry.Info()
			if err != nil {
				if current.optional {
					continue
				}
				return fmt.Errorf("failed to inspect directory entry %s: %w", path, err)
			}
			isSymlink := entryInfo.Mode()&os.ModeSymlink != 0
			isDir := entryInfo.IsDir()
			if isSymlink {
				info, err := os.Stat(path)
				if err != nil {
					continue
				}
				isDir = info.IsDir()
			}

			if isDir {
				if current.depth < MaxScanDepth+1 {
					queue = append(queue, pendingDir{
						path:     path,
						depth:    current.depth + 1,
						optional: current.optional || isSymlink,
					})
				}
			} else if entry.Name() == SkillFileName {
				skillKey := filepath.Join(resolvedDir, SkillFileName)
				if _, seen := seenSkillFiles[skillKey]; seen {
					continue
				}
				seenSkillFiles[skillKey] = struct{}{}
				if err := fn(path); err != nil {
					return err
				}
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
