package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/tools"
)

// GlobTool finds files matching patterns using fd or fallback find.
type GlobTool struct{}

func (t *GlobTool) Name() string { return "glob" }
func (t *GlobTool) Description() string {
	return "Find files matching a glob pattern (e.g., '**/*.go', 'src/**/*.ts'). Uses fd if available for better performance."
}

func (t *GlobTool) Schema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"description": "Glob pattern to match files (e.g., '*.go', '**/*.ts', 'src/**/*.json')",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "Directory to search in (default: current directory)",
			},
			"type": map[string]any{
				"type":        "string",
				"enum":        []string{"file", "directory", "any"},
				"description": "Type of entries to find (default: file)",
			},
			"max_depth": map[string]any{
				"type":        "integer",
				"description": "Maximum depth to search (default: unlimited)",
			},
			"max_count": map[string]any{
				"type":        "integer",
				"description": "Maximum number of results (default: 200)",
			},
		},
		"required": []string{"pattern"},
	}
}

func (t *GlobTool) Classify(call tools.SafetyCall) tools.SafetyDecision {
	if tools.IsInSafeScope(call, []string{"path"}) && !tools.IsSensitivePath(call, []string{"path"}) {
		return tools.SafetyDecision{Level: tools.LevelSafe, Reason: "glob in safe scope"}
	}
	return tools.SafetyDecision{Level: tools.LevelMedium, Reason: "glob outside safe scope or on sensitive path"}
}

func (t *GlobTool) Execute(ctx context.Context, exec executor.Executor, argsJSON string) (any, error) {
	var args struct {
		Pattern  string `json:"pattern"`
		Path     string `json:"path"`
		Type     string `json:"type"`
		MaxDepth int    `json:"max_depth"`
		MaxCount int    `json:"max_count"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if args.Pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}

	// Default values
	if args.Path == "" {
		args.Path = "."
	}
	if args.Type == "" {
		args.Type = "file"
	}
	if args.MaxCount <= 0 {
		args.MaxCount = 200
	}

	// Check if fd is available
	useFd := t.hasFd(ctx, exec)

	var cmd string
	if useFd {
		cmd = t.buildFdCommand(args.Pattern, args.Path, args.Type, args.MaxDepth, args.MaxCount)
	} else {
		cmd = t.buildFindCommand(args.Pattern, args.Path, args.Type, args.MaxDepth, args.MaxCount)
	}

	result, err := exec.Exec(ctx, cmd, executor.ExecOptions{
		Timeout: 30 * time.Second,
	})
	if err != nil && result != nil && len(result.Stdout) == 0 {
		return "No files found matching pattern", nil
	}
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}

	output := strings.TrimSpace(string(result.Stdout))
	if output == "" {
		return "No files found matching pattern", nil
	}

	// Count and truncate results
	lines := strings.Split(output, "\n")
	if len(lines) > args.MaxCount {
		output = strings.Join(lines[:args.MaxCount], "\n")
		output += fmt.Sprintf("\n\n... (truncated, showing %d of %d+ files)", args.MaxCount, len(lines))
	} else {
		output += fmt.Sprintf("\n\n(%d files found)", len(lines))
	}

	return output, nil
}

func (t *GlobTool) hasFd(ctx context.Context, exec executor.Executor) bool {
	// Try fd first, then fd-find (Debian/Ubuntu package name)
	result, err := exec.Exec(ctx, "which fd || which fdfind", executor.ExecOptions{Timeout: 2 * time.Second})
	return err == nil && result.ExitCode == 0
}

func (t *GlobTool) buildFdCommand(pattern, path, fileType string, maxDepth, maxCount int) string {
	var parts []string

	// Use fdfind on systems where fd is named differently
	parts = append(parts, "(fd || fdfind)")
	parts = append(parts, "--glob")

	switch fileType {
	case "file":
		parts = append(parts, "-t", "f")
	case "directory":
		parts = append(parts, "-t", "d")
	}

	if maxDepth > 0 {
		parts = append(parts, "-d", fmt.Sprintf("%d", maxDepth))
	}

	parts = append(parts, shellQuote(pattern))
	parts = append(parts, shellQuote(path))

	if maxCount > 0 {
		parts = append(parts, "|", "head", fmt.Sprintf("-%d", maxCount))
	}

	return strings.Join(parts, " ")
}

func (t *GlobTool) buildFindCommand(pattern, path, fileType string, maxDepth, maxCount int) string {
	var parts []string
	parts = append(parts, "find")
	parts = append(parts, shellQuote(path))

	if maxDepth > 0 {
		parts = append(parts, "-maxdepth", fmt.Sprintf("%d", maxDepth))
	}

	switch fileType {
	case "file":
		parts = append(parts, "-type", "f")
	case "directory":
		parts = append(parts, "-type", "d")
	}

	// Convert glob pattern to find -name patterns
	// Handle ** (any depth) by splitting the pattern
	if strings.Contains(pattern, "**") {
		// For **, just use the filename part
		name := filepath.Base(pattern)
		if name == "**" {
			name = "*"
		}
		parts = append(parts, "-name", shellQuote(name))
	} else {
		parts = append(parts, "-name", shellQuote(pattern))
	}

	if maxCount > 0 {
		parts = append(parts, "|", "head", fmt.Sprintf("-%d", maxCount))
	}

	return strings.Join(parts, " ")
}
