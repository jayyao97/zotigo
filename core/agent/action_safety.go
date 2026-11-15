package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/tools"
)

// classifyToolCall decides how a tool call should be handled.
//
// Each tool assigns an ordinal SafetyLevel to the call. The agent
// compares that against a single configurable threshold:
//
//	Level         | Manual mode       | Auto mode
//	LevelSafe     | auto_execute      | auto_execute
//	LevelLow      | require_approval  | auto_execute (< threshold) or
//	              |                   | classifier (>= threshold)
//	LevelMedium   | require_approval  | auto_execute (< threshold) or
//	              |                   | classifier (>= threshold)
//	LevelHigh     | require_approval  | classifier (default threshold),
//	              |                   | or approval when classifier is off
//	LevelBlocked  | block             | block
//
// Bypass mode is intentionally outside this matrix: once the tool name is
// known, it skips Tool.Classify, the LLM classifier, snapshots, and approval.
// The explicit CLI flag enabling it carries the corresponding warning.
//
// Auto threshold is config.Safety.Classifier.ReviewThreshold; default
// LevelMedium. Manual threshold is fixed at LevelLow — any mutation
// gets a prompt. ReviewAllTools explicitly sends Safe calls to the classifier
// in either policy and overrides the normal review threshold in Auto mode.
// Otherwise, a threshold of "off" never calls the classifier but LevelHigh
// calls still require user approval ("off" disables classifier calls, not
// safety). Tools can set RequiresApproval for scope-expansion cases that must
// be user-approved even in Auto mode.
func (a *Agent) classifyToolCall(ctx context.Context, tc *protocol.ToolCall) ActionDecision {
	tool, ok := a.tools[tc.Name]
	if !ok {
		return ActionDecision{
			Decision:  ExecutionDecisionBlock,
			Source:    SafetyDecisionSourceHardRule,
			Reason:    "tool not found",
			RiskLevel: tools.LevelBlocked.String(),
		}
	}
	if a.policy == ApprovalPolicyBypass {
		return ActionDecision{
			Decision: ExecutionDecisionAutoExecute,
			Source:   SafetyDecisionSourceBypass,
			Reason:   "permission checks bypassed",
		}
	}

	decision := tool.Classify(tools.SafetyCall{
		Arguments: tc.Arguments,
		WorkDir:   a.executor.WorkDir(),
		SafeDirs:  a.safeDirs(),
		Executor:  a.executor,
	})

	level := decision.Level
	riskLabel := level.String()

	if level >= tools.LevelBlocked {
		return ActionDecision{
			Decision:  ExecutionDecisionBlock,
			Source:    SafetyDecisionSourceHardRule,
			Reason:    decision.Reason,
			RiskLevel: riskLabel,
		}
	}

	if decision.RequiresApproval {
		return ActionDecision{
			Decision:         ExecutionDecisionRequireApproval,
			Source:           SafetyDecisionSourceHardRule,
			Reason:           decision.Reason,
			RiskLevel:        riskLabel,
			RequiresSnapshot: decision.RequiresSnapshot,
		}
	}

	if level <= tools.LevelSafe {
		if a.reviewAllTools {
			if resp, ok := a.classifyWithSafetyClassifier(ctx, tc, riskLabel); ok {
				return resp
			}
			return ActionDecision{
				Decision:         ExecutionDecisionRequireApproval,
				Source:           SafetyDecisionSourceHardRule,
				Reason:           decision.Reason,
				RiskLevel:        riskLabel,
				RequiresSnapshot: decision.RequiresSnapshot,
			}
		}
		return ActionDecision{
			Decision:         ExecutionDecisionAutoExecute,
			Source:           SafetyDecisionSourceHardRule,
			Reason:           decision.Reason,
			RiskLevel:        riskLabel,
			RequiresSnapshot: decision.RequiresSnapshot,
		}
	}

	if a.policy != ApprovalPolicyAuto {
		// Manual mode: any mutation (level > Safe) prompts the user.
		return ActionDecision{
			Decision:         ExecutionDecisionRequireApproval,
			Source:           SafetyDecisionSourceHardRule,
			Reason:           decision.Reason,
			RiskLevel:        riskLabel,
			RequiresSnapshot: decision.RequiresSnapshot,
		}
	}

	threshold := tools.ParseSafetyLevel(a.cfg.Safety.Classifier.ReviewThreshold)
	if a.reviewAllTools {
		threshold = tools.LevelSafe
	}
	// "off" / "none" parses to a level beyond LevelHigh. Interpret this as
	// "never call the classifier" but still require approval for anything
	// LevelHigh and above — the setting disables the classifier, not
	// safety. Clamp the threshold so High stays above it.
	classifierDisabled := threshold > tools.LevelHigh
	if classifierDisabled {
		threshold = tools.LevelHigh
	}

	if level < threshold {
		// Below the configured review bar: trust the tool's call.
		return ActionDecision{
			Decision:         ExecutionDecisionAutoExecute,
			Source:           SafetyDecisionSourceHardRule,
			Reason:           decision.Reason,
			RiskLevel:        riskLabel,
			RequiresSnapshot: decision.RequiresSnapshot,
		}
	}

	// At or above the threshold: consult the classifier (when enabled).
	if !classifierDisabled {
		if resp, ok := a.classifyWithSafetyClassifier(ctx, tc, riskLabel); ok {
			return resp
		}
	}
	// Classifier disabled or unavailable — fall back to user approval.
	return ActionDecision{
		Decision:         ExecutionDecisionRequireApproval,
		Source:           SafetyDecisionSourceHardRule,
		Reason:           decision.Reason,
		RiskLevel:        riskLabel,
		RequiresSnapshot: decision.RequiresSnapshot,
	}
}

func (a *Agent) classifyWithSafetyClassifier(ctx context.Context, tc *protocol.ToolCall, riskLevel string) (ActionDecision, bool) {
	if !a.cfg.Safety.Classifier.IsEnabled() {
		return ActionDecision{}, false
	}
	if a.classifier == nil {
		reason := a.classifierUnavailableReason
		if reason == "" {
			reason = "classifier enabled but no classifier implementation configured; falling back to user approval"
		}
		return ActionDecision{
			Decision:  ExecutionDecisionRequireApproval,
			Source:    SafetyDecisionSourceClassifier,
			Reason:    reason,
			RiskLevel: riskLevel,
		}, true
	}

	req := SafetyClassifierRequest{
		UserPrompt:     a.turnSafety.CurrentUserPrompt,
		RequestContext: a.turnSafety.RequestContext.Clone(),
		ToolName:       tc.Name,
		ToolArguments:  tc.Arguments,
		RiskLevel:      riskLevel,
		IsGitRepo:      a.isGitRepository(context.Background(), a.executor),
		HasSnapshot:    a.turnSafety.SnapshotCreated,
		RecentActions:  a.recentActionsForClassifier(),
	}

	// Capture a bounded dump of the classifier request for audit when the
	// operator has explicitly opted in. Never attached otherwise.
	var rawCtx string
	if a.cfg.Safety.Classifier.CaptureRawAuditContext {
		limit := a.cfg.Safety.Classifier.MaxAuditContextChars
		if limit <= 0 {
			limit = 1200
		}
		rawCtx = summarizeClassifierRequest(req, limit)
	}

	resp, err := a.classifier.Classify(ctx, req)
	if err != nil {
		return ActionDecision{
			Decision:   ExecutionDecisionRequireApproval,
			Source:     SafetyDecisionSourceClassifier,
			Reason:     fmt.Sprintf("classifier error: %v; falling back to user approval", err),
			RiskLevel:  riskLevel,
			RawContext: rawCtx,
		}, true
	}

	switch resp.Decision {
	case SafetyClassifierDecisionAllow:
		// Classifier is only called in Auto mode (see classifyToolCall /
		// classifyShellToolCall), so allow always means auto-execute. Manual
		// mode never consults the classifier — users review every mutating
		// action themselves.
		return ActionDecision{
			Decision:         ExecutionDecisionAutoExecute,
			Source:           SafetyDecisionSourceClassifier,
			Reason:           resp.Reason,
			RiskLevel:        riskLevel,
			RequiresSnapshot: resp.RequiresSnapshot,
			RawContext:       rawCtx,
		}, true
	case SafetyClassifierDecisionDeny:
		return ActionDecision{
			Decision:   ExecutionDecisionBlock,
			Source:     SafetyDecisionSourceClassifier,
			Reason:     resp.Reason,
			RiskLevel:  riskLevel,
			RawContext: rawCtx,
		}, true
	case SafetyClassifierDecisionAskUser:
		return ActionDecision{
			Decision:         ExecutionDecisionRequireApproval,
			Source:           SafetyDecisionSourceClassifier,
			Reason:           resp.Reason,
			RiskLevel:        riskLevel,
			RequiresSnapshot: resp.RequiresSnapshot,
			RawContext:       rawCtx,
		}, true
	default:
		return ActionDecision{}, false
	}
}

// summarizeClassifierRequest renders a bounded textual dump of a classifier
// request for audit persistence. It is only used when the operator has
// enabled CaptureRawAuditContext.
func summarizeClassifierRequest(req SafetyClassifierRequest, limit int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "user_prompt: %s\n", summarizeText(req.UserPrompt, 400))
	if req.RequestContext != nil {
		fmt.Fprintf(&sb, "request_context: %s\n", summarizeText(req.RequestContext.PromptText(), 400))
	}
	fmt.Fprintf(&sb, "tool: %s\n", req.ToolName)
	fmt.Fprintf(&sb, "args: %s\n", summarizeText(req.ToolArguments, 400))
	fmt.Fprintf(&sb, "risk_level: %s\n", req.RiskLevel)
	fmt.Fprintf(&sb, "is_git_repo: %t\n", req.IsGitRepo)
	fmt.Fprintf(&sb, "has_snapshot: %t\n", req.HasSnapshot)
	if len(req.RecentActions) > 0 {
		sb.WriteString("recent_actions:\n")
		for _, a := range req.RecentActions {
			status := "ok"
			if a.IsError {
				status = "error"
			}
			fmt.Fprintf(&sb, "- [%s] %s: %s\n", status, a.ToolName, summarizeText(a.Result, 120))
		}
	}
	return summarizeText(sb.String(), limit)
}

func (a *Agent) recentActionsForClassifier() []RecentAction {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var actions []RecentAction
	history := a.conversation.history
	for i := len(history) - 1; i >= 0 && len(actions) < 6; i-- {
		msg := history[i]
		for _, cp := range msg.Content {
			if cp.Type == protocol.ContentTypeToolResult && cp.ToolResult != nil {
				actions = append(actions, RecentAction{
					ToolName: cp.ToolResult.ToolName,
					Result:   summarizeText(cp.ToolResult.Text, 160),
					IsError:  cp.ToolResult.IsError,
				})
			}
		}
	}
	return actions
}

func (a *Agent) ensureSnapshotForActions(ctx context.Context, exec executor.Executor, actions []*PendingAction) error {
	var needsSnapshot bool
	for _, action := range actions {
		if action.Decision.RequiresSnapshot {
			needsSnapshot = true
			break
		}
	}
	if !needsSnapshot || a.turnSafety.SnapshotCreated {
		return nil
	}

	if !a.isGitRepository(ctx, exec) {
		a.setTurnSnapshot(SnapshotStatusMissingGitRepo, "")
		return nil
	}

	a.turnSafety.SnapshotAttempted = true
	result, err := exec.Exec(ctx, `snap-commit store -m "zotigo pre-action snapshot"`, executor.ExecOptions{})

	// snap-commit is an optional external binary. If it isn't installed,
	// degrade gracefully: mark the turn as unsnapshotted but do not abort
	// the user's action. The same pattern is used in
	// cli/commands/builtin/snapshot.go for explicit /snapshot invocations.
	//
	// LocalExecutor swallows *exec.ExitError (so a missing binary surfaces
	// as err=nil, exit=127, stderr contains "command not found") while other
	// executors may surface it as a non-nil error. Check both paths.
	if isCommandNotFound(err, result) {
		a.setTurnSnapshot(SnapshotStatusNotInstalled, "")
		return nil
	}
	if err != nil {
		a.setTurnSnapshot(SnapshotStatusFailed, "")
		return fmt.Errorf("failed to create pre-action snapshot: %w", err)
	}
	if !result.Success() {
		a.setTurnSnapshot(SnapshotStatusFailed, "")
		stderr := strings.TrimSpace(string(result.Stderr))
		if stderr == "" {
			stderr = fmt.Sprintf("exit code %d", result.ExitCode)
		}
		return fmt.Errorf("failed to create pre-action snapshot: %s", stderr)
	}

	snapshotID := summarizeText(strings.TrimSpace(string(result.Stdout)), 120)
	a.setTurnSnapshot(SnapshotStatusCreated, snapshotID)
	return nil
}

// isCommandNotFound reports whether an exec error / result pair indicates the
// snap-commit binary is missing from PATH, as opposed to snap-commit itself
// failing with a non-zero exit.
//
// We deliberately use narrow, anchored matches. A bare "not found" substring
// is too broad: snap-commit's own failure messages can legitimately contain
// "ref not found" or "object not found", and mis-classifying those as
// "binary missing" would silently skip the snapshot for a protected action.
func isCommandNotFound(err error, result *executor.ExecResult) bool {
	if err != nil {
		// Go's os/exec surfaces a missing binary with this canonical string
		// (see exec.LookPath / *exec.Error).
		if strings.Contains(strings.ToLower(err.Error()), "executable file not found") {
			return true
		}
	}
	if result != nil {
		// POSIX shells return 127 when the command does not exist. This is the
		// most reliable signal across platforms.
		if result.ExitCode == 127 {
			return true
		}
		// Some non-POSIX execution paths may not set exit 127 but still print
		// the canonical "command not found" phrase to stderr. Match only the
		// anchored phrase to avoid swallowing unrelated snap-commit errors.
		if strings.Contains(strings.ToLower(string(result.Stderr)), "command not found") {
			return true
		}
	}
	return false
}

func (a *Agent) isGitRepository(ctx context.Context, exec executor.Executor) bool {
	result, err := exec.Exec(ctx, "git rev-parse --is-inside-work-tree", executor.ExecOptions{Timeout: 2 * time.Second})
	if err != nil || !result.Success() {
		return false
	}
	return strings.TrimSpace(string(result.Stdout)) == "true"
}

func summarizeText(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}

func mapExecutionDecision(decision ExecutionDecision) SafetyClassifierDecision {
	switch decision {
	case ExecutionDecisionAutoExecute:
		return SafetyClassifierDecisionAllow
	case ExecutionDecisionBlock:
		return SafetyClassifierDecisionDeny
	default:
		return SafetyClassifierDecisionAskUser
	}
}

// safeDirs returns all directories that are safe for auto-approved read access.
func (a *Agent) safeDirs() []string {
	dirs := make([]string, 0, 1+len(a.extraSafeDirs))
	dirs = append(dirs, a.executor.WorkDir())
	dirs = append(dirs, a.extraSafeDirs...)
	return dirs
}
