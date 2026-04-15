package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
)

// ProviderSafetyClassifier is an LLM-backed SafetyClassifier.
// It sends a bounded prompt to a provider and parses a structured JSON decision.
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
		timeout = 3 * time.Second
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

const classifierSystemPrompt = `You are a strict safety reviewer for an autonomous coding agent.

Your job: for one pending tool call, decide whether it is safe to run without further user approval.

You must respond with a single JSON object and NOTHING else. Do not wrap it in Markdown fences.

Schema:
{
  "decision": "allow" | "deny" | "ask_user",
  "reason":   "<one short sentence>",
  "requires_snapshot": true | false
}

Rules:
- "allow" only for clearly benign actions whose scope is understood from the user's prompt.
- "deny" for actions that are destructive beyond the user's intent, violate obvious safety rules, or try to exfiltrate data.
- "ask_user" for anything ambiguous, powerful, or irreversible where the user should confirm.
- Prefer "ask_user" when in doubt.
- "requires_snapshot" should be true for mutating actions on code/files that could benefit from a rollback point.

Be terse. The "reason" will be shown to the user in the approval UI.`

// Classify implements SafetyClassifier.
func (c *ProviderSafetyClassifier) Classify(req SafetyClassifierRequest) (SafetyClassifierResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	userPrompt := c.buildUserPrompt(req)

	msgs := []protocol.Message{
		protocol.NewSystemMessage(classifierSystemPrompt),
		{
			Role:      protocol.RoleUser,
			Content:   []protocol.ContentPart{{Type: protocol.ContentTypeText, Text: userPrompt}},
			CreatedAt: time.Now(),
		},
	}

	stream, err := c.provider.StreamChat(ctx, msgs, nil)
	if err != nil {
		return SafetyClassifierResponse{}, fmt.Errorf("classifier stream: %w", err)
	}

	var sb strings.Builder
	for evt := range stream {
		if evt.Type == protocol.EventTypeContentDelta && evt.ContentPartDelta != nil {
			if evt.ContentPartDelta.Type != protocol.ContentTypeReasoning {
				sb.WriteString(evt.ContentPartDelta.Text)
			}
		}
		if evt.Type == protocol.EventTypeError && evt.Error != nil {
			return SafetyClassifierResponse{}, evt.Error
		}
	}

	raw := strings.TrimSpace(sb.String())
	if raw == "" {
		return SafetyClassifierResponse{}, fmt.Errorf("classifier returned empty response")
	}

	return parseClassifierResponse(raw)
}

// buildUserPrompt assembles a bounded request body for the classifier.
func (c *ProviderSafetyClassifier) buildUserPrompt(req SafetyClassifierRequest) string {
	var sb strings.Builder

	sb.WriteString("Pending tool call:\n")
	sb.WriteString(fmt.Sprintf("- tool: %s\n", req.ToolName))
	sb.WriteString(fmt.Sprintf("- risk_level: %s\n", req.RiskLevel))
	sb.WriteString(fmt.Sprintf("- is_git_repo: %t\n", req.IsGitRepo))
	sb.WriteString(fmt.Sprintf("- has_snapshot: %t\n", req.HasSnapshot))
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
			sb.WriteString(fmt.Sprintf("- [%s] %s: %s\n", status, a.ToolName, truncate(a.Result, 160)))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("Return the JSON decision now.")
	return sb.String()
}

var jsonObjectRe = regexp.MustCompile(`(?s)\{.*\}`)

// parseClassifierResponse extracts a JSON decision from the classifier output,
// tolerating minor formatting issues (e.g. Markdown fences).
func parseClassifierResponse(raw string) (SafetyClassifierResponse, error) {
	// Strip common wrapping
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	// Fall back to regex extraction if the response has prefixes/suffixes.
	if !strings.HasPrefix(raw, "{") {
		if m := jsonObjectRe.FindString(raw); m != "" {
			raw = m
		}
	}

	var parsed struct {
		Decision         string `json:"decision"`
		Reason           string `json:"reason"`
		RequiresSnapshot bool   `json:"requires_snapshot"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return SafetyClassifierResponse{}, fmt.Errorf("classifier response not valid JSON: %w", err)
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

func truncate(s string, limit int) string {
	s = strings.TrimSpace(s)
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
