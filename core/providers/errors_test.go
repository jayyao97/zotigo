package providers_test

import (
	"errors"
	"testing"

	"github.com/jayyao97/zotigo/core/providers"
)

func TestWrapIfContextLength(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantMatch bool
	}{
		{"openai", errors.New("This model's maximum context length is 128000 tokens."), true},
		{"openai code", errors.New(`{"error":{"code":"context_length_exceeded"}}`), true},
		{"anthropic", errors.New("prompt is too long: 250000 tokens"), true},
		{"gemini", errors.New("input token count (1500000) exceeds the maximum number of tokens"), true},
		{"llama.cpp", errors.New("Requested tokens exceed context window of 8192"), true},

		{"connection", errors.New("connection refused"), false},
		{"auth", errors.New("401 Unauthorized"), false},
		{"rate limit", errors.New("rate limit exceeded"), false},
		{"nil", nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := providers.WrapIfContextLength(tc.err)
			matched := errors.Is(got, providers.ErrContextLengthExceeded)
			if matched != tc.wantMatch {
				t.Errorf("WrapIfContextLength(%v) matched=%v, want %v", tc.err, matched, tc.wantMatch)
			}
		})
	}
}

func TestWrapIfContextLength_IdempotentAndPreservesOriginal(t *testing.T) {
	original := errors.New("maximum context length: 200k")
	wrapped := providers.WrapIfContextLength(original)
	if !errors.Is(wrapped, providers.ErrContextLengthExceeded) {
		t.Fatal("first wrap should match")
	}
	twice := providers.WrapIfContextLength(wrapped)
	if twice != wrapped {
		t.Error("second wrap should be a no-op")
	}
	if !errors.Is(twice, providers.ErrContextLengthExceeded) {
		t.Error("twice-wrapped should still match")
	}
}
