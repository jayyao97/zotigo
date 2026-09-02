package skills

import (
	"fmt"
	"os"
	"sort"
	"sync"
)

type SkillManager struct {
	mu          sync.RWMutex
	skills      map[string]*SkillDefinition
	aliases     map[string]string
	diagnostics []Diagnostic
	projectDir  string
	userDir     string
	extraDirs   []string
	loaded      bool
}

type SkillManagerOption func(*SkillManager)

func WithUserDir(dir string) SkillManagerOption {
	return func(m *SkillManager) { m.userDir = dir }
}

// WithAgentsDir is retained as an alias for WithUserDir.
func WithAgentsDir(dir string) SkillManagerOption {
	return WithUserDir(dir)
}

func WithExtraDirs(dirs ...string) SkillManagerOption {
	return func(m *SkillManager) { m.extraDirs = append(m.extraDirs, dirs...) }
}

func NewSkillManager(projectDir string, opts ...SkillManagerOption) *SkillManager {
	userDir, _ := GetUserSkillsDir()
	manager := &SkillManager{
		skills: make(map[string]*SkillDefinition), aliases: make(map[string]string),
		projectDir: projectDir, userDir: userDir,
	}
	for _, opt := range opts {
		opt(manager)
	}
	return manager
}

func (m *SkillManager) Load() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.skills = make(map[string]*SkillDefinition)
	m.aliases = make(map[string]string)
	m.diagnostics = nil

	for _, skill := range BuiltinSkills {
		m.addSkill(skill)
	}
	if err := m.loadDir(m.userDir, SkillSourceUser); err != nil {
		m.diagnostics = append(m.diagnostics, Diagnostic{Code: "scan_failed", Message: err.Error(), Scope: SkillSourceUser.String()})
	}
	for _, dir := range m.extraDirs {
		if err := m.loadDir(dir, SkillSourceUser); err != nil {
			m.diagnostics = append(m.diagnostics, Diagnostic{Code: "scan_failed", Message: err.Error(), Scope: SkillSourceUser.String()})
		}
	}
	if m.projectDir != "" {
		if err := m.loadDir(GetProjectSkillsDir(m.projectDir), SkillSourceWorkspace); err != nil {
			m.diagnostics = append(m.diagnostics, Diagnostic{Code: "scan_failed", Message: err.Error(), Scope: SkillSourceWorkspace.String()})
		}
	}
	m.loaded = true
	return nil
}

func (m *SkillManager) loadDir(dir string, source SkillSource) error {
	if dir == "" {
		return nil
	}
	loaded, diagnostics, err := DiscoverSkillsWithDiagnostics(dir, source)
	m.diagnostics = append(m.diagnostics, diagnostics...)
	if err != nil {
		return err
	}
	for _, skill := range loaded {
		m.addSkill(skill)
	}
	return nil
}

func (m *SkillManager) Reload() error { return m.Load() }

func (m *SkillManager) EnsureLoaded() error {
	m.mu.RLock()
	loaded := m.loaded
	m.mu.RUnlock()
	if loaded {
		return nil
	}
	return m.Load()
}

func (m *SkillManager) SetAgentsDir(dir string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.userDir = dir
	m.loaded = false
}

func (m *SkillManager) addSkill(skill *SkillDefinition) {
	if old, exists := m.skills[skill.Name]; exists {
		for _, alias := range old.Aliases {
			delete(m.aliases, alias)
		}
		m.diagnostics = append(m.diagnostics, Diagnostic{
			Code: "skill_overridden", Scope: skill.Source.String(), Name: skill.Name,
			Message: fmt.Sprintf("skill %q from %s overrides %s", skill.Name, skill.Source.String(), old.Source.String()),
		})
	}
	m.skills[skill.Name] = skill
	for _, alias := range skill.Aliases {
		m.aliases[alias] = skill.Name
	}
}

func (m *SkillManager) Get(nameOrAlias string) (*SkillDefinition, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if skill, ok := m.skills[nameOrAlias]; ok {
		return skill, true
	}
	name, ok := m.aliases[nameOrAlias]
	if !ok {
		return nil, false
	}
	skill, ok := m.skills[name]
	return skill, ok
}

func (m *SkillManager) List() []*SkillDefinition {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*SkillDefinition, 0, len(m.skills))
	for _, skill := range m.skills {
		result = append(result, skill)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (m *SkillManager) Diagnostics() []Diagnostic {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]Diagnostic(nil), m.diagnostics...)
}

func (m *SkillManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.skills)
}

func (m *SkillManager) Dirs() []string {
	m.mu.RLock()
	dirs := append([]string{m.userDir, GetProjectSkillsDir(m.projectDir)}, m.extraDirs...)
	m.mu.RUnlock()
	var existing []string
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			existing = append(existing, dir)
		}
	}
	return existing
}
