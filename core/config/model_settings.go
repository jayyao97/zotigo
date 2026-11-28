package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/gofrs/flock"
	"gopkg.in/yaml.v3"
)

var ErrModelSettingsConflict = errors.New("configuration changed; reload before saving")
var ErrSearchAPIKeyRequired = errors.New("search requires an API key")

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

type SearchSettings struct {
	Provider       string `json:"provider"`
	HasTavilyKey   bool   `json:"has_tavily_key"`
	HasDeepSeekKey bool   `json:"has_deepseek_key"`
}

type SearchSettingsChange struct {
	Provider    string  `json:"provider"`
	TavilyKey   *string `json:"tavily_key,omitempty"`
	DeepSeekKey *string `json:"deepseek_key,omitempty"`
}

type ModelSettings struct {
	Search         SearchSettings         `json:"web_search"`
	Defaults       ModelSettingsDefaults  `json:"defaults"`
	Revision       string                 `json:"revision"`
	DefaultProfile string                 `json:"default_profile"`
	Profiles       []ModelSettingsProfile `json:"profiles"`
}

type ModelSettingsChange struct {
	Search   *SearchSettingsChange `json:"web_search,omitempty"`
	Revision string                `json:"revision"`
	Action   string                `json:"action"` // save, delete, default
	Profile  ModelSettingsProfile  `json:"profile"`
	APIKey   *string               `json:"api_key,omitempty"` // omitted: keep; empty: explicitly clear
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

func inheritedModelNode(node *yaml.Node, key string) *yaml.Node {
	if node.Kind == yaml.AliasNode {
		return inheritedModelNode(node.Alias, key)
	}
	if node.Kind == yaml.SequenceNode {
		for _, child := range node.Content {
			if value := inheritedModelNode(child, key); value != nil {
				return value
			}
		}
		return nil
	}
	if value := modelNode(node, key); value != nil {
		return value
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Tag == "!!merge" {
			return inheritedModelNode(node.Content[i+1], key)
		}
	}
	return nil
}

// Local overrides must not mutate shared nodes or freeze inherited values. Use
// merge references when possible; do not silently copy an inherited mapping.
func localModelMapping(parent *yaml.Node, key string) (*yaml.Node, error) {
	node := modelNode(parent, key)
	if node == nil {
		inherited := inheritedModelNode(parent, key)
		if inherited != nil && inherited.Tag != "!!null" {
			if inherited.Kind == yaml.AliasNode {
				copy := *inherited
				node = &copy
			} else if inherited.Anchor != "" {
				node = &yaml.Node{Kind: yaml.AliasNode, Value: inherited.Anchor, Alias: inherited}
			} else {
				return nil, fmt.Errorf("%s is inherited from a YAML merge without its own anchor; edit the shared definition in the configuration file", key)
			}
			parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, node)
		} else {
			// Not setModelNode(parent, key, map[string]any{}): an encoded empty
			// map is flow style, and every field added later stayed on its line.
			parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"})
		}
		node = modelNode(parent, key)
	}
	if node.Kind == yaml.AliasNode {
		alias := *node
		*node = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!merge", Value: "<<"}, &alias,
		}}
	}
	if node.Tag == "!!null" {
		*node = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping", key)
	}
	// Written mappings use block style like a hand-edited file, which also
	// unfolds profiles that earlier versions saved on a single line.
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i+1] == node {
			keyNode := parent.Content[i]
			unfoldMapping(node, func(comment string) { keyNode.LineComment = joinComments(keyNode.LineComment, comment) })
		}
	}
	return node, nil
}

// unfoldMapping switches a flow mapping to block style. A comment after its
// closing brace has no line of its own in block style; keep moves it where it
// still reads in its place rather than dropping it.
func unfoldMapping(node *yaml.Node, keep func(comment string)) {
	if node.Style&yaml.FlowStyle == 0 {
		return
	}
	node.Style &^= yaml.FlowStyle
	if node.LineComment != "" {
		keep(node.LineComment)
		node.LineComment = ""
	}
}

func joinComments(first, second string) string {
	return strings.TrimSpace(first + "\n" + second)
}

// A shared anchor may be used outside the profile, including in unknown fields.
// Compare the resolved whole document before publishing so such edits cannot
// silently change other consumers, and reparse to catch dangling anchor references.
func validateModelEdit(before, after []byte, change ModelSettingsChange) error {
	name := change.Profile.Name
	var original, updated map[string]any
	if len(before) == 0 {
		original = map[string]any{}
	} else if err := yaml.Unmarshal(before, &original); err != nil {
		return errors.New("cannot safely edit shared YAML configuration")
	}
	if err := yaml.Unmarshal(after, &updated); err != nil {
		return errors.New("this edit would break a YAML anchor reference; edit the shared definition in the configuration file")
	}
	strip := func(doc map[string]any) {
		if change.Action == "web_search" {
			tools, _ := doc["tools"].(map[string]any)
			web, _ := tools["web"].(map[string]any)
			delete(web, "search_provider")
			if change.Search.TavilyKey != nil {
				delete(web, "tavily_api_key")
			}
			if change.Search.DeepSeekKey != nil {
				delete(web, "deepseek_api_key")
			}
			if len(web) == 0 {
				delete(tools, "web")
			}
			if len(tools) == 0 {
				delete(doc, "tools")
			}
			return
		}
		delete(doc, "default_profile")
		if profiles, ok := doc["profiles"].(map[string]any); ok {
			switch change.Action {
			case "save":
				if profile, ok := profiles[name].(map[string]any); ok {
					for _, key := range []string{"provider", "model", "base_url", "thinking_level", "context_window", "max_output_tokens"} {
						delete(profile, key)
					}
					if change.APIKey != nil {
						delete(profile, "api_key")
					}
					if params, ok := profile["params"].(map[string]any); ok {
						delete(params, "mode")
						if len(params) == 0 {
							delete(profile, "params")
						}
					} else if profile["params"] == nil {
						delete(profile, "params")
					}
				}
			case "create", "delete":
				delete(profiles, name)
			}
			if len(profiles) == 0 {
				delete(doc, "profiles")
			}
		} else if doc["profiles"] == nil {
			delete(doc, "profiles")
		}
	}
	if change.Action == "delete" {
		if profiles, ok := updated["profiles"].(map[string]any); ok {
			if _, exists := profiles[name]; exists {
				return errors.New("this profile is inherited from a YAML merge; remove it from the shared definition in the configuration file")
			}
		}
	}
	strip(original)
	strip(updated)
	if !reflect.DeepEqual(original, updated) {
		return errors.New("this edit would change other configuration through a shared YAML anchor; edit the shared definition in the configuration file")
	}
	return nil
}

func projectModelSettings(document *yaml.Node, data []byte) (ModelSettings, error) {
	var cfg Config
	if err := document.Decode(&cfg); err != nil {
		return ModelSettings{}, errors.New("invalid model configuration")
	}
	out := ModelSettings{Search: SearchSettings{Provider: cfg.Tools.Web.SearchProvider, HasTavilyKey: cfg.Tools.Web.TavilyAPIKey != "", HasDeepSeekKey: cfg.Tools.Web.DeepSeekAPIKey != ""}, Defaults: ModelSettingsDefaults{ContextWindow: DefaultContextWindow, MaxOutputTokens: DefaultMaxOutputTokens}, Revision: modelSettingsRevision(data), DefaultProfile: cfg.DefaultProfile, Profiles: []ModelSettingsProfile{}}
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
	if err := expandGeneratedAnchors(doc); err != nil {
		return ModelSettings{}, err
	}
	root := doc.Content[0]
	// A flow-style root keeps everything inside it on one line, however its
	// children are styled. Its trailing comment moves to the end of the
	// document, ahead of the comments that followed it there.
	unfoldMapping(root, func(comment string) { doc.FootComment = joinComments(comment, doc.FootComment) })
	var profiles *yaml.Node
	if change.Action != "web_search" {
		profiles, err = localModelMapping(root, "profiles")
		if err != nil {
			return ModelSettings{}, err
		}
	}
	name := change.Profile.Name
	var cfg Config
	_ = doc.Decode(&cfg)
	_, exists := cfg.Profiles[name]
	switch change.Action {
	case "web_search":
		search := change.Search
		if search == nil || (search.Provider != "" && search.Provider != "tavily" && search.Provider != "deepseek") {
			return ModelSettings{}, errors.New("invalid web search provider")
		}
		tools, err := localModelMapping(root, "tools")
		if err != nil {
			return ModelSettings{}, err
		}
		web, err := localModelMapping(tools, "web")
		if err != nil {
			return ModelSettings{}, err
		}
		if err = setModelNode(web, "search_provider", search.Provider); err != nil {
			return ModelSettings{}, err
		}
		for _, key := range []struct {
			name  string
			value *string
		}{{"tavily_api_key", search.TavilyKey}, {"deepseek_api_key", search.DeepSeekKey}} {
			if key.value == nil {
				continue
			}
			if len(*key.value) > 8192 || strings.ContainsAny(*key.value, "\r\n") {
				return ModelSettings{}, errors.New("invalid search API key")
			}
			value := strings.TrimSpace(*key.value)
			if value == "" && *key.value != "" {
				continue
			}
			if err = setModelNode(web, key.name, value); err != nil {
				return ModelSettings{}, err
			}
		}
		var effective WebToolsConfig
		if err := web.Decode(&effective); err != nil {
			return ModelSettings{}, errors.New("invalid web search configuration")
		}
		if effective.SearchProvider == "deepseek" && strings.TrimSpace(effective.DeepSeekAPIKey) == "" && strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")) == "" {
			return ModelSettings{}, fmt.Errorf("deepseek %w or DEEPSEEK_API_KEY", ErrSearchAPIKeyRequired)
		}
		if effective.SearchProvider == "tavily" && strings.TrimSpace(effective.TavilyAPIKey) == "" && strings.TrimSpace(os.Getenv("TAVILY_API_KEY")) == "" {
			return ModelSettings{}, fmt.Errorf("tavily %w or TAVILY_API_KEY", ErrSearchAPIKeyRequired)
		}
	case "save", "create":
		if change.Action == "create" && exists {
			return ModelSettings{}, errors.New("profile already exists; choose another name")
		}
		if change.Action == "save" && !exists {
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
		old, err := localModelMapping(profiles, name)
		if err != nil {
			return ModelSettings{}, err
		}
		p := change.Profile
		previous := cfg.Profiles[name]
		apiKey := previous.APIKey
		if change.APIKey != nil {
			apiKey = *change.APIKey
		}
		// New fields are appended in the order people write profiles by hand;
		// existing fields keep their place.
		for _, field := range []struct {
			key             string
			value, previous any
		}{
			{"provider", p.Provider, previous.Provider},
			{"model", p.Model, previous.Model},
			{"api_key", apiKey, previous.APIKey},
			{"base_url", p.BaseURL, previous.BaseURL},
			{"thinking_level", p.ThinkingLevel, previous.ThinkingLevel},
			{"context_window", p.ContextWindow, previous.ContextWindow},
			{"max_output_tokens", p.MaxOutputTokens, previous.MaxOutputTokens},
		} {
			if reflect.DeepEqual(field.previous, field.value) {
				continue
			}
			if err = setModelNode(old, field.key, field.value); err != nil {
				return ModelSettings{}, err
			}
		}
		previousMode, _ := previous.Params["mode"].(string)
		if p.Mode != previousMode {
			params, err := localModelMapping(old, "params")
			if err != nil {
				return ModelSettings{}, err
			}
			if err = setModelNode(params, "mode", p.Mode); err != nil {
				return ModelSettings{}, err
			}
		}
		if len(cfg.Profiles) == 0 && cfg.DefaultProfile == "" {
			if err = setModelNode(root, "default_profile", name); err != nil {
				return ModelSettings{}, err
			}
		}
	case "delete":
		if !exists {
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
		if !exists {
			return ModelSettings{}, errors.New("profile does not exist")
		}
		if err = setModelNode(root, "default_profile", name); err != nil {
			return ModelSettings{}, err
		}
	default:
		return ModelSettings{}, errors.New("unknown configuration action")
	}
	updated, err := encodeConfigYAML(doc)
	if err != nil {
		return ModelSettings{}, err
	}
	if err := validateModelEdit(data, updated, change); err != nil {
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
