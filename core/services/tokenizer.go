package services

import (
	"sync"

	"github.com/pkoukk/tiktoken-go"
)

// Tokenizer provides accurate token counting using tiktoken.
type Tokenizer struct {
	mu       sync.RWMutex
	encoding *tiktoken.Tiktoken
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
