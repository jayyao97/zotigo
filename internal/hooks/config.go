package hooks

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	ConfigVersion  = 1
	ConfigFileName = "hooks.yaml"
)

type Config struct {
	Version int
	Hooks   map[EventName][]Handler
}

type Handler struct {
	Command   string
	Args      []string
	Agents    []string
	Matchers  []string
	TimeoutMS int
	Async     bool
}

type rawConfig struct {
	Version int                    `yaml:"version"`
	Hooks   map[string][]yaml.Node `yaml:"hooks"`
}

type rawHandler struct {
	Command   string   `yaml:"command"`
	Args      []string `yaml:"args,omitempty"`
	Agents    []string `yaml:"agents,omitempty"`
	Matchers  []string `yaml:"matchers,omitempty"`
	TimeoutMS *int     `yaml:"timeout_ms,omitempty"`
	Async     *bool    `yaml:"async,omitempty"`
}

func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".zotigo", ConfigFileName), nil
}

// LoadFile loads user hook configuration. Invalid handlers are omitted and
// returned as issues so one typo does not disable unrelated valid handlers.
func LoadFile(path string) (Config, []error, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return emptyConfig(), nil, nil
	}
	if err != nil {
		return Config{}, nil, fmt.Errorf("read hooks config: %w", err)
	}

	var raw rawConfig
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		return Config{}, nil, fmt.Errorf("decode hooks config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, nil, fmt.Errorf("decode hooks config: multiple YAML documents are not supported")
		}
		return Config{}, nil, fmt.Errorf("decode hooks config: %w", err)
	}
	if raw.Version != ConfigVersion {
		return Config{}, nil, fmt.Errorf("unsupported hooks config version %d", raw.Version)
	}

	config := emptyConfig()
	issues := make([]error, 0)
	for rawEvent, nodes := range raw.Hooks {
		eventName, ok := ParseEventName(rawEvent)
		if !ok {
			issues = append(issues, fmt.Errorf("hooks.%s: unknown event", rawEvent))
			continue
		}
		for index := range nodes {
			handler, err := decodeHandler(eventName, nodes[index])
			if err != nil {
				issues = append(issues, fmt.Errorf("hooks.%s[%d]: %w", eventName, index, err))
				continue
			}
			config.Hooks[eventName] = append(config.Hooks[eventName], handler)
		}
	}
	return config, issues, nil
}

func emptyConfig() Config {
	return Config{Version: ConfigVersion, Hooks: make(map[EventName][]Handler)}
}

func decodeHandler(eventName EventName, node yaml.Node) (Handler, error) {
	data, err := yaml.Marshal(&node)
	if err != nil {
		return Handler{}, fmt.Errorf("encode handler: %w", err)
	}
	var raw rawHandler
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		return Handler{}, err
	}
	raw.Command = strings.TrimSpace(raw.Command)
	if raw.Command == "" {
		return Handler{}, fmt.Errorf("command is required")
	}
	if raw.TimeoutMS != nil && *raw.TimeoutMS <= 0 {
		return Handler{}, fmt.Errorf("timeout_ms must be greater than zero")
	}
	if (eventName == SessionStart || eventName == SessionEnd) && len(raw.Matchers) > 0 {
		return Handler{}, fmt.Errorf("matchers are only valid for tool events")
	}
	if eventName == PreToolUse && raw.Async != nil && *raw.Async {
		return Handler{}, fmt.Errorf("async is not valid for PreToolUse")
	}

	agents := make([]string, 0, len(raw.Agents))
	for _, agent := range raw.Agents {
		agent = strings.TrimSpace(agent)
		if agent == "" {
			return Handler{}, fmt.Errorf("agents must not contain empty values")
		}
		agents = append(agents, agent)
	}
	matchers := make([]string, 0, len(raw.Matchers))
	for _, matcher := range raw.Matchers {
		matcher = strings.TrimSpace(matcher)
		if matcher == "" {
			return Handler{}, fmt.Errorf("matchers must not contain empty values")
		}
		matchers = append(matchers, matcher)
	}
	async := eventName == PostToolUse
	if raw.Async != nil {
		async = *raw.Async
	}
	timeoutMS := 0
	if raw.TimeoutMS != nil {
		timeoutMS = *raw.TimeoutMS
	}
	return Handler{
		Command: raw.Command, Args: append([]string(nil), raw.Args...), Agents: agents, Matchers: matchers,
		TimeoutMS: timeoutMS, Async: async,
	}, nil
}

func (h Handler) matchesAgent(agentName string) bool {
	if len(h.Agents) == 0 {
		return true
	}
	for _, agent := range h.Agents {
		if agent == agentName {
			return true
		}
	}
	return false
}

func (h Handler) matches(toolName string) bool {
	if len(h.Matchers) == 0 {
		return true
	}
	for _, matcher := range h.Matchers {
		if matcher == "*" || matcher == toolName {
			return true
		}
	}
	return false
}
