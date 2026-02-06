package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jayyao97/zotigo/core/executor"
)

// EditTool performs precise string replacement edits in files.
// Similar to Claude Code's Edit tool - uses old_string/new_string approach.
type EditTool struct{}

func (t *EditTool) Name() string { return "edit" }
func (t *EditTool) Description() string {
	return `Perform exact string replacements in files. Use old_string to specify the text to replace and new_string for the replacement.
The old_string must be unique in the file unless replace_all is true.
Always read the file first before editing to ensure correct content matching.`
}

func (t *EditTool) Schema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "The path to the file to edit",
			},
			"old_string": map[string]any{
				"type":        "string",
				"description": "The exact text to find and replace. Must match exactly including whitespace and indentation.",
			},
			"new_string": map[string]any{
				"type":        "string",
				"description": "The text to replace old_string with. Can be empty to delete the old_string.",
			},
			"replace_all": map[string]any{
				"type":        "boolean",
				"description": "If true, replace all occurrences. If false (default), old_string must be unique.",
			},
		},
		"required": []string{"path", "old_string", "new_string"},
	}
}

func (t *EditTool) Execute(ctx context.Context, exec executor.Executor, argsJSON string) (any, error) {
	var args struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if args.Path == "" {
		return nil, fmt.Errorf("path is required")
	}
	if args.OldString == "" {
		return nil, fmt.Errorf("old_string is required")
	}
	if args.OldString == args.NewString {
		return nil, fmt.Errorf("old_string and new_string are identical - no change needed")
	}

	// Read the file
	content, err := exec.ReadFile(ctx, args.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	originalContent := string(content)

	// Check if old_string exists
	count := strings.Count(originalContent, args.OldString)
	if count == 0 {
		// Provide helpful error with context
		return nil, fmt.Errorf("old_string not found in file. Make sure you're matching the exact content including whitespace and indentation. First 100 chars of file: %s", truncate(originalContent, 100))
	}

	// Check uniqueness unless replace_all is true
	if !args.ReplaceAll && count > 1 {
		return nil, fmt.Errorf("old_string appears %d times in the file. Either provide more context to make it unique, or set replace_all=true to replace all occurrences", count)
	}

	// Perform replacement
	var newContent string
	if args.ReplaceAll {
		newContent = strings.ReplaceAll(originalContent, args.OldString, args.NewString)
	} else {
		newContent = strings.Replace(originalContent, args.OldString, args.NewString, 1)
	}

	// Write back
	if err := exec.WriteFile(ctx, args.Path, []byte(newContent), 0644); err != nil {
		return nil, fmt.Errorf("failed to write file: %w", err)
	}

	// Return success message
	if args.ReplaceAll && count > 1 {
		return fmt.Sprintf("Successfully replaced %d occurrences in %s", count, args.Path), nil
	}
	return fmt.Sprintf("Successfully edited %s", args.Path), nil
}

// truncate returns the first n characters of s, appending "..." if truncated
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// PatchTool applies unified diff patches to files.
// This is useful for applying larger changes in diff format.
type PatchTool struct{}

func (t *PatchTool) Name() string { return "patch" }
func (t *PatchTool) Description() string {
	return `Apply a unified diff patch to modify files. The patch should be in standard unified diff format.
Use this for applying larger changes that span multiple locations in a file.`
}

func (t *PatchTool) Schema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "The path to the file to patch",
			},
			"patch": map[string]any{
				"type":        "string",
				"description": "The unified diff patch content",
			},
		},
		"required": []string{"path", "patch"},
	}
}

func (t *PatchTool) Execute(ctx context.Context, exec executor.Executor, argsJSON string) (any, error) {
	var args struct {
		Path  string `json:"path"`
		Patch string `json:"patch"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if args.Path == "" {
		return nil, fmt.Errorf("path is required")
	}
	if args.Patch == "" {
		return nil, fmt.Errorf("patch is required")
	}

	// Read original content
	content, err := exec.ReadFile(ctx, args.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	// Apply the patch
	newContent, err := applyUnifiedPatch(string(content), args.Patch)
	if err != nil {
		return nil, fmt.Errorf("failed to apply patch: %w", err)
	}

	// Write back
	if err := exec.WriteFile(ctx, args.Path, []byte(newContent), 0644); err != nil {
		return nil, fmt.Errorf("failed to write file: %w", err)
	}

	return fmt.Sprintf("Successfully applied patch to %s", args.Path), nil
}

// applyUnifiedPatch applies a unified diff patch to content.
// This is a simplified implementation that handles basic cases.
func applyUnifiedPatch(content, patch string) (string, error) {
	lines := strings.Split(content, "\n")
	patchLines := strings.Split(patch, "\n")

	var result []string
	lineIndex := 0
	patchIndex := 0

	for patchIndex < len(patchLines) {
		line := patchLines[patchIndex]

		// Skip header lines (@@ ... @@)
		if strings.HasPrefix(line, "@@") {
			// Parse hunk header: @@ -start,count +start,count @@
			var oldStart, oldCount, newStart, newCount int
			_, err := fmt.Sscanf(line, "@@ -%d,%d +%d,%d @@", &oldStart, &oldCount, &newStart, &newCount)
			if err != nil {
				// Try single line format
				_, err = fmt.Sscanf(line, "@@ -%d +%d @@", &oldStart, &newStart)
				if err != nil {
					patchIndex++
					continue
				}
				oldCount = 1
				newCount = 1
			}

			// Copy lines before the hunk
			for lineIndex < oldStart-1 && lineIndex < len(lines) {
				result = append(result, lines[lineIndex])
				lineIndex++
			}

			patchIndex++
			continue
		}

		// Skip diff headers
		if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++") ||
			strings.HasPrefix(line, "diff ") || strings.HasPrefix(line, "index ") {
			patchIndex++
			continue
		}

		// Handle patch content
		if strings.HasPrefix(line, "-") {
			// Remove line - skip it in original
			lineIndex++
			patchIndex++
		} else if strings.HasPrefix(line, "+") {
			// Add line
			result = append(result, line[1:])
			patchIndex++
		} else if strings.HasPrefix(line, " ") || line == "" {
			// Context line - copy from original
			if lineIndex < len(lines) {
				result = append(result, lines[lineIndex])
				lineIndex++
			}
			patchIndex++
		} else {
			patchIndex++
		}
	}

	// Copy remaining lines
	for lineIndex < len(lines) {
		result = append(result, lines[lineIndex])
		lineIndex++
	}

	return strings.Join(result, "\n"), nil
}
