package providers

import (
	"errors"
	"fmt"
	"strings"
)

// ErrContextLengthExceeded is the cross-provider sentinel for "the
// prompt exceeded the model's context window". Each adapter wraps its
// native error with this so the agent's reactive-compact retry path
// branches on a single errors.Is check.
var ErrContextLengthExceeded = errors.New("provider: context length exceeded")

// WrapIfContextLength returns err wrapped with ErrContextLengthExceeded
// if the error message matches a known context-too-long pattern,
// otherwise returns err unchanged. Adapters call this in their stream
// error path so the agent doesn't need provider-specific detection.
func WrapIfContextLength(err error) error {
	if err == nil || errors.Is(err, ErrContextLengthExceeded) {
		return err
	}
	if !isContextLengthMessage(err.Error()) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrContextLengthExceeded, err)
}

// Match is case-insensitive substring; we err on false negatives (let
// the error fall through unchanged) over false positives (wrongly
// recompacting an unrelated 4xx). Sources: production observation +
// vendor docs as of 2026-04.
var contextLenFragments = []string{
	"context length exceeded",
	"context_length_exceeded",
	"maximum context length",
	"prompt is too long",
	"input length and `max_tokens` exceed",
	"input length exceeds",
	"input token count",
	"exceeds the maximum number of tokens",
	"requested tokens exceed context window",
}

func isContextLengthMessage(msg string) bool {
	if msg == "" {
		return false
	}
	lower := strings.ToLower(msg)
	for _, frag := range contextLenFragments {
		if strings.Contains(lower, frag) {
			return true
		}
	}
	return false
}
