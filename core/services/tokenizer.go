package services

import (
	"sync"

	"github.com/pkoukk/tiktoken-go"
)

// Tokenizer provides accurate token counting using tiktoken.
type Tokenizer struct {
	mu       sync.RWMutex
	encoding *tiktoken.Tiktoken
	model    string
}

// Model encoding mappings
var modelEncodings = map[string]string{
	// OpenAI models
	"gpt-4":              "cl100k_base",
	"gpt-4-turbo":        "cl100k_base",
	"gpt-4o":             "o200k_base",
	"gpt-4o-mini":        "o200k_base",
	"gpt-3.5-turbo":      "cl100k_base",
	"text-embedding-ada": "cl100k_base",

	// Claude models (use cl100k_base as approximation)
	"claude-3-opus":   "cl100k_base",
	"claude-3-sonnet": "cl100k_base",
	"claude-3-haiku":  "cl100k_base",
	"claude-3.5":      "cl100k_base",

	// Default
	"default": "cl100k_base",
}

// NewTokenizer creates a new tokenizer for the given model.
// If model is empty or unknown, uses cl100k_base encoding.
func NewTokenizer(model string) (*Tokenizer, error) {
	encoding := "cl100k_base"
	if enc, ok := modelEncodings[model]; ok {
		encoding = enc
	}

	tk, err := tiktoken.GetEncoding(encoding)
	if err != nil {
		return nil, err
	}

	return &Tokenizer{
		encoding: tk,
		model:    model,
	}, nil
}

// NewTokenizerWithEncoding creates a tokenizer with a specific encoding.
func NewTokenizerWithEncoding(encoding string) (*Tokenizer, error) {
	tk, err := tiktoken.GetEncoding(encoding)
	if err != nil {
		return nil, err
	}

	return &Tokenizer{
		encoding: tk,
	}, nil
}

// Count returns the exact token count for the given text.
func (t *Tokenizer) Count(text string) int {
	if text == "" {
		return 0
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	tokens := t.encoding.Encode(text, nil, nil)
	return len(tokens)
}

// CountBatch counts tokens for multiple strings efficiently.
func (t *Tokenizer) CountBatch(texts []string) int {
	t.mu.RLock()
	defer t.mu.RUnlock()

	total := 0
	for _, text := range texts {
		if text != "" {
			tokens := t.encoding.Encode(text, nil, nil)
			total += len(tokens)
		}
	}
	return total
}

// AsTokenCounter returns a TokenCounter function for use with Compressor.
func (t *Tokenizer) AsTokenCounter() TokenCounter {
	return func(text string) int {
		return t.Count(text)
	}
}

// DefaultTokenizer returns a tokenizer using cl100k_base encoding.
// This is suitable for GPT-4, GPT-3.5-turbo, and Claude models.
func DefaultTokenizer() (*Tokenizer, error) {
	return NewTokenizerWithEncoding("cl100k_base")
}

// MustDefaultTokenizer returns the default tokenizer or panics.
// Use this only in init() or when you're certain the encoding is available.
func MustDefaultTokenizer() *Tokenizer {
	tk, err := DefaultTokenizer()
	if err != nil {
		panic("failed to create default tokenizer: " + err.Error())
	}
	return tk
}

// CompareEstimation compares the estimate vs actual token count.
// Useful for debugging and testing.
func CompareEstimation(text string) (estimated, actual int) {
	estimated = estimateTokens(text)

	tk, err := DefaultTokenizer()
	if err != nil {
		return estimated, estimated // Fallback
	}
	actual = tk.Count(text)
	return
}
