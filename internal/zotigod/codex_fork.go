package zotigod

import (
	"context"
	"fmt"
	"strings"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/codexapp"
)

// forkCodexConversation never resumes, rolls back, or changes the source thread.
// Callers must journal dispatch before calling: a transport failure can leave an
// unbound child thread, and retrying the RPC would create another child.
func forkCodexConversation(ctx context.Context, host codexapp.HostProvider, source *zotigosession.Session, throughTurnID string) (string, error) {
	if host == nil || source == nil || strings.TrimSpace(source.ConversationID) == "" || strings.TrimSpace(throughTurnID) == "" {
		return "", fmt.Errorf("codex fork requires a configured host, source conversation, and completed turn")
	}
	lease, err := host.Acquire(ctx)
	if err != nil {
		return "", fmt.Errorf("acquire Codex fork host: %w", err)
	}
	defer func() { _ = lease.Release() }()
	params := codexThreadParams(codexWorkerConfig{
		WorkingDirectory:      source.WorkingDirectory,
		Model:                 source.Model,
		ApprovalPolicy:        string(source.ApprovalPolicy),
		DeveloperInstructions: codexDeveloperInstructions(source.PromptConfig),
		AutoReview:            source.PromptConfig.ReviewAllTools,
		AutoReviewPolicy:      codexAutoReviewPolicy(source.PromptConfig),
		ChannelToolsVersion:   source.Capabilities.ChannelToolsVersion,
	}, false)
	params["threadId"] = source.ConversationID
	params["lastTurnId"] = throughTurnID
	params["excludeTurns"] = true
	// Forking must not execute inherited goals before a new explicit input.
	params["deferGoalContinuation"] = true
	var response codexThreadSnapshot
	if err := lease.RPC.Call(ctx, "thread/fork", params, &response); err != nil {
		return "", fmt.Errorf("fork Codex thread (exact lastTurnId support required): %w", err)
	}
	childID := response.Thread.ID
	if childID == "" || childID == source.ConversationID {
		return "", fmt.Errorf("codex fork did not return a distinct child thread")
	}
	// Older servers may ignore unknown fields. Do not bind a child containing
	// later turns if lastTurnId was silently ignored. Only read the last turn,
	// never hydrate the full history just to verify the branch boundary.
	var tail codexTurnList
	if err := lease.RPC.Call(ctx, "thread/turns/list", map[string]any{
		"threadId": childID, "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded",
	}, &tail); err != nil {
		return "", fmt.Errorf("verify Codex fork boundary: %w", err)
	}
	if len(tail.Data) != 1 || tail.Data[0].ID != throughTurnID || tail.Data[0].Status != "completed" {
		return "", fmt.Errorf("codex fork boundary mismatch: exact completed-turn fork is unsupported or source changed")
	}
	return childID, nil
}
