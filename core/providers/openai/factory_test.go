package openai

import (
	"testing"

	"github.com/jayyao97/zotigo/core/config"
)

func TestModelUsesResponsesAPI(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		// gpt-5 family — reasoning-first, Responses API required.
		{"gpt-5", true},
		{"gpt-5-mini", true},
		{"gpt-5-codex", true},
		{"gpt-5-nano", true},
		{"GPT-5", true}, // case-insensitive
		// o-series.
		{"o1", true},
		{"o1-mini", true},
		{"o3", true},
		{"o3-pro", true},
		{"o4-mini", true},
		// Classic Chat models stay on /v1/chat/completions.
		{"gpt-4o", false},
		{"gpt-4o-mini", false},
		{"gpt-4-turbo", false},
		{"gpt-3.5-turbo", false},
		// Should not false-match substrings.
		{"gpt-50-whatever", false}, // not actually a gpt-5 variant
		{"o10-something", false},   // not an o1 variant
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			if got := modelUsesResponsesAPI(tc.model); got != tc.want {
				t.Errorf("modelUsesResponsesAPI(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

func TestResolveOpenAIMode(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.ProfileConfig
		want string
	}{
		{
			name: "explicit chat override on a gpt-5 model",
			cfg: config.ProfileConfig{
				Model:  "gpt-5",
				Params: map[string]any{"mode": "chat"},
			},
			want: "chat",
		},
		{
			name: "explicit response override on a gpt-4o model",
			cfg: config.ProfileConfig{
				Model:  "gpt-4o",
				Params: map[string]any{"mode": "response"},
			},
			want: "response",
		},
		{
			name: "gpt-5-codex auto-routes to response",
			cfg:  config.ProfileConfig{Model: "gpt-5-codex"},
			want: "response",
		},
		{
			name: "gpt-4o auto-routes to chat",
			cfg:  config.ProfileConfig{Model: "gpt-4o"},
			want: "chat",
		},
		{
			name: "empty model defaults to chat",
			cfg:  config.ProfileConfig{Model: ""},
			want: "chat",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveOpenAIMode(tc.cfg); got != tc.want {
				t.Errorf("resolveOpenAIMode() = %q, want %q", got, tc.want)
			}
		})
	}
}
