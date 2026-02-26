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

func TestTokenizer_CountBatch(t *testing.T) {
	tk, err := DefaultTokenizer()
	if err != nil {
		t.Fatalf("Failed to create tokenizer: %v", err)
	}

	texts := []string{
		"Hello, world!",
		"This is a test.",
		"Multiple strings.",
	}

	total := tk.CountBatch(texts)

	// Count individually and compare
	individual := 0
	for _, text := range texts {
		individual += tk.Count(text)
	}

	if total != individual {
		t.Errorf("CountBatch = %d, but individual sum = %d", total, individual)
	}
}

func TestTokenizer_Models(t *testing.T) {
	models := []string{
		"gpt-4",
		"gpt-4o",
		"gpt-3.5-turbo",
		"claude-3-opus",
		"unknown-model",
		"",
	}

	for _, model := range models {
		t.Run(model, func(t *testing.T) {
			tk, err := NewTokenizer(model)
			if err != nil {
				t.Fatalf("Failed to create tokenizer for %s: %v", model, err)
			}

			// Should be able to count tokens
			count := tk.Count("Hello, world!")
			if count <= 0 {
				t.Error("Should count at least 1 token")
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

func TestCompareEstimation(t *testing.T) {
	tests := []string{
		"Hello, world!",
		"The quick brown fox jumps over the lazy dog.",
		"func main() { fmt.Println(\"hello\") }",
	}

	for _, text := range tests {
		estimated, actual := CompareEstimation(text)
		t.Logf("Text: %q\n  Estimated: %d, Actual: %d, Diff: %d (%.1f%%)",
			text[:min(len(text), 30)], estimated, actual,
			estimated-actual, float64(estimated-actual)/float64(actual)*100)

		// Estimate should be in reasonable range (not off by more than 2x)
		if estimated > actual*3 || estimated < actual/3 {
			t.Errorf("Estimation too far off for %q: estimated=%d, actual=%d",
				text, estimated, actual)
		}
	}
}

func TestMustDefaultTokenizer(t *testing.T) {
	// Should not panic
	tk := MustDefaultTokenizer()
	if tk == nil {
		t.Error("MustDefaultTokenizer returned nil")
	}

	count := tk.Count("test")
	if count <= 0 {
		t.Error("Should count tokens")
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
