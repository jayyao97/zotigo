package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
)

func makeToolResult(text string) *protocol.ToolResult {
	return &protocol.ToolResult{
		ToolCallID: "test",
		Text:       text,
	}
}

func TestFormatToolResult_CompressesConsecutiveBlankLines(t *testing.T) {
	tr := makeToolResult("line1\n\n\n\nline2\n\n\n\n\nline3")
	got := formatToolResult(tr, 0)

	// After compression, there should be no run of 3+ newlines in the rendered output.
	// The rendered output uses "\n     " as line separator, so consecutive blank lines
	// become lines with only whitespace. Check the source text is compressed by
	// verifying we don't see two consecutive empty indented lines.
	if strings.Contains(got, "\n     \n     \n     ") {
		t.Errorf("consecutive blank lines should be compressed, got:\n%s", got)
	}
	// Should still contain all three content lines
	if !strings.Contains(got, "line1") || !strings.Contains(got, "line2") || !strings.Contains(got, "line3") {
		t.Errorf("content lines should be preserved, got:\n%s", got)
	}
}

func TestFormatToolResult_SingleBlankLinePreserved(t *testing.T) {
	tr := makeToolResult("line1\n\nline2")
	got := formatToolResult(tr, 0)

	if !strings.Contains(got, "line1") || !strings.Contains(got, "line2") {
		t.Errorf("content lines should be preserved, got:\n%s", got)
	}
}

func TestFormatToolResult_TruncatesOverMaxLines(t *testing.T) {
	lines := make([]string, 15)
	for i := range lines {
		lines[i] = fmt.Sprintf("line%d", i+1)
	}
	input := strings.Join(lines, "\n")

	tr := makeToolResult(input)
	got := formatToolResult(tr, 10)

	// Should contain the truncation notice
	if !strings.Contains(got, "15 lines total") {
		t.Errorf("expected truncation notice with '15 lines total', got:\n%s", got)
	}
	// Should contain first line but not last
	if !strings.Contains(got, "line1") {
		t.Errorf("first line should be present, got:\n%s", got)
	}
	if strings.Contains(got, "line15") {
		t.Errorf("line15 should be truncated, got:\n%s", got)
	}
}

func TestFormatToolResult_ShortOutputUnchanged(t *testing.T) {
	tr := makeToolResult("hello world")
	got := formatToolResult(tr, 10)

	if !strings.Contains(got, "hello world") {
		t.Errorf("short output should be preserved, got:\n%s", got)
	}
	if strings.Contains(got, "lines total") {
		t.Errorf("short output should not be truncated, got:\n%s", got)
	}
}

func TestFormatToolResult_EmptyOutput(t *testing.T) {
	tr := makeToolResult("")
	got := formatToolResult(tr, 10)

	if !strings.Contains(got, "(No output)") {
		t.Errorf("empty output should show '(No output)', got:\n%s", got)
	}
}

func TestFormatToolResult_TrimsTrailingWhitespace(t *testing.T) {
	tr := makeToolResult("hello\n\n\n")
	got := formatToolResult(tr, 0)

	// After trim, trailing blank lines should be gone — only "hello" content
	if strings.Contains(got, "lines total") {
		t.Errorf("trailing whitespace should be trimmed, not trigger truncation, got:\n%s", got)
	}
	if !strings.Contains(got, "hello") {
		t.Errorf("content should be preserved, got:\n%s", got)
	}
}

func TestFormatToolResult_ExactlyAtLimit(t *testing.T) {
	lines := make([]string, 10)
	for i := range lines {
		lines[i] = fmt.Sprintf("line%d", i+1)
	}
	input := strings.Join(lines, "\n")

	tr := makeToolResult(input)
	got := formatToolResult(tr, 10)

	if strings.Contains(got, "lines total") {
		t.Errorf("output exactly at limit should not be truncated, got:\n%s", got)
	}
}

func TestFormatToolResult_CharTruncation(t *testing.T) {
	// Single-line mega output (e.g. JSON blob) exceeding 1500 chars
	longLine := strings.Repeat("x", 3000)
	tr := makeToolResult(longLine)
	got := formatToolResult(tr, 10)

	if !strings.Contains(got, "output truncated") {
		t.Errorf("expected char truncation notice, got:\n%s", got)
	}
	// The raw "x" content in the output should not exceed 1500 chars.
	// (lipgloss adds ANSI escape codes that inflate total len)
	xCount := strings.Count(got, "x")
	if xCount > 300 {
		t.Errorf("expected at most 300 x's after truncation, got %d", xCount)
	}
}

func TestFormatToolResult_CharTruncation_ShortNotAffected(t *testing.T) {
	short := strings.Repeat("y", 100)
	tr := makeToolResult(short)
	got := formatToolResult(tr, 10)

	if strings.Contains(got, "output truncated") {
		t.Errorf("short output should not trigger char truncation, got:\n%s", got)
	}
}
