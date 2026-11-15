package zotigod

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	_ "github.com/jayyao97/zotigo/core/providers/anthropic"
	_ "github.com/jayyao97/zotigo/core/providers/deepseek"
	_ "github.com/jayyao97/zotigo/core/providers/gemini"
	_ "github.com/jayyao97/zotigo/core/providers/openai"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/skills"
	zotigotransport "github.com/jayyao97/zotigo/core/transport"
	"github.com/jayyao97/zotigo/internal/sessionadapter"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

func (r *workerRuntime) pauseTurn(ctx context.Context, command *wire.PauseCommandPayload) error {
	currentTurnID := r.display.CurrentTurnID()
	if currentTurnID == "" {
		return nil
	}
	if command.TurnID != "" && command.TurnID != currentTurnID {
		return nil
	}
	r.mu.Lock()
	if r.turnActive {
		r.turnStopping = true
	}
	r.mu.Unlock()
	interrupted, err := r.transport.interruptTurn(ctx, currentTurnID, command.Reason, r.cancelCurrentTurn)
	if err != nil || !interrupted {
		r.mu.Lock()
		r.turnStopping = false
		r.mu.Unlock()
	}
	return err
}

func (r *workerRuntime) queueTurnUserInput(ctx context.Context, command wire.Command) error {
	payload := command.Steering
	msg, err := sessionlog.UserMessageFromCommand(payload.Text, payload.Images, "steering")
	if err != nil {
		return err
	}
	if len(msg.Content) == 0 {
		return nil
	}
	if r.display.HasQueuedSteering(command.ID) {
		return nil
	}

	r.mu.Lock()
	active := r.turnActive
	ready := r.turnReady
	done := r.turnDone
	r.mu.Unlock()
	if !active || ready == nil || done == nil {
		return staleSteeringResult(command)
	}

	currentTurnID := r.display.CurrentTurnID()
	if payload.TurnID != "" && currentTurnID != "" && payload.TurnID != currentTurnID {
		return staleSteeringResult(command)
	}
	select {
	case <-ready:
	case <-done:
		return staleSteeringResult(command)
	case <-ctx.Done():
		return ctx.Err()
	}
	currentTurnID = r.display.CurrentTurnID()
	if currentTurnID == "" || (payload.TurnID != "" && payload.TurnID != currentTurnID) {
		return staleSteeringResult(command)
	}
	msg.ID = command.ID
	r.display.QueueSteering(command)
	if err := r.agent.QueueTurnUserMessage(msg); err != nil {
		r.display.DiscardSteering(command.ID)
		if isStaleTurnUserInputError(err) {
			return staleSteeringResult(command)
		}
		return err
	}
	return r.afterTurnUserInputQueued(ctx, currentTurnID, payload.Text)
}

func (r *workerRuntime) afterTurnUserInputQueued(ctx context.Context, turnID string, text string) error {
	dispatchUserPromptSubmitHook(r.hooks, r.sessionID, "zotigo", r.workDir, turnID, text)
	snapshot := r.agent.Snapshot()
	if snapshot.State == agent.StatePaused && len(snapshot.PendingActions) > 0 {
		if err := r.beginRuntimeWAL(ctx); err != nil {
			return err
		}
		if err := r.transport.interruptApprovalForSteering(ctx, turnID); err != nil {
			_ = r.saveSnapshot(context.Background(), snapshot)
			return err
		}
	}
	return nil
}

func staleSteeringResult(wire.Command) error {
	return nil
}

func isStaleTurnUserInputError(err error) bool {
	return errors.Is(err, agent.ErrNotRunning)
}

func (r *workerRuntime) startMessageTurn(ctx context.Context, commandID string, commandSequence uint64, command *wire.MessageCommandPayload) error {
	msg, err := messageFromCommandWithSkills(commandID, command, r.skills)
	if err != nil {
		return err
	}

	for {
		r.mu.Lock()
		if !r.turnActive {
			break
		}
		// A stopping turn is about to finish; wait for it instead of
		// rejecting input that arrives while the agent is still unwinding.
		if r.agent == nil || (r.agent.Snapshot().State != agent.StateIdle && !r.turnStopping) || r.turnDone == nil {
			r.mu.Unlock()
			return sessionlog.ErrActiveTurn
		}
		done := r.turnDone
		r.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	turnCtx, cancel := context.WithCancel(context.Background())
	r.turnCancel = cancel
	r.turnActive = true
	r.turnCompacting = isCompactCommand(command)
	r.turnStopping = false
	r.turnReady = make(chan struct{})
	r.turnDone = make(chan struct{})
	r.readyDone = false
	r.doneDone = false
	r.turnCommandSequence = commandSequence
	if r.channelTools != nil {
		r.channelTools.setRequestContext(command.RequestContext)
	}
	r.mu.Unlock()

	model := r.turnHookModel()
	turnID, err := r.display.StartTurnWithRuntime(ctx, zotigosession.DisplayRuntime{Agent: "zotigo", ProfileName: r.agent.ActiveProfileName(), Model: model})
	if err != nil {
		r.finishTurn()
		return err
	}
	if err := r.beginRuntimeWAL(ctx); err != nil {
		r.finishTurn()
		return fmt.Errorf("begin runtime WAL: %w", err)
	}
	if r.notifyWorking != nil {
		_ = r.notifyWorking(ctx)
	}
	usageBefore := r.agent.Snapshot().CumulativeUsage
	dispatchTurnStartHook(r.hooks, r.sessionID, "zotigo", r.workDir, turnID, model)
	dispatchUserPromptSubmitHook(r.hooks, r.sessionID, "zotigo", r.workDir, turnID, command.Text)
	go func() {
		var runErr error
		if isCompactCommand(command) {
			r.markTurnReady()
			runErr = r.compactTurn(turnCtx)
		} else {
			runErr = r.runner.RunFullTurnStarted(turnCtx, msg, r.markTurnReady)
		}
		if runErr != nil && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, zotigotransport.ErrTransportClosed) {
			_ = r.display.Fail(context.Background(), runErr)
		}
		_ = r.agent.WaitForRuntimeIdle(context.Background())
		snapshot := r.snapshotAfterTurn(turnID)
		completedTurnID := ""
		if status, terminal := r.display.TerminalStatus(turnID); terminal && status == "completed" {
			completedTurnID = turnID
		}
		saveErr := r.saveSnapshot(context.Background(), snapshot, completedTurnID)
		if saveErr != nil {
			r.fail(fmt.Errorf("persist terminal turn state: %w", saveErr))
		} else {
			status, terminal := r.display.TerminalStatus(turnID)
			if !terminal {
				r.fail(fmt.Errorf("terminal display state missing for turn %s", turnID))
			} else {
				dispatchTurnEndHook(r.hooks, r.sessionID, "zotigo", r.workDir, turnID, status, model, turnUsageDelta(usageBefore, snapshot.CumulativeUsage))
			}
		}
		sequence := r.turnSequence()
		r.finishTurn()
		if saveErr == nil && snapshot.State != agent.StatePaused && r.notifyIdle != nil {
			_ = r.notifyIdle(context.Background(), wire.Idle{CommandSequence: sequence})
		}
	}()
	return nil
}

// Only bare, local commands are interpreted; text with attachments, skills or
// channel provenance remains normal model input.
func isCompactCommand(command *wire.MessageCommandPayload) bool {
	if command == nil || len(command.Images) != 0 || len(command.Skills) != 0 || command.RequestContext != nil {
		return false
	}
	switch strings.TrimSpace(command.Text) {
	case "/compact", "/compress":
		return true
	}
	return false
}

func (r *workerRuntime) compactTurn(ctx context.Context) error {
	result, err := r.agent.ForceCompress(ctx)
	if err != nil {
		return err
	}
	text := "Nothing to compact: the retained history cannot be reduced further."
	if result.Compressed {
		if err := r.display.HandleEvent(ctx, protocol.Event{
			Type: protocol.EventTypeContextCompacted,
			ContextCompaction: &protocol.ContextCompaction{
				OriginalTokens: result.OriginalTokens, CompressedTokens: result.CompressedTokens,
				MessagesBefore: result.MessagesBefore, MessagesAfter: result.MessagesAfter,
			},
		}); err != nil {
			return err
		}
		text = fmt.Sprintf("Context compacted: %d → %d messages. Estimated next input: %d tokens.", result.MessagesBefore, result.MessagesAfter, result.CompressedTokens)
	}
	if err := r.display.HandleEvent(ctx, protocol.NewTextDeltaEvent(text)); err != nil {
		return err
	}
	return r.display.HandleEvent(ctx, protocol.NewFinishEvent(protocol.FinishReasonStop))
}

func messageFromCommandWithSkills(commandID string, command *wire.MessageCommandPayload, manager *skills.SkillManager) (protocol.Message, error) {
	if len(command.Skills) == 0 {
		return messageFromCommand(commandID, command)
	}
	original, err := messageFromCommand(commandID, command)
	if err != nil {
		return protocol.Message{}, err
	}
	if manager == nil {
		return protocol.Message{}, fmt.Errorf("skill manager is not configured")
	}
	if err := manager.Reload(); err != nil {
		return protocol.Message{}, fmt.Errorf("reload explicit skills: %w", err)
	}
	selected, err := manager.ResolveExplicit(command.Skills)
	if err != nil {
		return protocol.Message{}, fmt.Errorf("resolve explicit skills: %w", err)
	}
	commandCopy := *command
	commandCopy.Text = skills.InjectExplicit(command.Text, selected)
	enriched, err := messageFromCommand(commandID, &commandCopy)
	if err != nil {
		return protocol.Message{}, err
	}
	if enriched.Metadata == nil {
		enriched.Metadata = &protocol.MessageMetadata{}
	}
	enriched.Metadata.OriginalText = original.String()
	return enriched, nil
}

func (r *workerRuntime) noteTurnCommandSequence(sequence uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.turnActive && sequence > r.turnCommandSequence {
		r.turnCommandSequence = sequence
	}
}

func (r *workerRuntime) turnSequence() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.turnCommandSequence
}

func (r *workerRuntime) NotifyIdleIfIdle(ctx context.Context, commandSequence uint64) error {
	r.mu.Lock()
	active := r.turnActive
	r.mu.Unlock()
	if active || r.notifyIdle == nil || r.agent.Snapshot().State == agent.StatePaused {
		return nil
	}
	return r.notifyIdle(ctx, wire.Idle{CommandSequence: commandSequence})
}

func (r *workerRuntime) snapshotAfterTurn(turnID string) agent.Snapshot {
	snapshot := r.agent.Snapshot()
	if r.transport == nil {
		return snapshot
	}
	if reason, interrupted := r.transport.interruptedTurn(turnID); interrupted && snapshot.State == agent.StatePaused {
		snapshot = agent.InterruptPendingSnapshot(snapshot, reason)
		r.agent.Restore(snapshot)
	}
	return snapshot
}

func messageFromCommand(commandID string, command *wire.MessageCommandPayload) (protocol.Message, error) {
	message, err := sessionlog.UserMessageFromCommand(command.Text, command.Images, fmt.Sprintf("message command %q", commandID))
	if err != nil {
		return protocol.Message{}, err
	}
	if command.RequestContext != nil {
		if message.Metadata == nil {
			message.Metadata = &protocol.MessageMetadata{}
		}
		message.Metadata.RequestContext = command.RequestContext.Clone()
	}
	return message, nil
}

func (r *workerRuntime) cancelCurrentTurn() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.turnCancel != nil {
		r.turnCancel()
	}
}

func (r *workerRuntime) finishTurn() {
	r.agent.ClearPendingTurnUserInput()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeTurnReadyLocked()
	r.closeTurnDoneLocked()
	r.turnCancel = nil
	r.turnActive = false
	r.turnCompacting = false
	r.turnStopping = false
	if r.channelTools != nil {
		r.channelTools.setRequestContext(nil)
	}
	r.turnReady = nil
	r.turnDone = nil
}

func (r *workerRuntime) markTurnReady() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeTurnReadyLocked()
}

func (r *workerRuntime) closeTurnReadyLocked() {
	if r.turnReady == nil || r.readyDone {
		return
	}
	close(r.turnReady)
	r.readyDone = true
}

func (r *workerRuntime) closeTurnDoneLocked() {
	if r.turnDone == nil || r.doneDone {
		return
	}
	close(r.turnDone)
	r.doneDone = true
}

func (r *workerRuntime) saveSnapshot(ctx context.Context, snap agent.Snapshot, completedTurnIDs ...string) error {
	r.storeMu.Lock()
	defer r.storeMu.Unlock()
	sess, err := ensureWorkerSession(ctx, r.store, r.sessionID, "")
	if err != nil {
		return err
	}
	sessionadapter.ApplySnapshot(sess, snap, sessionadapter.LastUserPrompt(snap.History))
	if len(completedTurnIDs) == 1 && completedTurnIDs[0] != "" && snap.State == agent.StateIdle && len(snap.PendingActions) == 0 && len(snap.DeferredActions) == 0 {
		if sess.ForkPoints == nil {
			sess.ForkPoints = make(map[string]zotigosession.ForkPoint)
		}
		sess.ForkPoints[completedTurnIDs[0]] = zotigosession.ForkPoint{HistoryLength: len(snap.History), HistoryDigest: zotigosession.SnapshotDigest(snap)}
	}
	if r.runtimeWAL != nil {
		return r.runtimeWAL.Commit(ctx, sess, r.store.Put)
	}
	sess.SnapshotVersion++
	return r.store.Put(ctx, sess)
}

func (r *workerRuntime) beginRuntimeWAL(ctx context.Context) error {
	if r.runtimeWAL == nil {
		return nil
	}
	r.storeMu.Lock()
	defer r.storeMu.Unlock()
	sess, err := ensureWorkerSession(ctx, r.store, r.sessionID, "")
	if err != nil {
		return err
	}
	return r.runtimeWAL.Begin(ctx, sess, r.agent.Snapshot(), r.display.CurrentTurnID())
}
