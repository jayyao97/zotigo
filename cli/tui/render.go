package tui

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/session"
)

// reBlankRun matches 3+ consecutive newlines (2+ blank lines) for compression.
var reBlankRun = regexp.MustCompile(`\n{3,}`)

func renderDisplayItem(item session.DisplayItem) (string, bool) {
	switch item.Type {
	case session.DisplayItemUserMessage:
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
				summary := part.Summary
				if summary == "" && part.ToolCall != nil {
					summary = formatDisplayToolCall(part.ToolCall)
				}
				if summary == "" {
					continue
				}
				parts = append(parts, "\n"+toolMarkerStyle.Render("⏺ ")+summary)
			case string(protocol.ContentTypeToolResult):
				rendered := renderDisplayToolResult(part)
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

func renderDisplayToolResult(part session.DisplayContentPart) string {
	text := part.Summary
	if text == "" {
		text = part.Text
	}
	if text == "" && part.ToolResult != nil {
		text = displayToolResultTextFromDisplay(part.ToolResult, 10)
	}
	if text == "" {
		return ""
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

func renderMessage(msg protocol.Message) (string, bool) {
	switch msg.Role {
	case protocol.RoleUser:
		text := msg.String()
		if text == "" {
			return "", false
		}
		return "\n" + userMarkerStyle.Render("❯ ") + text, true

	case protocol.RoleAssistant:
		var parts []string
		for _, p := range msg.Content {
			switch p.Type {
			case protocol.ContentTypeText:
				if p.Text != "" {
					parts = append(parts, "\n"+asstMarkerStyle.Render("⏺ ")+p.Text)
				}
			case protocol.ContentTypeReasoning:
				if p.Text != "" {
					parts = append(parts, "\n"+reasoningLabelStyle.Render("⏺ Thinking: ")+reasoningStyle.Render(p.Text))
				}
			case protocol.ContentTypeToolCall:
				if p.ToolCall != nil {
					parts = append(parts, "\n"+toolMarkerStyle.Render("⏺ ")+formatToolCall(p.ToolCall))
				}
			}
		}
		if len(parts) == 0 {
			return "", false
		}
		return strings.Join(parts, ""), true

	case protocol.RoleTool:
		var parts []string
		for _, p := range msg.Content {
			if p.Type == protocol.ContentTypeToolResult && p.ToolResult != nil {
				parts = append(parts, formatToolResult(p.ToolResult, 10))
			}
		}
		if len(parts) == 0 {
			return "", false
		}
		return "\n" + strings.Join(parts, "\n"), true

	default:
		return "", false
	}
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

func approvalHintForAction(action *agent.PendingAction) string {
	if action == nil || action.Name != "spawn" {
		return ""
	}
	arguments := action.Arguments
	if arguments == "" && action.ToolCall != nil {
		arguments = action.ToolCall.Arguments
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
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

// formatToolResult renders tool result lines with ⎿ prefix and indentation.
func formatToolResult(tr *protocol.ToolResult, maxLines int) string {
	if tr.Type == protocol.ToolResultTypeExecutionDenied {
		reason := strings.TrimSpace(tr.Reason)
		if reason == "" {
			reason = "permission denied"
		}
		return "  " + errorStyle.Render("⎿  Denied: "+reason)
	}

	if tr.IsError || tr.Type == protocol.ToolResultTypeErrorText || tr.Type == protocol.ToolResultTypeErrorJSON {
		errText := tr.Text
		if errText == "" {
			errText = fmt.Sprintf("%v", tr.JSON)
		}
		if len(errText) > 200 {
			errText = errText[:197] + "..."
		}
		return "  " + errorStyle.Render("⎿  Error: "+errText)
	}

	text := tr.Text
	if text == "" && tr.JSON != nil {
		// If JSON is a slice/array, join elements as lines for readability.
		switch arr := tr.JSON.(type) {
		case []any:
			var lines []string
			for _, item := range arr {
				lines = append(lines, fmt.Sprintf("%v", item))
			}
			text = strings.Join(lines, "\n")
		case []string:
			text = strings.Join(arr, "\n")
		default:
			b, _ := json.Marshal(tr.JSON)
			text = string(b)
		}
	}
	if text == "" {
		return resultStyle.Render("  ⎿  (No output)")
	}

	// Compress runs of 2+ blank lines into a single blank line for display.
	text = reBlankRun.ReplaceAllString(text, "\n\n")
	text = strings.TrimRight(text, " \t\n\r")

	// Hard cap on total characters to handle single-line mega outputs (e.g. JSON blobs).
	const maxDisplayChars = 300
	charTruncated := false
	if len(text) > maxDisplayChars {
		text = text[:maxDisplayChars]
		charTruncated = true
	}

	lines := strings.Split(text, "\n")
	totalLines := len(lines)

	if maxLines > 0 && totalLines > maxLines {
		lines = lines[:maxLines]
	}

	var sb strings.Builder
	for i, line := range lines {
		if i == 0 {
			sb.WriteString("  ⎿  " + line)
		} else {
			sb.WriteString("\n     " + line)
		}
	}

	if maxLines > 0 && totalLines > maxLines {
		sb.WriteString(fmt.Sprintf("\n     ... (%d lines total)", totalLines))
	} else if charTruncated {
		sb.WriteString("\n     ... (output truncated)")
	}

	return resultStyle.Render(sb.String())
}

func displayToolResultText(tr *protocol.ToolResult, maxLines int) string {
	if tr.Type == protocol.ToolResultTypeExecutionDenied {
		reason := strings.TrimSpace(tr.Reason)
		if reason == "" {
			reason = "permission denied"
		}
		return "Denied: " + reason
	}

	if tr.IsError || tr.Type == protocol.ToolResultTypeErrorText || tr.Type == protocol.ToolResultTypeErrorJSON {
		errText := tr.Text
		if errText == "" {
			errText = fmt.Sprintf("%v", tr.JSON)
		}
		if len(errText) > 200 {
			errText = errText[:197] + "..."
		}
		return "Error: " + errText
	}

	text := tr.Text
	if text == "" && tr.JSON != nil {
		switch arr := tr.JSON.(type) {
		case []any:
			var lines []string
			for _, item := range arr {
				lines = append(lines, fmt.Sprintf("%v", item))
			}
			text = strings.Join(lines, "\n")
		case []string:
			text = strings.Join(arr, "\n")
		default:
			b, _ := json.Marshal(tr.JSON)
			text = string(b)
		}
	}
	if text == "" {
		return "(No output)"
	}

	text = reBlankRun.ReplaceAllString(text, "\n\n")
	text = strings.TrimRight(text, " \t\n\r")

	const maxDisplayChars = 300
	charTruncated := false
	if len(text) > maxDisplayChars {
		text = text[:maxDisplayChars]
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

func displayToolResultTextFromDisplay(tr *session.DisplayToolResult, maxLines int) string {
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
		if len(errText) > 200 {
			errText = errText[:197] + "..."
		}
		return "Error: " + errText
	}

	text := tr.Text
	if text == "" && tr.JSON != nil {
		switch arr := tr.JSON.(type) {
		case []any:
			var lines []string
			for _, item := range arr {
				lines = append(lines, fmt.Sprintf("%v", item))
			}
			text = strings.Join(lines, "\n")
		case []string:
			text = strings.Join(arr, "\n")
		default:
			b, _ := json.Marshal(tr.JSON)
			text = string(b)
		}
	}
	if text == "" {
		return "(No output)"
	}

	text = reBlankRun.ReplaceAllString(text, "\n\n")
	text = strings.TrimRight(text, " \t\n\r")

	const maxDisplayChars = 300
	charTruncated := false
	if len(text) > maxDisplayChars {
		text = text[:maxDisplayChars]
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

// formatPendingActions builds the approval header string from pending tool actions.
// Includes the classifier/policy reason when available so users can see why
// approval was requested.
func formatPendingActions(actions []*agent.PendingAction) string {
	var parts []string
	for _, act := range actions {
		tc := act.ToolCall
		if tc == nil {
			tc = &protocol.ToolCall{Name: act.Name, Arguments: act.Arguments}
		}
		line := formatToolCall(tc)
		if reason := strings.TrimSpace(act.Decision.Reason); reason != "" {
			badge := ""
			switch act.Decision.Source {
			case agent.SafetyDecisionSourceClassifier:
				badge = "classifier"
			case agent.SafetyDecisionSourceHardRule:
				badge = "policy"
			}
			if act.Decision.RiskLevel != "" && act.Decision.RiskLevel != "normal" {
				badge = strings.TrimSpace(badge + " " + act.Decision.RiskLevel)
			}
			if badge != "" {
				line += fmt.Sprintf("\n  ⚠ [%s] %s", badge, reason)
			} else {
				line += fmt.Sprintf("\n  ⚠ %s", reason)
			}
			if act.Decision.RequiresSnapshot {
				line += " (snapshot will be created)"
			}
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, "\n")
}

// renderAgentBanner formats the resolved agent configuration into a
// compact, non-secret banner shown right under the welcome header.
// Fields that are unset (e.g. no classifier configured) are omitted so
// the block stays tight.
func renderAgentBanner(d agent.Description) string {
	subStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	keyStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("248"))

	var rows []string
	add := func(k, v string) {
		if strings.TrimSpace(v) == "" {
			return
		}
		rows = append(rows, "  "+keyStyle.Render(k+":")+" "+subStyle.Render(v))
	}

	add("Provider", d.Provider)
	add("Model", d.Model)
	if d.ThinkingLevel != "" {
		add("Thinking", d.ThinkingLevel)
	}
	add("Policy", string(d.ApprovalPolicy))

	switch {
	case d.ClassifierAvailable:
		cls := d.ClassifierProvider
		if d.ClassifierModel != "" {
			cls += " / " + d.ClassifierModel
		}
		if d.ReviewThreshold != "" {
			cls += " (threshold=" + d.ReviewThreshold + ")"
		}
		add("Classifier", cls)
	case d.ClassifierEnabled:
		// Enabled in config but not wired — usually means the resolver
		// failed (missing classifier profile). Surface it so the user
		// isn't surprised when approvals start firing.
		add("Classifier", "enabled but unavailable (falling back to approval)")
	default:
		add("Classifier", "off")
	}

	return strings.Join(rows, "\n")
}
