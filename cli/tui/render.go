package tui

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigodclient"
)

// reBlankRun matches 3+ consecutive newlines (2+ blank lines) for compression.
var reBlankRun = regexp.MustCompile(`\n{3,}`)

const (
	defaultToolResultMaxLines = 10
	maxToolResultDisplayChars = 300
	maxToolResultErrorChars   = 200
)

func renderDisplayItem(item session.DisplayItem) (string, bool) {
	switch item.Type {
	case session.DisplayItemUserMessage, session.DisplayItemSteeringMessage:
		text := displayText(item.Content)
		if text == "" {
			return "", false
		}
		return "\n" + userMarkerStyle.Render("❯ ") + text, true

	case session.DisplayItemAssistantMessage:
		var parts []string
		for _, part := range item.Content {
			switch part.Type {
			case string(protocol.ContentTypeText):
				if part.Text == "" {
					continue
				}
				parts = append(parts, "\n"+asstMarkerStyle.Render("⏺ ")+part.Text)
			case string(protocol.ContentTypeReasoning):
				if part.Text == "" {
					continue
				}
				parts = append(parts, "\n"+reasoningLabelStyle.Render("⏺ Thinking: ")+reasoningStyle.Render(part.Text))
			case string(protocol.ContentTypeToolCall):
				summary := formatDisplayToolCall(part.ToolCall)
				if summary == "" {
					continue
				}
				parts = append(parts, "\n"+toolMarkerStyle.Render("⏺ ")+summary)
			case string(protocol.ContentTypeToolResult):
				rendered := renderDisplayToolResult(part.ToolResult)
				if rendered == "" {
					continue
				}
				parts = append(parts, "\n"+rendered)
			}
		}
		if len(parts) == 0 {
			return "", false
		}
		return strings.Join(parts, ""), true

	case session.DisplayItemError:
		if item.Error == "" {
			return "", false
		}
		return "\n" + errorStyle.Render("✗ ") + "Error: " + item.Error, true

	case session.DisplayItemContextCompacted:
		return "\n" + headerStyle.Render("── Context compacted ──"), true

	default:
		return "", false
	}
}

func renderDisplayToolResult(result *session.DisplayToolResult) string {
	if result == nil {
		return ""
	}
	return renderToolResultFromDisplay(result, defaultToolResultMaxLines)
}

// primaryArgKey maps tool names to the single most informative argument.
var primaryArgKey = map[string]string{
	"shell":           "command",
	"bash":            "command",
	"execute_command": "command",
	"read_file":       "path",
	"write_file":      "path",
	"edit_file":       "path",
	"create_file":     "path",
	"delete_file":     "path",
	"list_files":      "path",
	"search_files":    "pattern",
	"search":          "query",
	"web_search":      "query",
	"grep":            "pattern",
	"find":            "pattern",
}

// toPascalCase converts a snake_case name to PascalCase, e.g. "read_file" → "ReadFile".
func toPascalCase(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

// formatDuration formats a duration for the timing footer.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm %ds", m, s)
}

// formatToolCall returns a compact summary like "Shell(git status)" or "ReadFile(path=src/main.go)".
func formatToolCall(tc *protocol.ToolCall) string {
	name := toPascalCase(tc.Name)

	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil || len(args) == 0 {
		return name + "()"
	}

	if tc.Name == "spawn" {
		return formatSpawnToolCall(name, args)
	}

	// Try the known primary arg first.
	if key, ok := primaryArgKey[tc.Name]; ok {
		if v, found := args[key]; found {
			s := truncateToolArg(fmt.Sprintf("%v", v))
			return fmt.Sprintf("%s(%s)", name, s)
		}
	}

	// Fallback: pick the first non-large arg.
	for k, v := range args {
		s := fmt.Sprintf("%v", v)
		if len(s) > 200 {
			continue // skip large values
		}
		s = truncateToolArg(s)
		return fmt.Sprintf("%s(%s=%s)", name, k, s)
	}

	return name + "(...)"
}

func formatSpawnToolCall(name string, args map[string]any) string {
	var parts []string
	if v, ok := args["name"]; ok {
		parts = append(parts, "name="+truncateToolArg(fmt.Sprintf("%v", v)))
	}
	if v, ok := args["agent_type"]; ok {
		parts = append(parts, "agent_type="+truncateToolArg(fmt.Sprintf("%v", v)))
	}
	if v, ok := args["workdir"]; ok {
		parts = append(parts, "workdir="+truncateToolArg(fmt.Sprintf("%v", v)))
	}
	if len(parts) == 0 {
		if v, ok := args["description"]; ok {
			parts = append(parts, "description="+truncateToolArg(fmt.Sprintf("%v", v)))
		}
	}
	if len(parts) == 0 {
		return name + "(...)"
	}
	return fmt.Sprintf("%s(%s)", name, strings.Join(parts, ", "))
}

func approvalHintForAction(action *session.DisplayPendingApproval) string {
	if action == nil || action.ToolName != "spawn" {
		return ""
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(action.Arguments), &args); err != nil {
		return ""
	}
	agentType := fmt.Sprintf("%v", args["agent_type"])
	if agentType == "explore" {
		return ""
	}
	return "child agent may run write/shell tools inside its workdir without further prompts"
}

func denyLabelForApprovalCount(count int) string {
	if count > 1 {
		return "Deny batch"
	}
	return "Deny"
}

func truncateToolArg(s string) string {
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
}

func renderToolResultFromDisplay(tr *session.DisplayToolResult, maxLines int) string {
	text := toolResultTextFromDisplay(tr, maxLines)
	if text == "" {
		return ""
	}
	return renderToolResultText(text, toolResultIsError(tr))
}

func toolResultTextFromDisplay(tr *session.DisplayToolResult, maxLines int) string {
	if tr == nil {
		return ""
	}
	if tr.ResultType == string(protocol.ToolResultTypeExecutionDenied) {
		reason := strings.TrimSpace(tr.Reason)
		if reason == "" {
			reason = "permission denied"
		}
		return "Denied: " + reason
	}

	if tr.IsError || tr.ResultType == string(protocol.ToolResultTypeErrorText) || tr.ResultType == string(protocol.ToolResultTypeErrorJSON) {
		errText := tr.Text
		if errText == "" {
			errText = fmt.Sprintf("%v", tr.JSON)
		}
		if len(errText) > maxToolResultErrorChars {
			errText = errText[:maxToolResultErrorChars-3] + "..."
		}
		return "Error: " + errText
	}

	text := toolResultOutputText(tr.Text, tr.JSON)
	if text == "" {
		return "(No output)"
	}

	text = reBlankRun.ReplaceAllString(text, "\n\n")
	text = strings.TrimRight(text, " \t\n\r")

	charTruncated := false
	if len(text) > maxToolResultDisplayChars {
		text = text[:maxToolResultDisplayChars]
		charTruncated = true
	}

	lines := strings.Split(text, "\n")
	totalLines := len(lines)
	if maxLines > 0 && totalLines > maxLines {
		lines = lines[:maxLines]
	}

	result := strings.Join(lines, "\n")
	if maxLines > 0 && totalLines > maxLines {
		result += fmt.Sprintf("\n... (%d lines total)", totalLines)
	} else if charTruncated {
		result += "\n... (output truncated)"
	}
	return result
}

func toolResultIsError(tr *session.DisplayToolResult) bool {
	if tr == nil {
		return false
	}
	return tr.ResultType == string(protocol.ToolResultTypeExecutionDenied) ||
		tr.IsError ||
		tr.ResultType == string(protocol.ToolResultTypeErrorText) ||
		tr.ResultType == string(protocol.ToolResultTypeErrorJSON)
}

func renderToolResultText(text string, isError bool) string {
	if text == "" {
		return ""
	}
	if isError {
		return "  " + errorStyle.Render("⎿  "+text)
	}

	lines := strings.Split(text, "\n")
	var sb strings.Builder
	for idx, line := range lines {
		if idx == 0 {
			sb.WriteString("  ⎿  " + line)
		} else {
			sb.WriteString("\n     " + line)
		}
	}
	return resultStyle.Render(sb.String())
}

func toolResultOutputText(text string, jsonValue any) string {
	if text != "" {
		return text
	}
	if jsonValue == nil {
		return ""
	}
	switch arr := jsonValue.(type) {
	case []any:
		var lines []string
		for _, item := range arr {
			lines = append(lines, fmt.Sprintf("%v", item))
		}
		return strings.Join(lines, "\n")
	case []string:
		return strings.Join(arr, "\n")
	default:
		b, _ := json.Marshal(jsonValue)
		return string(b)
	}
}

// formatPendingApprovals builds the approval header from the service's
// pending tool calls, including the reason the call needs approval.
func formatPendingApprovals(pending []session.DisplayPendingApproval) string {
	parts := make([]string, 0, len(pending))
	for _, action := range pending {
		line := formatToolCall(&protocol.ToolCall{ID: action.ToolCallID, Name: action.ToolName, Arguments: action.Arguments})
		if strings.HasPrefix(action.Source, "subagent:") {
			line = "[" + strings.TrimPrefix(action.Source, "subagent:") + "] " + line
		}
		reason := strings.TrimSpace(action.Reason)
		if reason == "" {
			reason = strings.TrimSpace(action.Description)
		}
		if reason != "" {
			if action.RiskLevel != "" && action.RiskLevel != "normal" {
				line += fmt.Sprintf("\n  ⚠ [%s] %s", action.RiskLevel, reason)
			} else {
				line += fmt.Sprintf("\n  ⚠ %s", reason)
			}
			if action.RequiresSnapshot {
				line += " (snapshot will be created)"
			}
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, "\n")
}

// renderSessionBanner formats the non-secret session facts shown under the
// welcome header: where it runs, who owns it and how it is configured.
func renderSessionBanner(sess zotigodclient.Session, location string, embedded bool) string {
	subStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	keyStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("248"))

	var rows []string
	add := func(k, v string) {
		if strings.TrimSpace(v) == "" {
			return
		}
		rows = append(rows, "  "+keyStyle.Render(k+":")+" "+subStyle.Render(v))
	}
	add("Session", sess.ID)
	add("Location", location)
	add("Directory", sess.WorkingDirectory)
	add("Profile", sess.Profile)
	add("Policy", approvalPolicyLabel(sess.ApprovalPolicy))
	if embedded {
		add("Service", "in-process (--no-daemon)")
	} else {
		add("Service", "zotigod")
	}
	return strings.Join(rows, "\n")
}
