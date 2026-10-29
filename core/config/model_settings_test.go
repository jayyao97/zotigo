package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModelSettingsPreservesSecretsAndUneditedYAML(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := NewManager()
	path, _ := m.GetConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	const original = `# retained header
default_profile: main
custom: keep-me
tools:
  web:
    search_provider: deepseek
profiles:
  main:
    provider: openai
    model: old-model
    api_key: never-return-this
    params:
      temperature: 0.2
    safety:
      classifier:
        enabled: false
`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.ReadModelSettings()
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(snapshot)
	if strings.Contains(string(encoded), "never-return-this") {
		t.Fatal("secret leaked")
	}
	p := snapshot.Profiles[0]
	if !p.HasAPIKey {
		t.Fatal("missing key indicator")
	}
	p.Model = "new-model"
	next, err := m.UpdateModelSettings(ModelSettingsChange{Revision: snapshot.Revision, Action: "save", Profile: p})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	for _, want := range []string{"# retained header", "custom: keep-me", "search_provider: deepseek", "temperature: 0.2", "enabled: false", "api_key: never-return-this", "model: new-model"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("lost %s", want)
		}
	}
	if _, err = m.UpdateModelSettings(ModelSettingsChange{Revision: snapshot.Revision, Action: "save", Profile: p}); !errors.Is(err, ErrModelSettingsConflict) {
		t.Fatalf("stale revision accepted: %v", err)
	}
	resolved, err := m.ModelTestProfile(ModelSettingsChange{Revision: next.Revision, Profile: p})
	if err != nil || resolved.APIKey != "never-return-this" || resolved.Params["temperature"] == nil {
		t.Fatalf("test failed to retain private config: %v", err)
	}
	empty := ""
	next, err = m.UpdateModelSettings(ModelSettingsChange{Revision: next.Revision, Action: "save", Profile: p, APIKey: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if next.Profiles[0].HasAPIKey {
		t.Fatal("explicit clear not saved")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("config permissions: %v", info.Mode())
	}
	_, err = m.LoadForDir("")
	if err != nil {
		t.Fatalf("saved configuration not loadable: %v", err)
	}
}

func TestModelSettingsCreateDefaultDeleteAndConflict(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := NewManager()
	s, err := m.ReadModelSettings()
	if err != nil {
		t.Fatal(err)
	}
	p := ModelSettingsProfile{Name: "one", Provider: "openai", Model: "example"}
	s, err = m.UpdateModelSettings(ModelSettingsChange{Revision: s.Revision, Action: "create", Profile: p})
	if err != nil {
		t.Fatal(err)
	}
	if s.DefaultProfile != "one" {
		t.Fatal("first profile not default")
	}
	if _, err = m.UpdateModelSettings(ModelSettingsChange{Revision: s.Revision, Action: "create", Profile: p}); err == nil {
		t.Fatal("duplicate create overwrote profile")
	}
	p.Name = "two"
	s, err = m.UpdateModelSettings(ModelSettingsChange{Revision: s.Revision, Action: "create", Profile: p})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.UpdateModelSettings(ModelSettingsChange{Revision: s.Revision, Action: "delete", Profile: ModelSettingsProfile{Name: "one"}}); err == nil {
		t.Fatal("deleted current default")
	}
	s, err = m.UpdateModelSettings(ModelSettingsChange{Revision: s.Revision, Action: "default", Profile: p})
	if err != nil {
		t.Fatal(err)
	}
	s, err = m.UpdateModelSettings(ModelSettingsChange{Revision: s.Revision, Action: "delete", Profile: ModelSettingsProfile{Name: "one"}})
	if err != nil {
		t.Fatal(err)
	}
	path, _ := m.GetConfigPath()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString("# external edit\n")
	_ = file.Close()
	if _, err = m.UpdateModelSettings(ModelSettingsChange{Revision: s.Revision, Action: "delete", Profile: p}); !errors.Is(err, ErrModelSettingsConflict) {
		t.Fatalf("external edit overwritten: %v", err)
	}
	s, err = m.ReadModelSettings()
	if err != nil {
		t.Fatal(err)
	}
	s, err = m.UpdateModelSettings(ModelSettingsChange{Revision: s.Revision, Action: "delete", Profile: p})
	if err != nil {
		t.Fatal(err)
	}
	if s.DefaultProfile != "" || len(s.Profiles) != 0 {
		t.Fatal("last profile deletion left dangling default")
	}
}

func TestModelSettingsInvalidProfile(t *testing.T) {
	for _, p := range []ModelSettingsProfile{{Name: "bad/name", Provider: "openai", Model: "m"}, {Name: "ok", Provider: "unknown", Model: "m"}, {Name: "ok", Provider: "openai"}, {Name: "ok", Provider: "openai", Model: "m", BaseURL: "https://user:secret@example.com"}, {Name: "ok", Provider: "gemini", Model: "m", Mode: "chat"}} {
		if ValidateModelSettingsProfile(p) == nil {
			t.Fatalf("invalid profile accepted: %s", p.Name)
		}
	}
}

func TestModelSettingsExistingNamesAndDeepSeek(t *testing.T) {
	for _, provider := range []string{"openai", "deepseek"} {
		p := ModelSettingsProfile{Name: "gpt-5.6-sol", Provider: provider, Model: "example"}
		if provider == "deepseek" {
			p.ThinkingLevel = "disabled"
		}
		if err := ValidateModelSettingsProfile(p); err != nil {
			t.Fatal(err)
		}
	}
}

func TestModelSettingsDottedDeepSeekProfileRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := NewManager()
	snapshot, err := m.ReadModelSettings()
	if err != nil {
		t.Fatal(err)
	}
	profile := ModelSettingsProfile{Name: "deepseek.v4", Provider: "deepseek", Model: "deepseek-v4-pro", ThinkingLevel: "disabled"}
	saved, err := m.UpdateModelSettings(ModelSettingsChange{Revision: snapshot.Revision, Action: "create", Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Profiles) != 1 || saved.Profiles[0].Name != profile.Name || saved.Profiles[0].Provider != "deepseek" {
		t.Fatalf("unexpected saved profile %+v", saved.Profiles)
	}
	profile.ThinkingLevel = "high"
	saved, err = m.UpdateModelSettings(ModelSettingsChange{Revision: saved.Revision, Action: "save", Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := m.ModelTestProfile(ModelSettingsChange{Revision: saved.Revision, Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Provider != "deepseek" || resolved.ThinkingLevel != "high" {
		t.Fatalf("wrong resolved profile %+v", resolved)
	}
}

func TestModelSettingsReportsRuntimeDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := NewManager()
	path, err := m.GetConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("profiles:\n  gemini:\n    provider: gemini\n    model: test\n    max_output_tokens: 8192\n    params:\n      max_tokens: 49152\n  native:\n    provider: anthropic\n    model: test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	settings, err := m.ReadModelSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Defaults.ContextWindow != DefaultContextWindow || settings.Defaults.MaxOutputTokens != DefaultMaxOutputTokens {
		t.Fatalf("defaults differ from runtime: %+v", settings.Defaults)
	}
	for _, p := range settings.Profiles {
		want := DefaultMaxOutputTokens
		if p.Provider == "gemini" {
			want = 49152
		}
		if p.DefaultMaxOutputTokens == nil || *p.DefaultMaxOutputTokens != want {
			t.Fatalf("wrong fallback for %s: %v", p.Name, p.DefaultMaxOutputTokens)
		}
	}
}
