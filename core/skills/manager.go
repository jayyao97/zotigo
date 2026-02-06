package skills

import (
	"fmt"
	"sort"
	"sync"
)

// SkillManager manages skill discovery, loading, and activation
type SkillManager struct {
	mu sync.RWMutex

	// skills maps skill names to definitions
	skills map[string]*SkillDefinition

	// aliases maps alias names to skill names
	aliases map[string]string

	// activated tracks which skills are currently activated
	activated map[string]bool

	// projectDir is the current project directory
	projectDir string

	// userDir is the user skills directory
	userDir string

	// loaded indicates if skills have been loaded
	loaded bool
}

// NewSkillManager creates a new skill manager
func NewSkillManager(projectDir string) *SkillManager {
	userDir, _ := GetUserSkillsDir()

	return &SkillManager{
		skills:     make(map[string]*SkillDefinition),
		aliases:    make(map[string]string),
		activated:  make(map[string]bool),
		projectDir: projectDir,
		userDir:    userDir,
	}
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

	// 2. Load user skills (medium priority)
	if m.userDir != "" {
		userSkills, err := DiscoverSkills(m.userDir, SkillSourceUser)
		if err != nil {
			return fmt.Errorf("failed to load user skills: %w", err)
		}
		for _, skill := range userSkills {
			m.addSkill(skill)
		}
	}

	// 3. Load project skills (highest priority)
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

// Activate activates a skill by name or alias
func (m *SkillManager) Activate(nameOrAlias string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Find the skill
	skill, ok := m.getSkillUnlocked(nameOrAlias)
	if !ok {
		return fmt.Errorf("skill not found: %s", nameOrAlias)
	}

	m.activated[skill.Name] = true
	return nil
}

// Deactivate deactivates a skill by name or alias
func (m *SkillManager) Deactivate(nameOrAlias string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Find the skill
	skill, ok := m.getSkillUnlocked(nameOrAlias)
	if !ok {
		return fmt.Errorf("skill not found: %s", nameOrAlias)
	}

	delete(m.activated, skill.Name)
	return nil
}

// DeactivateAll deactivates all skills
func (m *SkillManager) DeactivateAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activated = make(map[string]bool)
}

// IsActivated checks if a skill is activated
func (m *SkillManager) IsActivated(nameOrAlias string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	skill, ok := m.getSkillUnlocked(nameOrAlias)
	if !ok {
		return false
	}

	return m.activated[skill.Name]
}

// GetActivated returns all activated skills
func (m *SkillManager) GetActivated() []*SkillDefinition {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var skills []*SkillDefinition
	for name := range m.activated {
		if skill, ok := m.skills[name]; ok {
			skills = append(skills, skill)
		}
	}

	// Sort by name for consistent ordering
	sort.Slice(skills, func(i, j int) bool {
		return skills[i].Name < skills[j].Name
	})

	return skills
}

// getSkillUnlocked returns a skill without locking (caller must hold lock)
func (m *SkillManager) getSkillUnlocked(nameOrAlias string) (*SkillDefinition, bool) {
	if skill, ok := m.skills[nameOrAlias]; ok {
		return skill, true
	}
	if name, ok := m.aliases[nameOrAlias]; ok {
		if skill, ok := m.skills[name]; ok {
			return skill, true
		}
	}
	return nil, false
}

// Count returns the number of available skills
func (m *SkillManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.skills)
}

// ActivatedCount returns the number of activated skills
func (m *SkillManager) ActivatedCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.activated)
}
