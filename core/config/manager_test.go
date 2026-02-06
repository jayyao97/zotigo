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
}
