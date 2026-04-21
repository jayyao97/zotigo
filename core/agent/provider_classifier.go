package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/debug"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

// ProviderSafetyClassifier is an LLM-backed SafetyClassifier.
// The classifier is asked to invoke a record_decision tool rather than
// return free-form JSON; the provider's schema validation gives us a
// typed decision enum and required fields with no regex parsing.
type ProviderSafetyClassifier struct {
	provider  providers.Provider
	timeout   time.Duration
	maxRecent int
	maxChars  int
}

// NewProviderSafetyClassifier creates a classifier backed by the given provider
// and classifier configuration.
func NewProviderSafetyClassifier(p providers.Provider, cfg config.SafetyClassifierConfig) *ProviderSafetyClassifier {
	timeout := time.Duration(cfg.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		// Real-world classifier calls routinely take 10–30s even with
		// reasoning=low on a small model — a 3s default silently kills
		// the call and forces fallback to user approval, which
		// surprises operators who didn't set an explicit timeout.
		timeout = 30 * time.Second
	}
	maxRecent := cfg.MaxRecentActions
	if maxRecent <= 0 {
		maxRecent = 6
	}
	maxChars := cfg.MaxAuditContextChars
	if maxChars <= 0 {
		maxChars = 1200
	}
	return &ProviderSafetyClassifier{
		provider:  p,
		timeout:   timeout,
		maxRecent: maxRecent,
		maxChars:  maxChars,
	}
}

const classifierToolName = "record_decision"

const classifierSystemPrompt = `You are a strict safety reviewer for an autonomous coding agent.

Your job: for one pending tool call, decide whether it is safe to run without further user approval. You MUST respond by invoking the record_decision tool — do not write prose.

Rules:
- "allow" only for clearly benign actions whose scope is understood from the user's prompt.
- "deny" for actions that are destructive beyond the user's intent, violate obvious safety rules, or try to exfiltrate data.
- "ask_user" for anything ambiguous, powerful, or irreversible where the user should confirm.
- Prefer "ask_user" when in doubt.
- requires_snapshot should be true for mutating actions on code/files that could benefit from a rollback point.

The reason field is shown to the user in the approval UI. Keep it to one short sentence.`

// classifierDecisionTool is a tool-shaped stub passed to the provider so
// the classifier response is a structured tool call. Execute/Classify are
// never invoked — the agent only reads the tool call's Arguments.
type classifierDecisionTool struct{}

func (classifierDecisionTool) Name() string { return classifierToolName }
func (classifierDecisionTool) Description() string {
	return "Record your safety decision for the pending tool call. Always invoke this tool; never reply with prose."
}

func (classifierDecisionTool) Schema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"decision": map[string]any{
				"type":        "string",
				"enum":        []string{"allow", "deny", "ask_user"},
				"description": "allow = run without approval; deny = hard refuse; ask_user = user must confirm.",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "One short sentence shown in the approval UI.",
			},
			"requires_snapshot": map[string]any{
				"type":        "boolean",
				"description": "True for mutating actions that would benefit from a rollback point.",
			},
		},
		"required": []string{"decision", "reason", "requires_snapshot"},
	}
}

func (classifierDecisionTool) Execute(context.Context, executor.Executor, string) (any, error) {
	return nil, fmt.Errorf("classifier decision tool is not executable")
}

func (classifierDecisionTool) Classify(tools.SafetyCall) tools.SafetyDecision {
	return tools.SafetyDecision{Level: tools.LevelBlocked}
}

// Classify implements SafetyClassifier.
func (c *ProviderSafetyClassifier) Classify(req SafetyClassifierRequest) (SafetyClassifierResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	userPrompt := c.buildUserPrompt(req)
	start := time.Now()
	debug.Logf(
		"classifier start provider=%s tool=%s risk=%s prompt_chars=%d args_chars=%d",
		c.provider.Name(),
		req.ToolName,
		req.RiskLevel,
		len(userPrompt),
		len(req.ToolArguments),
	)

	msgs := []protocol.Message{
		protocol.NewSystemMessage(classifierSystemPrompt),
		{
			Role:      protocol.RoleUser,
			Content:   []protocol.ContentPart{{Type: protocol.ContentTypeText, Text: userPrompt}},
			CreatedAt: time.Now(),
		},
	}

	stream, err := c.provider.StreamChat(ctx, msgs, []tools.Tool{classifierDecisionTool{}},
		providers.WithToolChoiceTool(classifierToolName),
		providers.WithReasoningEffort("low"),
	)
	if err != nil {
		debug.Logf("classifier error provider=%s tool=%s duration=%s err=%v", c.provider.Name(), req.ToolName, time.Since(start).Round(time.Millisecond), err)
		return SafetyClassifierResponse{}, fmt.Errorf("classifier stream: %w", err)
	}

	var textBuf strings.Builder
	var toolArgs string
	for evt := range stream {
		switch evt.Type {
		case protocol.EventTypeToolCallEnd:
			if evt.ToolCall != nil && evt.ToolCall.Name == classifierToolName {
				toolArgs = evt.ToolCall.Arguments
			}
		case protocol.EventTypeContentDelta:
			if evt.ContentPartDelta != nil && evt.ContentPartDelta.Type != protocol.ContentTypeReasoning {
				textBuf.WriteString(evt.ContentPartDelta.Text)
			}
		case protocol.EventTypeError:
			if evt.Error != nil {
				debug.Logf("classifier stream error provider=%s tool=%s duration=%s err=%v", c.provider.Name(), req.ToolName, time.Since(start).Round(time.Millisecond), evt.Error)
				return SafetyClassifierResponse{}, evt.Error
			}
		}
	}

	var (
		resp   SafetyClassifierResponse
		source string
	)
	switch {
	case toolArgs != "":
		resp, err = parseClassifierDecisionArgs(toolArgs)
		source = "tool_call"
	case strings.TrimSpace(textBuf.String()) != "":
		// Model ignored the tool and replied in prose — best-effort parse.
		resp, err = parseClassifierResponse(textBuf.String())
		source = "text_fallback"
	default:
		return SafetyClassifierResponse{}, fmt.Errorf("classifier returned empty response")
	}
	if err != nil {
		debug.Logf("classifier parse error provider=%s tool=%s source=%s duration=%s err=%v", c.provider.Name(), req.ToolName, source, time.Since(start).Round(time.Millisecond), err)
		return SafetyClassifierResponse{}, err
	}

	debug.Logf(
		"classifier end provider=%s tool=%s source=%s duration=%s decision=%s snapshot=%t",
		c.provider.Name(),
		req.ToolName,
		source,
		time.Since(start).Round(time.Millisecond),
		resp.Decision,
		resp.RequiresSnapshot,
	)
	return resp, nil
}

// buildUserPrompt assembles a bounded request body for the classifier.
func (c *ProviderSafetyClassifier) buildUserPrompt(req SafetyClassifierRequest) string {
	var sb strings.Builder

	sb.WriteString("Pending tool call:\n")
	fmt.Fprintf(&sb, "- tool: %s\n", req.ToolName)
	fmt.Fprintf(&sb, "- risk_level: %s\n", req.RiskLevel)
	fmt.Fprintf(&sb, "- is_git_repo: %t\n", req.IsGitRepo)
	fmt.Fprintf(&sb, "- has_snapshot: %t\n", req.HasSnapshot)
	sb.WriteString("- arguments: ")
	sb.WriteString(truncate(req.ToolArguments, c.maxChars))
	sb.WriteString("\n\n")

	sb.WriteString("User prompt (what the user asked for):\n")
	sb.WriteString(truncate(req.UserPrompt, c.maxChars))
	sb.WriteString("\n\n")

	recent := req.RecentActions
	if len(recent) > c.maxRecent {
		recent = recent[len(recent)-c.maxRecent:]
	}
	if len(recent) > 0 {
		sb.WriteString("Recent actions:\n")
		for _, a := range recent {
			status := "ok"
			if a.IsError {
				status = "error"
			}
			fmt.Fprintf(&sb, "- [%s] %s: %s\n", status, a.ToolName, truncate(a.Result, 160))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("Invoke the record_decision tool now.")
	return sb.String()
}

// parseClassifierDecisionArgs parses the structured tool-call arguments.
// No regex, no markdown stripping — the provider has already validated
// that this is a JSON object matching the record_decision schema.
func parseClassifierDecisionArgs(args string) (SafetyClassifierResponse, error) {
	var parsed struct {
		Decision         string `json:"decision"`
		Reason           string `json:"reason"`
		RequiresSnapshot bool   `json:"requires_snapshot"`
	}
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		return SafetyClassifierResponse{}, fmt.Errorf("classifier tool arguments not valid JSON: %w", err)
	}
	decision := SafetyClassifierDecision(strings.ToLower(strings.TrimSpace(parsed.Decision)))
	switch decision {
	case SafetyClassifierDecisionAllow, SafetyClassifierDecisionDeny, SafetyClassifierDecisionAskUser:
	default:
		return SafetyClassifierResponse{}, fmt.Errorf("classifier returned unknown decision %q", parsed.Decision)
	}
	return SafetyClassifierResponse{
		Decision:         decision,
		Reason:           strings.TrimSpace(parsed.Reason),
		RequiresSnapshot: parsed.RequiresSnapshot,
	}, nil
}

var jsonObjectRe = regexp.MustCompile(`(?s)\{.*\}`)

// parseClassifierResponse is the legacy free-text parser kept as a
// fallback for providers/models that refuse to invoke the tool.
func parseClassifierResponse(raw string) (SafetyClassifierResponse, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	if !strings.HasPrefix(raw, "{") {
		if m := jsonObjectRe.FindString(raw); m != "" {
			raw = m
		}
	}

	return parseClassifierDecisionArgs(raw)
}

func truncate(s string, limit int) string {
	s = strings.TrimSpace(s)
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
