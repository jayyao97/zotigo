package tools

import (
	"context"

	"github.com/jayyao97/zotigo/core/executor"
)

// SafetyLevel is an ordinal severity the tool assigns to a specific call.
// Think of it like a log level: the higher the number, the more scrutiny
// the agent should apply. The dispatcher compares this against a single
// configured threshold to decide between auto-execute, review, or block.
type SafetyLevel int

const (
	// LevelSafe: read-only action in a trusted location. Runs without
	// prompting under any approval policy.
	LevelSafe SafetyLevel = iota
	// LevelLow: routine mutation inside the working directory
	// (acceptEdits-style writes). Auto-runs under Auto mode by default;
	// Manual mode still prompts.
	LevelLow
	// LevelMedium: concerning mutation — write outside the working
	// directory, write to a sensitive path, ambiguous shell command.
	// Triggers the classifier under the default threshold.
	LevelMedium
	// LevelHigh: known-dangerous action — sandbox flagged the command as
	// high-risk, or the tool otherwise has strong reason for concern.
	LevelHigh
	// LevelBlocked: hard refuse; never execute under any policy.
	LevelBlocked
)

// String returns the canonical risk label for a SafetyLevel. These
// strings are forwarded to the classifier request and surfaced in audit
// events, so they intentionally match the values produced by
// sandbox.RiskLevel.String() for the levels they overlap with.
func (l SafetyLevel) String() string {
	switch l {
	case LevelSafe:
		return "safe"
	case LevelLow:
		return "low"
	case LevelMedium:
		return "medium"
	case LevelHigh:
		return "high"
	case LevelBlocked:
		return "blocked"
	default:
		return "unknown"
	}
}

// ParseSafetyLevel converts a config string (safe/low/medium/high/off)
// into a SafetyLevel threshold. "off" is represented as LevelBlocked+1
// so that no non-blocked action ever meets the threshold. The empty
// string and unknown values default to LevelMedium.
func ParseSafetyLevel(s string) SafetyLevel {
	switch s {
	case "safe":
		return LevelSafe
	case "low":
		return LevelLow
	case "medium", "":
		return LevelMedium
	case "high":
		return LevelHigh
	case "off", "none":
		return LevelBlocked + 1
	default:
		return LevelMedium
	}
}

// SafetyCall is the per-invocation context passed to Tool.Classify. It
// exposes just enough for the tool to make a safety decision without
// coupling to the agent's internals.
type SafetyCall struct {
	// Arguments is the raw JSON argument string the LLM provided.
	Arguments string
	// WorkDir is the agent's working directory — the canonical "project root".
	WorkDir string
	// SafeDirs is the full set of directories the agent treats as safe
	// (working directory + any extras, such as skills). A superset of WorkDir.
	SafeDirs []string
	// Executor gives tools access to capabilities like sandbox CheckCommand
	// when they need to (e.g. the shell tool).
	Executor executor.Executor
}

// SafetyDecision is the tool's per-call classification.
type SafetyDecision struct {
	Level            SafetyLevel
	Reason           string
	RequiresSnapshot bool
}

// Tool defines an executable capability available to the agent.
type Tool interface {
	// Name returns the unique name of the tool (e.g., "read_file").
	Name() string

	// Description returns a short description of what the tool does.
	Description() string

	// Schema returns the JSON schema definition for the tool's arguments.
	// This is used to inform the LLM about how to call the tool.
	// It should return a struct or map compatible with JSON marshaling.
	Schema() any

	// Execute runs the tool with the provided arguments (as a JSON string).
	// The executor parameter provides access to file operations and command execution.
	// It returns the result (which will be serialized to JSON/Text) or an error.
	Execute(ctx context.Context, exec executor.Executor, argsJSON string) (any, error)

	// Classify decides how this specific invocation should be handled.
	// Tools own this judgment; common helpers (IsInWorkDir, IsInSafeScope,
	// IsSensitivePath, etc.) are available in this package so tools share
	// logic without the agent needing to know each tool's semantics.
	Classify(call SafetyCall) SafetyDecision
}
