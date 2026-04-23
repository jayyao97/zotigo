package skills

import (
	"fmt"
	"os"
	"sort"
	"sync"
)

// SkillManager manages skill discovery and loading
type SkillManager struct {
	mu sync.RWMutex

	// skills maps skill names to definitions
	skills map[string]*SkillDefinition

	// aliases maps alias names to skill names
	aliases map[string]string

	// projectDir is the current project directory
	projectDir string

	// userDir is the user skills directory (~/.zotigo/skills/)
	userDir string

	// agentsDir is the ~/.agents/skills/ directory (shared across agent CLIs)
	agentsDir string

	// extraDirs are additional skill directories (for SDK users)
	extraDirs []string

	// loaded indicates if skills have been loaded
	loaded bool
}

// SkillManagerOption configures a SkillManager during construction.
type SkillManagerOption func(*SkillManager)

// WithUserDir overrides the default user skills directory.
func WithUserDir(dir string) SkillManagerOption {
	return func(m *SkillManager) { m.userDir = dir }
}

// WithAgentsDir overrides the default agents skills directory.
func WithAgentsDir(dir string) SkillManagerOption {
	return func(m *SkillManager) { m.agentsDir = dir }
}

// WithExtraDirs adds additional skill directories (loaded between user and project).
func WithExtraDirs(dirs ...string) SkillManagerOption {
	return func(m *SkillManager) { m.extraDirs = append(m.extraDirs, dirs...) }
}

// NewSkillManager creates a new skill manager
func NewSkillManager(projectDir string, opts ...SkillManagerOption) *SkillManager {
	userDir, _ := GetUserSkillsDir()
	agentsDir, _ := GetAgentsUserSkillsDir()

	m := &SkillManager{
		skills:     make(map[string]*SkillDefinition),
		aliases:    make(map[string]string),
		projectDir: projectDir,
		userDir:    userDir,
		agentsDir:  agentsDir,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Load discovers and loads all skills from builtin, user, and project sources
func (m *SkillManager) Load() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Clear existing skills
	m.skills = make(map[string]*SkillDefinition)
	m.aliases = make(map[string]string)

	// 1. Load builtin skills (lowest priority)
	for _, skill := range BuiltinSkills {
		m.addSkill(skill)
	}

	// 2. Load skills from ~/.agents/skills/ (shared across agent CLIs)
	if m.agentsDir != "" {
		agentsSkills, err := DiscoverSkills(m.agentsDir, SkillSourceAgents)
		if err != nil {
			fmt.Printf("Warning: failed to load skills from %s: %v\n", m.agentsDir, err)
		} else {
			for _, skill := range agentsSkills {
				m.addSkill(skill)
			}
		}
	}

	// 3. Load user skills (medium priority)
	if m.userDir != "" {
		userSkills, err := DiscoverSkills(m.userDir, SkillSourceUser)
		if err != nil {
			return fmt.Errorf("failed to load user skills: %w", err)
		}
		for _, skill := range userSkills {
			m.addSkill(skill)
		}
	}

	// 4. Load extra dirs (SDK-configurable, between user and project)
	for _, dir := range m.extraDirs {
		if dir == "" {
			continue
		}
		extraSkills, err := DiscoverSkills(dir, SkillSourceUser)
		if err != nil {
			return fmt.Errorf("failed to load skills from %s: %w", dir, err)
		}
		for _, skill := range extraSkills {
			m.addSkill(skill)
		}
	}

	// 5. Load project skills (highest priority)
	if m.projectDir != "" {
		projectSkillsDir := GetProjectSkillsDir(m.projectDir)
		projectSkills, err := DiscoverSkills(projectSkillsDir, SkillSourceProject)
		if err != nil {
			return fmt.Errorf("failed to load project skills: %w", err)
		}
		for _, skill := range projectSkills {
			m.addSkill(skill)
		}
	}

	m.loaded = true
	return nil
}

// Reload reloads all skills
func (m *SkillManager) Reload() error {
	return m.Load()
}

// SetAgentsDir updates the agents skills directory at runtime.
// Useful for tests that need to point at a different directory.
func (m *SkillManager) SetAgentsDir(dir string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.agentsDir = dir
}

// addSkill adds a skill to the manager (higher priority overwrites lower)
func (m *SkillManager) addSkill(skill *SkillDefinition) {
	// Remove old aliases if skill exists
	if old, exists := m.skills[skill.Name]; exists {
		for _, alias := range old.Aliases {
			delete(m.aliases, alias)
		}
	}

	// Add new skill
	m.skills[skill.Name] = skill

	// Add aliases
	for _, alias := range skill.Aliases {
		m.aliases[alias] = skill.Name
	}
}

// Get returns a skill by name or alias
func (m *SkillManager) Get(nameOrAlias string) (*SkillDefinition, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Try direct name lookup
	if skill, ok := m.skills[nameOrAlias]; ok {
		return skill, true
	}

	// Try alias lookup
	if name, ok := m.aliases[nameOrAlias]; ok {
		if skill, ok := m.skills[name]; ok {
			return skill, true
		}
	}

	return nil, false
}

// List returns all available skills sorted by name
func (m *SkillManager) List() []*SkillDefinition {
	m.mu.RLock()
	defer m.mu.RUnlock()

	skills := make([]*SkillDefinition, 0, len(m.skills))
	for _, skill := range m.skills {
		skills = append(skills, skill)
	}

	sort.Slice(skills, func(i, j int) bool {
		return skills[i].Name < skills[j].Name
	})

	return skills
}

// Count returns the number of available skills
func (m *SkillManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.skills)
}

// Dirs returns all skill directories that actually exist on disk.
// Used by the agent to whitelist these paths for auto-approved reads.
func (m *SkillManager) Dirs() []string {
	var dirs []string
	for _, d := range []string{m.agentsDir, m.userDir, GetProjectSkillsDir(m.projectDir)} {
		if d == "" {
			continue
		}
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			dirs = append(dirs, d)
		}
	}
	for _, d := range m.extraDirs {
		if d == "" {
			continue
		}
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			dirs = append(dirs, d)
		}
	}
	return dirs
}
