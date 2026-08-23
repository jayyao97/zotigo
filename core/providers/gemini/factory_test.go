package gemini

import (
	"testing"

	"github.com/jayyao97/zotigo/core/config"
)

func TestNewResolvesMaxOutputTokens(t *testing.T) {
	topLevel := int64(65536)
	tests := []struct {
		name string
		cfg  config.ProfileConfig
		want int32
	}{
		{
			name: "native default",
			cfg:  config.ProfileConfig{Provider: "gemini", Model: "gemini-test", APIKey: "test"},
			want: 32768,
		},
		{
			name: "legacy params max tokens",
			cfg: config.ProfileConfig{Provider: "gemini", Model: "gemini-test", APIKey: "test",
				Params: map[string]any{"max_tokens": 49152}},
			want: 49152,
		},
		{
			name: "top level wins",
			cfg: config.ProfileConfig{Provider: "gemini", Model: "gemini-test", APIKey: "test",
				MaxOutputTokens: &topLevel, Params: map[string]any{"max_tokens": 49152}},
			want: 65536,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			provider, err := New(tc.cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			got := provider.(*ChatProvider).maxTokens
			if got != tc.want {
				t.Fatalf("maxTokens = %d, want %d", got, tc.want)
			}
		})
	}
}
