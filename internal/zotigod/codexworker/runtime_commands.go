package codexworker

import (
	"context"
	"fmt"
	"strings"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

func (r *Runtime) handleCommand(ctx context.Context, command wire.Command, boundResults <-chan wire.ConversationBoundResult) error {
	switch command.Type {
	case wire.CommandMessage:
		client := &workerbase.RuntimeToolClient{Writer: r.writer, Results: r.runtimeToolResults}
		if err := workerbase.AuthorizeRuntimeToolReplay(ctx, client, command.ID); err != nil {
			return err
		}
		if r.activeTurnID != "" {
			return sessionlog.ErrActiveTurn
		}
		turnID, runtime, err := r.callTurnStart(ctx, command, boundResults)
		if err != nil {
			return err
		}
		return r.recordTurnStarted(ctx, command, turnID, runtime)
	case wire.CommandPause:
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
	case wire.CommandSteering:
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
		if err := r.PersistSteering(command, targetTurnID); err != nil {
			return err
		}
		r.acknowledgeCommand(command.Sequence)
		return nil
	case wire.CommandApprovalPolicy:
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

func (r *Runtime) codexTurnIsTerminal(ctx context.Context, turnID string) (bool, error) {
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

func (r *Runtime) sendIdleAfterPause(ctx context.Context, commandSequence uint64) error {
	if r.writer == nil || r.messagePending {
		return nil
	}
	return r.writer.SendIdle(ctx, wire.Idle{CommandSequence: commandSequence})
}

func (r *Runtime) setApprovalPolicy(ctx context.Context, commandID string, command *wire.ApprovalPolicyCommandPayload) error {
	target, err := workerbase.NormalizeSessionApprovalPolicy(command.Policy, false)
	if err != nil {
		return err
	}
	sess, err := workerbase.EnsureSession(ctx, r.store, r.cfg.SessionID, r.cfg.WorkingDirectory)
	if err != nil {
		return err
	}
	from, err := workerbase.NormalizeSessionApprovalPolicy(sess.ApprovalPolicy, true)
	if err != nil {
		return err
	}
	sess.ApprovalPolicy = target
	sess.UpdatedAt = time.Now().UTC()
	loweringPermissions := workerbase.LowersPermissions(from, target)
	if loweringPermissions {
		r.cfg.ApprovalPolicy = string(target)
	}
	if err := workerbase.PersistSessionApprovalPolicy(ctx, r.store, sess); err != nil {
		return fmt.Errorf("persist approval policy: %w", err)
	}
	if !loweringPermissions {
		r.cfg.ApprovalPolicy = string(target)
	}
	if err := r.Append(zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemApprovalPolicyChanged,
		ApprovalPolicy: &zotigosession.DisplayApprovalPolicyChange{
			CommandID: commandID, From: string(from), To: string(target),
		},
	}); err != nil {
		return fmt.Errorf("record approval policy change: %w", err)
	}
	return nil
}

func (r *Runtime) canApplyCommand(command wire.Command) bool {
	return command.Type != wire.CommandMessage || r.activeTurnID == ""
}

func (r *Runtime) acknowledgeCommand(sequence uint64) {
	if sequence > r.commandSequence {
		r.commandSequence = sequence
	}
}
