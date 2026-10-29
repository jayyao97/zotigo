package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/gofrs/flock"
	"gopkg.in/yaml.v3"
)

var ErrModelSettingsConflict = errors.New("configuration changed; reload before saving")

// ModelSettingsProfile is a redacted projection of fields editable by clients.
// Provider-specific fields not represented here remain untouched in YAML.
type ModelSettingsProfile struct {
	Name                   string `json:"name"`
	Provider               string `json:"provider"`
	Model                  string `json:"model"`
	BaseURL                string `json:"base_url"`
	ThinkingLevel          string `json:"thinking_level"`
	ContextWindow          int    `json:"context_window"`
	MaxOutputTokens        *int64 `json:"max_output_tokens"`
	Mode                   string `json:"mode"`
	HasAPIKey              bool   `json:"has_api_key"`
	DefaultMaxOutputTokens *int64 `json:"default_max_output_tokens,omitempty"`
}

type ModelSettingsDefaults struct {
	ContextWindow   int   `json:"context_window"`
	MaxOutputTokens int64 `json:"max_output_tokens"`
}

type ModelSettings struct {
	Defaults       ModelSettingsDefaults  `json:"defaults"`
	Revision       string                 `json:"revision"`
	DefaultProfile string                 `json:"default_profile"`
	Profiles       []ModelSettingsProfile `json:"profiles"`
}

type ModelSettingsChange struct {
	Revision string               `json:"revision"`
	Action   string               `json:"action"` // save, delete, default
	Profile  ModelSettingsProfile `json:"profile"`
	APIKey   *string              `json:"api_key,omitempty"` // omitted: keep; empty: explicitly clear
}

func modelSettingsRevision(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func readModelDocument(path string) (*yaml.Node, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	var document yaml.Node
	if len(data) == 0 {
		data = nil
		document = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	} else if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, nil, errors.New("invalid configuration YAML; repair the file before editing models")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("configuration must be a YAML mapping")
	}
	// Editing aliases/merge keys can mutate shared values or shadow inherited secrets.
	var check func(*yaml.Node) error
	check = func(n *yaml.Node) error {
		if n.Kind == yaml.AliasNode || n.Anchor != "" || n.Tag == "!!merge" {
			return errors.New("configuration uses YAML anchors or merges; edit this file directly")
		}
		for _, c := range n.Content {
			if err := check(c); err != nil {
				return err
			}
		}
		return nil
	}
	if err := check(&document); err != nil {
		return nil, nil, err
	}
	var cfg Config
	if err := document.Decode(&cfg); err != nil {
		return nil, nil, errors.New("invalid configuration field types")
	}
	return &document, data, nil
}

func modelNode(node *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func setModelNode(node *yaml.Node, key string, value any) error {
	var encoded yaml.Node
	if err := encoded.Encode(value); err != nil {
		return err
	}
	if old := modelNode(node, key); old != nil {
		encoded.HeadComment = old.HeadComment
		encoded.LineComment = old.LineComment
		encoded.FootComment = old.FootComment
		*old = encoded
	} else {
		node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &encoded)
	}
	return nil
}

func projectModelSettings(document *yaml.Node, data []byte) (ModelSettings, error) {
	var cfg Config
	if err := document.Decode(&cfg); err != nil {
		return ModelSettings{}, errors.New("invalid model configuration")
	}
	out := ModelSettings{Defaults: ModelSettingsDefaults{ContextWindow: DefaultContextWindow, MaxOutputTokens: DefaultMaxOutputTokens}, Revision: modelSettingsRevision(data), DefaultProfile: cfg.DefaultProfile, Profiles: []ModelSettingsProfile{}}
	for name, p := range cfg.Profiles {
		mode, _ := p.Params["mode"].(string)
		fallback := p
		fallback.MaxOutputTokens = nil
		var defaultOutput *int64
		if value, err := fallback.EffectiveMaxOutputTokens(); err == nil {
			defaultOutput = &value
		}
		out.Profiles = append(out.Profiles, ModelSettingsProfile{Name: name, Provider: p.Provider, Model: p.Model, BaseURL: p.BaseURL, ThinkingLevel: p.ThinkingLevel, ContextWindow: p.ContextWindow, MaxOutputTokens: p.MaxOutputTokens, Mode: mode, HasAPIKey: p.APIKey != "", DefaultMaxOutputTokens: defaultOutput})
	}
	sort.Slice(out.Profiles, func(i, j int) bool { return out.Profiles[i].Name < out.Profiles[j].Name })
	return out, nil
}

func (m *Manager) ReadModelSettings() (ModelSettings, error) {
	path, err := m.GetConfigPath()
	if err != nil {
		return ModelSettings{}, err
	}
	doc, data, err := readModelDocument(path)
	if err != nil {
		return ModelSettings{}, err
	}
	return projectModelSettings(doc, data)
}

var modelProfileName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

// ModelTestProfile resolves the retained secret server-side without exposing it to clients.
func (m *Manager) ModelTestProfile(change ModelSettingsChange) (ProfileConfig, error) {
	if err := ValidateModelSettingsProfile(change.Profile); err != nil {
		return ProfileConfig{}, err
	}
	path, err := m.GetConfigPath()
	if err != nil {
		return ProfileConfig{}, err
	}
	doc, data, err := readModelDocument(path)
	if err != nil {
		return ProfileConfig{}, err
	}
	if change.Revision != modelSettingsRevision(data) {
		return ProfileConfig{}, ErrModelSettingsConflict
	}
	var cfg Config
	_ = doc.Decode(&cfg)
	p := change.Profile
	key := cfg.Profiles[p.Name].APIKey
	if change.APIKey != nil {
		key = *change.APIKey
	}
	params := cfg.Profiles[p.Name].Params
	if params == nil {
		params = map[string]any{}
	}
	params["mode"] = p.Mode
	return ProfileConfig{Provider: p.Provider, Model: p.Model, APIKey: key, BaseURL: p.BaseURL, ThinkingLevel: p.ThinkingLevel, ContextWindow: p.ContextWindow, MaxOutputTokens: p.MaxOutputTokens, Params: params}, nil
}

func ValidateModelSettingsProfile(p ModelSettingsProfile) error {
	if !modelProfileName.MatchString(p.Name) {
		return errors.New("profile name must use lowercase letters, digits, dots, hyphens or underscores (1-64 characters)")
	}
	if p.Provider != "openai" && p.Provider != "anthropic" && p.Provider != "gemini" && p.Provider != "deepseek" {
		return errors.New("provider must be openai, anthropic, gemini or deepseek")
	}
	if strings.TrimSpace(p.Model) == "" || len(p.Model) > 256 {
		return errors.New("model ID is required (at most 256 characters)")
	}
	if p.BaseURL != "" {
		u, err := url.Parse(p.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("base URL must be HTTP(S), without credentials, query or fragment")
		}
	}
	if p.ContextWindow < 0 || p.ContextWindow > 100000000 {
		return errors.New("context window must be between 0 and 100000000")
	}
	if p.MaxOutputTokens != nil && (*p.MaxOutputTokens <= 0 || *p.MaxOutputTokens > 100000000) {
		return errors.New("max output tokens must be between 1 and 100000000")
	}
	if p.Mode != "" && (p.Provider != "openai" || (p.Mode != "chat" && p.Mode != "response")) {
		return errors.New("API mode must be chat or response for OpenAI-compatible profiles")
	}
	switch p.ThinkingLevel {
	case "", "disabled", "none", "minimal", "low", "medium", "high", "xhigh", "max":
	default:
		return errors.New("unsupported thinking level")
	}
	if p.Provider != "openai" {
		switch p.ThinkingLevel {
		case "", "low", "medium", "high":
		case "disabled":
			if p.Provider != "anthropic" && p.Provider != "deepseek" {
				return errors.New("thinking level is not supported by this provider")
			}
		default:
			return errors.New("thinking level is not supported by this provider")
		}
	}
	return nil
}

// UpdateModelSettings patches only the selected profile, retaining unrelated
// YAML fields and comments. Revision checks reject stale forms and serialize UI writers.
func (m *Manager) UpdateModelSettings(change ModelSettingsChange) (ModelSettings, error) {
	path, err := m.GetConfigPath()
	if err != nil {
		return ModelSettings{}, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return ModelSettings{}, err
	}
	if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return ModelSettings{}, errors.New("configuration is a symbolic link; edit its target directly")
	}
	lock := flock.New(path + ".models.lock")
	if err = lock.Lock(); err != nil {
		return ModelSettings{}, err
	}
	defer func() { _ = lock.Unlock() }()
	doc, data, err := readModelDocument(path)
	if err != nil {
		return ModelSettings{}, err
	}
	if change.Revision == "" || change.Revision != modelSettingsRevision(data) {
		return ModelSettings{}, ErrModelSettingsConflict
	}
	root := doc.Content[0]
	profiles := modelNode(root, "profiles")
	if profiles == nil || profiles.Tag == "!!null" {
		if err = setModelNode(root, "profiles", map[string]any{}); err != nil {
			return ModelSettings{}, err
		}
		profiles = modelNode(root, "profiles")
	}
	if profiles.Kind != yaml.MappingNode {
		return ModelSettings{}, errors.New("profiles must be a mapping")
	}
	name := change.Profile.Name
	old := modelNode(profiles, name)
	var cfg Config
	_ = doc.Decode(&cfg)
	switch change.Action {
	case "save", "create":
		if change.Action == "create" && old != nil {
			return ModelSettings{}, errors.New("profile already exists; choose another name")
		}
		if change.Action == "save" && old == nil {
			return ModelSettings{}, errors.New("profile no longer exists; reload before saving")
		}
		if err = ValidateModelSettingsProfile(change.Profile); err != nil {
			return ModelSettings{}, err
		}
		for existing := range cfg.Profiles {
			if existing != name && strings.EqualFold(existing, name) {
				return ModelSettings{}, errors.New("profile name conflicts with existing configuration")
			}
		}
		if old == nil {
			if err = setModelNode(profiles, name, map[string]any{}); err != nil {
				return ModelSettings{}, err
			}
			old = modelNode(profiles, name)
		}
		if old.Kind != yaml.MappingNode {
			return ModelSettings{}, errors.New("profile must be a mapping")
		}
		p := change.Profile
		for key, value := range map[string]any{"provider": p.Provider, "model": p.Model, "base_url": p.BaseURL, "thinking_level": p.ThinkingLevel, "context_window": p.ContextWindow, "max_output_tokens": p.MaxOutputTokens} {
			if err = setModelNode(old, key, value); err != nil {
				return ModelSettings{}, err
			}
		}
		if change.APIKey != nil {
			if err = setModelNode(old, "api_key", *change.APIKey); err != nil {
				return ModelSettings{}, err
			}
		}
		params := modelNode(old, "params")
		if params == nil || params.Tag == "!!null" {
			if err = setModelNode(old, "params", map[string]any{}); err != nil {
				return ModelSettings{}, err
			}
			params = modelNode(old, "params")
		}
		if params.Kind != yaml.MappingNode {
			return ModelSettings{}, errors.New("profile params must be a mapping")
		}
		if err = setModelNode(params, "mode", p.Mode); err != nil {
			return ModelSettings{}, err
		}
		if len(cfg.Profiles) == 0 && cfg.DefaultProfile == "" {
			if err = setModelNode(root, "default_profile", name); err != nil {
				return ModelSettings{}, err
			}
		}
	case "delete":
		if old == nil {
			return ModelSettings{}, errors.New("profile does not exist")
		}
		if cfg.DefaultProfile == name && len(cfg.Profiles) > 1 {
			return ModelSettings{}, errors.New("choose another default profile before deleting this one")
		}
		for other, p := range cfg.Profiles {
			if other != name && p.Safety.Classifier.Profile == name {
				return ModelSettings{}, errors.New("profile is used by another profile's safety classifier")
			}
		}
		for i := 0; i < len(profiles.Content); i += 2 {
			if profiles.Content[i].Value == name {
				profiles.Content = append(profiles.Content[:i], profiles.Content[i+2:]...)
				break
			}
		}
		if cfg.DefaultProfile == name {
			if err = setModelNode(root, "default_profile", ""); err != nil {
				return ModelSettings{}, err
			}
		}
	case "default":
		if old == nil {
			return ModelSettings{}, errors.New("profile does not exist")
		}
		if err = setModelNode(root, "default_profile", name); err != nil {
			return ModelSettings{}, err
		}
	default:
		return ModelSettings{}, errors.New("action must be create, save, delete or default")
	}
	updated, err := yaml.Marshal(doc)
	if err != nil {
		return ModelSettings{}, err
	}
	// Recheck external edits before publishing; editors do not use our advisory lock.
	latest, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return ModelSettings{}, err
	}
	if modelSettingsRevision(latest) != change.Revision {
		return ModelSettings{}, ErrModelSettingsConflict
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".models-*")
	if err != nil {
		return ModelSettings{}, err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err = file.Write(updated); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return ModelSettings{}, err
	}
	if closeErr != nil {
		return ModelSettings{}, closeErr
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return ModelSettings{}, fmt.Errorf("publish model configuration: %w", err)
	}
	return projectModelSettings(doc, updated)
}
