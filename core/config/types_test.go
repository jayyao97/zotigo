package config_test

import (
	"testing"

	"github.com/jayyao97/zotigo/core/config"
)

func TestDefaultContextWindow(t *testing.T) {
	// Sanity band: below 100k over-warns frontier models; above 1M
	// never warns even when a small local model is the actual ceiling.
	if config.DefaultContextWindow < 100_000 || config.DefaultContextWindow > 1_000_000 {
		t.Errorf("DefaultContextWindow = %d outside the expected 100k–1M band",
			config.DefaultContextWindow)
	}
}

func TestResolveProfile(t *testing.T) {
	cfg := &config.Config{
		DefaultProfile: " default ",
		Profiles: map[string]config.ProfileConfig{
			"default": {Provider: "openai"},
			"other":   {Provider: "anthropic"},
		},
	}
	name, profile, err := cfg.ResolveProfile("")
	if err != nil || name != "default" || profile.Provider != "openai" {
		t.Fatalf("resolve default profile: name=%q profile=%#v err=%v", name, profile, err)
	}
	name, profile, err = cfg.ResolveProfile(" other ")
	if err != nil || name != "other" || profile.Provider != "anthropic" {
		t.Fatalf("resolve explicit profile: name=%q profile=%#v err=%v", name, profile, err)
	}
	if _, _, err := cfg.ResolveProfile("missing"); err == nil {
		t.Fatal("expected missing profile error")
	}
}
