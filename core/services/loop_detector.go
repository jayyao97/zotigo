package services

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
)

// LoopDetector detects when the agent is stuck in a loop,
// making repeated identical or similar tool calls without progress.
type LoopDetector struct {
	mu sync.RWMutex

	// Configuration
	maxRepeats       int // Max times the same call pattern can repeat
	windowSize       int // Number of recent calls to track
	similarityThresh float64

	// State
	recentCalls []callSignature
}

// callSignature represents a unique identifier for a tool call
type callSignature struct {
	hash     string
	toolName string
	argsHash string
}

// LoopDetectorConfig holds configuration for the loop detector
type LoopDetectorConfig struct {
	// MaxRepeats is the maximum times a call pattern can repeat before triggering
	MaxRepeats int
	// WindowSize is how many recent calls to track
	WindowSize int
}

// DefaultLoopDetectorConfig returns sensible defaults
func DefaultLoopDetectorConfig() LoopDetectorConfig {
	return LoopDetectorConfig{
		MaxRepeats: 7,
		WindowSize: 15,
	}
}

// NewLoopDetector creates a new loop detector with the given config
func NewLoopDetector(cfg LoopDetectorConfig) *LoopDetector {
	if cfg.MaxRepeats <= 0 {
		cfg.MaxRepeats = 7
	}
	if cfg.WindowSize <= 0 {
		cfg.WindowSize = 15
	}

	return &LoopDetector{
		maxRepeats:  cfg.MaxRepeats,
		windowSize:  cfg.WindowSize,
		recentCalls: make([]callSignature, 0, cfg.WindowSize),
	}
}

// LoopStatus represents the result of loop detection
type LoopStatus struct {
	// IsLooping indicates if a loop was detected
	IsLooping bool
	// RepeatCount is how many times the pattern has repeated
	RepeatCount int
	// Pattern describes the detected loop pattern
	Pattern string
	// Suggestion provides guidance on how to break the loop
	Suggestion string
}

// RecordCall records a tool call and returns the loop status.
//
// Loop detection requires the SAME (toolName, args) pair to be called
// consecutively maxRepeats times. Any intervening call with a different
// signature resets the counter — this matches how a human would judge
// a loop, and avoids flagging normal iterative workflows
// (go test → edit → go test → edit → go test) as loops just because
// the same command shows up several times in the recent history.
func (d *LoopDetector) RecordCall(toolName string, args string) LoopStatus {
	d.mu.Lock()
	defer d.mu.Unlock()

	sig := d.createSignature(toolName, args)
	d.recentCalls = append(d.recentCalls, sig)

	if len(d.recentCalls) > d.windowSize {
		d.recentCalls = d.recentCalls[1:]
	}

	// Count how many consecutive trailing entries match this signature.
	consec := 1
	for i := len(d.recentCalls) - 2; i >= 0; i-- {
		if d.recentCalls[i].hash != sig.hash {
			break
		}
		consec++
	}

	if consec >= d.maxRepeats {
		return LoopStatus{
			IsLooping:   true,
			RepeatCount: consec,
			Pattern:     fmt.Sprintf("%s (called %d times in a row)", toolName, consec),
			Suggestion:  d.getSuggestion(toolName, consec),
		}
	}
	return LoopStatus{
		IsLooping:   false,
		RepeatCount: consec,
	}
}

// CheckPatternLoop checks if there's a repeating pattern of multiple calls
func (d *LoopDetector) CheckPatternLoop(patternSize int) LoopStatus {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if len(d.recentCalls) < patternSize*2 {
		return LoopStatus{IsLooping: false}
	}

	// Check if the last N calls repeat a pattern
	calls := d.recentCalls
	n := len(calls)

	// Look for repeating patterns of size patternSize
	for size := 2; size <= patternSize && size*2 <= n; size++ {
		pattern1 := calls[n-size*2 : n-size]
		pattern2 := calls[n-size:]

		if d.patternsMatch(pattern1, pattern2) {
			// Count how many times this pattern appears
			repeatCount := 2
			for i := n - size*3; i >= 0 && i+size <= n-size*2; i -= size {
				if d.patternsMatch(calls[i:i+size], pattern1) {
					repeatCount++
				} else {
					break
				}
			}

			if repeatCount >= d.maxRepeats {
				return LoopStatus{
					IsLooping:   true,
					RepeatCount: repeatCount,
					Pattern:     fmt.Sprintf("Pattern of %d calls repeated %d times", size, repeatCount),
					Suggestion:  "The agent is repeating a sequence of tool calls. Consider changing approach or asking for clarification.",
				}
			}
		}
	}

	return LoopStatus{IsLooping: false}
}

// Reset clears all recorded calls
func (d *LoopDetector) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.recentCalls = make([]callSignature, 0, d.windowSize)
}

// createSignature creates a unique signature for a tool call
func (d *LoopDetector) createSignature(toolName string, args string) callSignature {
	// Hash the combination of tool name and arguments
	combined := toolName + ":" + args
	hash := sha256.Sum256([]byte(combined))
	hashStr := hex.EncodeToString(hash[:8]) // Use first 8 bytes

	argsHash := sha256.Sum256([]byte(args))
	argsHashStr := hex.EncodeToString(argsHash[:8])

	return callSignature{
		hash:     hashStr,
		toolName: toolName,
		argsHash: argsHashStr,
	}
}

// patternsMatch checks if two patterns of calls are identical
func (d *LoopDetector) patternsMatch(p1, p2 []callSignature) bool {
	if len(p1) != len(p2) {
		return false
	}
	for i := range p1 {
		if p1[i].hash != p2[i].hash {
			return false
		}
	}
	return true
}

// getSuggestion returns a suggestion based on the tool and repeat count
func (d *LoopDetector) getSuggestion(toolName string, count int) string {
	switch toolName {
	case "read_file":
		return "The agent is repeatedly reading the same file. This might indicate it's not processing the content correctly or needs different information."
	case "shell", "bash":
		return "The agent is repeatedly running the same command. Check if the command is failing or if different parameters are needed."
	case "grep", "search":
		return "The agent is repeating search operations. Consider if the search terms need adjustment or if the information sought doesn't exist."
	case "write_file", "edit":
		return "The agent is repeatedly modifying files. This might indicate conflicts or issues with the changes being made."
	default:
		return fmt.Sprintf("Tool '%s' has been called %d times with identical parameters. Consider a different approach.", toolName, count)
	}
}

// Stats returns a histogram of call signatures currently in the sliding
// window, keyed by signature hash.
func (d *LoopDetector) Stats() map[string]int {
	d.mu.RLock()
	defer d.mu.RUnlock()

	stats := make(map[string]int, len(d.recentCalls))
	for _, c := range d.recentCalls {
		stats[c.hash]++
	}
	return stats
}
