package zotigod

import (
	"context"
	"fmt"
	"strings"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
)

func (r *codexWorkerRuntime) handleCommand(ctx context.Context, command commandResponse, boundResults <-chan workerConversationBoundResult) error {
	switch command.Type {
	case sessionCommandMessage:
		client := &workerRuntimeToolClient{writer: r.writer, results: r.runtimeToolResults}
		if err := authorizeRuntimeToolReplay(ctx, client, command.ID); err != nil {
			return err
		}
		if r.activeTurnID != "" {
			return errActiveTurn
		}
		turnID, runtime, err := r.callTurnStart(ctx, command, boundResults)
		if err != nil {
			return err
		}
		return r.recordTurnStarted(ctx, command, turnID, runtime)
	case sessionCommandPause:
		if r.activeTurnID == "" {
			r.acknowledgeCommand(command.Sequence)
			return r.sendIdleAfterPause(ctx, command.Sequence)
		}
		targetTurnID := r.activeTurnID
		if command.Pause != nil && command.Pause.TurnID != "" {
			targetTurnID = command.Pause.TurnID
		}
		if targetTurnID != r.activeTurnID {
			r.acknowledgeCommand(command.Sequence)
			return nil
		}
		var response any
		if err := r.app.Call(ctx, "turn/interrupt", map[string]any{"threadId": r.threadID, "turnId": targetTurnID}, &response); err != nil {
			terminal, checkErr := r.codexTurnIsTerminal(ctx, targetTurnID)
			if checkErr != nil || !terminal {
				return err
			}
			r.acknowledgeCommand(command.Sequence)
			return r.sendIdleAfterPause(ctx, command.Sequence)
		}
		r.acknowledgeCommand(command.Sequence)
		return nil
	case sessionCommandSteering:
		if r.activeTurnID == "" || command.Steering == nil {
			return fmt.Errorf("codex steering requires an active turn")
		}
		if command.Steering.TurnID != "" && command.Steering.TurnID != r.activeTurnID {
			return nil
		}
		targetTurnID := r.activeTurnID
		if err := r.callTurnSteer(ctx, command, targetTurnID); err != nil {
			return err
		}
		if err := r.persistSteering(command, targetTurnID); err != nil {
			return err
		}
		r.acknowledgeCommand(command.Sequence)
		return nil
	case sessionCommandApprovalPolicy:
		if command.ApprovalPolicy == nil {
			return fmt.Errorf("codex approval policy payload is missing")
		}
		if err := r.setApprovalPolicy(ctx, command.ID, command.ApprovalPolicy); err != nil {
			return err
		}
		r.acknowledgeCommand(command.Sequence)
		return nil
	default:
		return fmt.Errorf("codex runtime does not support command %q", command.Type)
	}
}

func (r *codexWorkerRuntime) codexTurnIsTerminal(ctx context.Context, turnID string) (bool, error) {
	turns, err := listCodexThreadTurns(ctx, r.app, r.threadID, "notLoaded")
	if err != nil {
		return false, err
	}
	for _, turn := range turns {
		if turn.ID == turnID {
			switch strings.ToLower(turn.Status) {
			case "completed", "failed", "interrupted", "cancelled", "canceled":
				return true, nil
			default:
				return false, nil
			}
		}
	}
	return false, nil
}

func (r *codexWorkerRuntime) sendIdleAfterPause(ctx context.Context, commandSequence uint64) error {
	if r.writer == nil || r.messagePending {
		return nil
	}
	return r.writer.SendIdle(ctx, workerIdle{CommandSequence: commandSequence})
}

func (r *codexWorkerRuntime) setApprovalPolicy(ctx context.Context, commandID string, command *approvalPolicyCommandPayload) error {
	target, err := normalizeSessionApprovalPolicy(command.Policy, false)
	if err != nil {
		return err
	}
	sess, err := ensureWorkerSession(ctx, r.store, r.cfg.SessionID, r.cfg.WorkingDirectory)
	if err != nil {
		return err
	}
	from, err := normalizeSessionApprovalPolicy(sess.ApprovalPolicy, true)
	if err != nil {
		return err
	}
	sess.ApprovalPolicy = target
	sess.UpdatedAt = time.Now().UTC()
	loweringPermissions := lowersPermissions(from, target)
	if loweringPermissions {
		r.cfg.ApprovalPolicy = string(target)
	}
	if err := persistSessionApprovalPolicy(ctx, r.store, sess); err != nil {
		return fmt.Errorf("persist approval policy: %w", err)
	}
	if !loweringPermissions {
		r.cfg.ApprovalPolicy = string(target)
	}
	if err := r.append(zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemApprovalPolicyChanged,
		ApprovalPolicy: &zotigosession.DisplayApprovalPolicyChange{
			CommandID: commandID, From: string(from), To: string(target),
		},
	}); err != nil {
		return fmt.Errorf("record approval policy change: %w", err)
	}
	return nil
}

func (r *codexWorkerRuntime) canApplyCommand(command commandResponse) bool {
	return command.Type != sessionCommandMessage || r.activeTurnID == ""
}

func (r *codexWorkerRuntime) acknowledgeCommand(sequence uint64) {
	if sequence > r.commandSequence {
		r.commandSequence = sequence
	}
}
