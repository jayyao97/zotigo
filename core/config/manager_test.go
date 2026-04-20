package config_test

import (
	"os"
	"testing"

	"github.com/jayyao97/zotigo/core/config"
)

func TestConfigDefaults(t *testing.T) {
	defaults := config.DefaultConfig()
	if defaults.DefaultProfile != "gpt-4o" {
		t.Errorf("Expected default profile 'gpt-4o', got '%s'", defaults.DefaultProfile)
	}

	// Check if default profiles exist
	if _, ok := defaults.Profiles["gpt-4o"]; !ok {
		t.Error("Expected gpt-4o profile to exist")
	}

	classifier := defaults.Profiles["gpt-4o"].Safety.Classifier
	if !classifier.IsEnabled() {
		t.Error("Expected classifier to be enabled by default")
	}
	if classifier.ReviewThreshold == "" {
		t.Error("Expected classifier review_threshold default to be set")
	}
	if classifier.Profile != "" {
		t.Error("Expected classifier profile to default to current active profile when omitted")
	}
}

func TestProjectConfigMerge(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "zotigo_project_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	originalWd, _ := os.Getwd()
	defer os.Chdir(originalWd)
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("Failed to chdir: %v", err)
	}

	// Override default profile and add a custom profile
	projectConfigContent := []byte(`
default_profile: my-custom-model
profiles:
  my-custom-model:
    provider: openai
    model: gpt-4-turbo
ui:
  theme: light
`)
	if err := os.WriteFile("zotigo.yaml", projectConfigContent, 0644); err != nil {
		t.Fatalf("Failed to write project config: %v", err)
	}

	mgr := config.NewManager()
	cfg, err := mgr.Load()
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if cfg.DefaultProfile != "my-custom-model" {
		t.Errorf("Expected default profile override, got '%s'", cfg.DefaultProfile)
	}

	// Verify custom profile loaded
	profile, ok := cfg.Profiles["my-custom-model"]
	if !ok {
		t.Fatal("Custom profile not found")
	}
	if profile.Model != "gpt-4-turbo" {
		t.Errorf("Expected model gpt-4-turbo, got %s", profile.Model)
	}

	// Verify defaults preserved
	if _, ok := cfg.Profiles["gpt-4o"]; !ok {
		t.Error("Expected default profiles to be preserved")
	}

	if cfg.Tools.Web.TimeoutSec == 0 {
		t.Error("Expected tools defaults to be preserved")
	}
}

func TestProjectConfigMerge_PreservesSafetyDefaults(t *testing.T) {
	tmpDir := t.TempDir()

	originalWd, _ := os.Getwd()
	defer os.Chdir(originalWd)
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("Failed to chdir: %v", err)
	}

	projectConfigContent := []byte(`
profiles:
  gpt-4o:
    provider: openai
    model: gpt-4.1
    safety:
      classifier:
        timeout_ms: 1500
`)
	if err := os.WriteFile("zotigo.yaml", projectConfigContent, 0644); err != nil {
		t.Fatalf("Failed to write project config: %v", err)
	}

	mgr := config.NewManager()
	cfg, err := mgr.Load()
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	classifier := cfg.Profiles["gpt-4o"].Safety.Classifier
	if classifier.TimeoutMs != 1500 {
		t.Fatalf("Expected overridden timeout, got %d", classifier.TimeoutMs)
	}
	if !classifier.IsEnabled() {
		t.Error("Partial override (timeout_ms only) must not disable the classifier — Enabled should inherit from defaults")
	}
	if classifier.ReviewThreshold == "" {
		t.Error("Expected classifier mode default to be preserved")
	}
	if classifier.MaxRecentActions == 0 {
		t.Error("Expected max recent actions default to be preserved")
	}
}

func TestProjectConfigMerge_PreservesClassifierProfileOverride(t *testing.T) {
	tmpDir := t.TempDir()

	originalWd, _ := os.Getwd()
	defer os.Chdir(originalWd)
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("Failed to chdir: %v", err)
	}

	projectConfigContent := []byte(`
profiles:
  gpt-4o:
    provider: openai
    model: gpt-4.1
    safety:
      classifier:
        profile: gpt-5-mini
        timeout_ms: 1500
  gpt-5-mini:
    provider: openai
    model: gpt-5-mini
`)
	if err := os.WriteFile("zotigo.yaml", projectConfigContent, 0644); err != nil {
		t.Fatalf("Failed to write project config: %v", err)
	}

	mgr := config.NewManager()
	cfg, err := mgr.Load()
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	classifier := cfg.Profiles["gpt-4o"].Safety.Classifier
	if classifier.Profile != "gpt-5-mini" {
		t.Fatalf("Expected classifier profile override, got %q", classifier.Profile)
	}
	if classifier.TimeoutMs != 1500 {
		t.Fatalf("Expected overridden timeout, got %d", classifier.TimeoutMs)
	}
}

func TestProjectConfigMerge_ExplicitDisableClassifier(t *testing.T) {
	tmpDir := t.TempDir()

	originalWd, _ := os.Getwd()
	defer os.Chdir(originalWd)
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("Failed to chdir: %v", err)
	}

	projectConfigContent := []byte(`
profiles:
  gpt-4o:
    provider: openai
    model: gpt-4.1
    safety:
      classifier:
        enabled: false
`)
	if err := os.WriteFile("zotigo.yaml", projectConfigContent, 0644); err != nil {
		t.Fatalf("Failed to write project config: %v", err)
	}

	mgr := config.NewManager()
	cfg, err := mgr.Load()
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	classifier := cfg.Profiles["gpt-4o"].Safety.Classifier
	if classifier.IsEnabled() {
		t.Error("User explicitly set enabled: false but classifier is still enabled")
	}
	// Other defaults should still be inherited
	if classifier.ReviewThreshold == "" {
		t.Error("Expected classifier mode default to be preserved")
	}
}

func TestProjectConfigMerge_ExplicitDisableWithOtherOverrides(t *testing.T) {
	tmpDir := t.TempDir()

	originalWd, _ := os.Getwd()
	defer os.Chdir(originalWd)
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("Failed to chdir: %v", err)
	}

	projectConfigContent := []byte(`
profiles:
  gpt-4o:
    provider: openai
    model: gpt-4.1
    safety:
      classifier:
        enabled: false
        timeout_ms: 5000
`)
	if err := os.WriteFile("zotigo.yaml", projectConfigContent, 0644); err != nil {
		t.Fatalf("Failed to write project config: %v", err)
	}

	mgr := config.NewManager()
	cfg, err := mgr.Load()
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	classifier := cfg.Profiles["gpt-4o"].Safety.Classifier
	if classifier.IsEnabled() {
		t.Error("User explicitly set enabled: false + timeout_ms but classifier is still enabled")
	}
	if classifier.TimeoutMs != 5000 {
		t.Errorf("Expected timeout 5000, got %d", classifier.TimeoutMs)
	}
}

func TestProjectConfigMerge_CustomProfileGetsClassifierDefaults(t *testing.T) {
	tmpDir := t.TempDir()

	originalWd, _ := os.Getwd()
	defer os.Chdir(originalWd)
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("Failed to chdir: %v", err)
	}

	// A user-defined profile with no safety block at all. Before the fix,
	// this profile would load with Enabled=nil and IsEnabled() would return
	// false — silently disabling the classifier.
	projectConfigContent := []byte(`
profiles:
  my-ollama:
    provider: openai
    model: llama3
    base_url: http://localhost:11434
`)
	if err := os.WriteFile("zotigo.yaml", projectConfigContent, 0644); err != nil {
		t.Fatalf("Failed to write project config: %v", err)
	}

	mgr := config.NewManager()
	cfg, err := mgr.Load()
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	classifier := cfg.Profiles["my-ollama"].Safety.Classifier
	if !classifier.IsEnabled() {
		t.Error("Custom profile should get classifier default Enabled=true; got disabled")
	}
	if classifier.ReviewThreshold == "" {
		t.Error("Custom profile should get classifier default Mode")
	}
	if classifier.TimeoutMs == 0 {
		t.Error("Custom profile should get classifier default TimeoutMs")
	}
	if classifier.MaxRecentActions == 0 {
		t.Error("Custom profile should get classifier default MaxRecentActions")
	}
}

func TestProjectConfigMerge_CustomProfilePartialClassifier(t *testing.T) {
	tmpDir := t.TempDir()

	originalWd, _ := os.Getwd()
	defer os.Chdir(originalWd)
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("Failed to chdir: %v", err)
	}

	// A user-defined profile with partial classifier config. Fields not set
	// should inherit defaults; explicit fields should be preserved.
	projectConfigContent := []byte(`
profiles:
  my-ollama:
    provider: openai
    model: llama3
    safety:
      classifier:
        timeout_ms: 7500
`)
	if err := os.WriteFile("zotigo.yaml", projectConfigContent, 0644); err != nil {
		t.Fatalf("Failed to write project config: %v", err)
	}

	mgr := config.NewManager()
	cfg, err := mgr.Load()
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	classifier := cfg.Profiles["my-ollama"].Safety.Classifier
	if classifier.TimeoutMs != 7500 {
		t.Errorf("Expected user override timeout=7500, got %d", classifier.TimeoutMs)
	}
	if !classifier.IsEnabled() {
		t.Error("Partial override must not disable the classifier; Enabled should inherit default=true")
	}
	if classifier.ReviewThreshold == "" {
		t.Error("Mode should inherit default for partial override")
	}
}

func TestResolveClassifierProfile_DefaultsToActiveProfile(t *testing.T) {
	cfg := config.DefaultConfig()

	name, profile, err := cfg.ResolveClassifierProfile("gpt-4o")
	if err != nil {
		t.Fatalf("ResolveClassifierProfile error: %v", err)
	}
	if name != "gpt-4o" {
		t.Fatalf("Expected active profile fallback, got %q", name)
	}
	if profile.Model != "gpt-4o" {
		t.Fatalf("Expected gpt-4o model, got %q", profile.Model)
	}
}

func TestResolveClassifierProfile_UsesExplicitProfile(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Profiles["gpt-5-mini"] = config.ProfileConfig{
		Provider: "openai",
		Model:    "gpt-5-mini",
	}
	active := cfg.Profiles["gpt-4o"]
	active.Safety.Classifier.Profile = "gpt-5-mini"
	cfg.Profiles["gpt-4o"] = active

	name, profile, err := cfg.ResolveClassifierProfile("gpt-4o")
	if err != nil {
		t.Fatalf("ResolveClassifierProfile error: %v", err)
	}
	if name != "gpt-5-mini" {
		t.Fatalf("Expected explicit classifier profile, got %q", name)
	}
	if profile.Model != "gpt-5-mini" {
		t.Fatalf("Expected gpt-5-mini model, got %q", profile.Model)
	}
}

func TestResolveClassifierProfile_MissingExplicitProfile(t *testing.T) {
	cfg := config.DefaultConfig()
	active := cfg.Profiles["gpt-4o"]
	active.Safety.Classifier.Profile = "missing-mini"
	cfg.Profiles["gpt-4o"] = active

	_, _, err := cfg.ResolveClassifierProfile("gpt-4o")
	if err == nil {
		t.Fatal("Expected error for missing classifier profile")
	}
}
