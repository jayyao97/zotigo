package services

import (
	"testing"
)

func TestTokenizer_Count(t *testing.T) {
	tk, err := DefaultTokenizer()
	if err != nil {
		t.Fatalf("Failed to create tokenizer: %v", err)
	}

	tests := []struct {
		name      string
		text      string
		minTokens int
		maxTokens int
	}{
		{"empty", "", 0, 0},
		{"single word", "hello", 1, 2},
		{"simple sentence", "Hello, world!", 3, 5},
		{"code snippet", "func main() { fmt.Println(\"hello\") }", 10, 20},
		{"chinese", "你好世界", 2, 6},
		{"mixed", "Hello 你好 World 世界", 4, 10},
		{"long text", "The quick brown fox jumps over the lazy dog. " +
			"This is a longer sentence to test token counting accuracy.", 20, 30},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			count := tk.Count(tc.text)
			if count < tc.minTokens || count > tc.maxTokens {
				t.Errorf("Count(%q) = %d, expected between %d and %d",
					tc.text[:min(len(tc.text), 30)], count, tc.minTokens, tc.maxTokens)
			}
		})
	}
}

func TestTokenizer_AsTokenCounter(t *testing.T) {
	tk, err := DefaultTokenizer()
	if err != nil {
		t.Fatalf("Failed to create tokenizer: %v", err)
	}

	counter := tk.AsTokenCounter()

	// Should work as TokenCounter function
	count := counter("Hello, world!")
	if count <= 0 {
		t.Error("TokenCounter should return positive count")
	}

	// Should match direct count
	direct := tk.Count("Hello, world!")
	if count != direct {
		t.Errorf("TokenCounter = %d, but direct = %d", count, direct)
	}
}

func BenchmarkTokenizer_Count(b *testing.B) {
	tk, _ := DefaultTokenizer()
	text := "The quick brown fox jumps over the lazy dog. This is a benchmark test."

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tk.Count(text)
	}
}

func BenchmarkEstimateTokens(b *testing.B) {
	text := "The quick brown fox jumps over the lazy dog. This is a benchmark test."

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		estimateTokens(text)
	}
}
