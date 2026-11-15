package zotigod

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	_ "github.com/jayyao97/zotigo/core/providers/anthropic"
	_ "github.com/jayyao97/zotigo/core/providers/deepseek"
	_ "github.com/jayyao97/zotigo/core/providers/gemini"
	_ "github.com/jayyao97/zotigo/core/providers/openai"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

func (r *workerRuntime) HandleCommand(ctx context.Context, command wire.Command) error {
	_, err := r.handleCommand(ctx, command)
	return err
}

func (r *workerRuntime) AcceptInput(ctx context.Context, request wire.InputRequest) wire.InputResult {
	result := wire.InputResult{RequestID: request.RequestID}
	command, err := r.acceptInput(ctx, request)
	if err != nil {
		result.ErrorCode, result.Error = workerInputErrorDetails(err)
		return result
	}
	result.Command = &command
	return result
}

func (r *workerRuntime) acceptInput(ctx context.Context, request wire.InputRequest) (wire.Command, error) {
	if existing, found, err := findExistingSessionInput(ctx, r.display.Items, r.sessionID, request.Command, storeRoot(r.store)); err != nil {
		return wire.Command{}, err
	} else if found {
		if request.SteeringOnly && existing.Type != wire.CommandSteering {
			return wire.Command{}, errCommandIDConflict
		}
		if request.StartOnly && existing.Type != wire.CommandMessage {
			return wire.Command{}, errCommandIDConflict
		}
		if request.ExpectedTurnID != "" && (existing.Steering == nil || existing.Steering.TurnID != request.ExpectedTurnID) {
			return wire.Command{}, errCommandIDConflict
		}
		return existing, nil
	}
	// Do not wait for Agent.mu while manual compaction is summarizing.
	r.mu.Lock()
	compacting := r.turnCompacting
	r.mu.Unlock()
	if compacting {
		return wire.Command{}, fmt.Errorf("%w: context compaction is in progress", errActiveTurn)
	}
	if request.RequiredApprovalPolicy != "" || request.RequirePromptRevision {
		r.mu.Lock()
		promptRevision := r.promptConfig.Revision
		r.mu.Unlock()
		if request.RequiredApprovalPolicy != "" {
			policy, err := r.agent.RequestedApprovalPolicyForInput()
			if errors.Is(err, agent.ErrCompacting) {
				return wire.Command{}, fmt.Errorf("%w: %v", errActiveTurn, err)
			}
			if err != nil {
				return wire.Command{}, err
			}
			if policy != request.RequiredApprovalPolicy {
				return wire.Command{}, errInputPolicyMismatch
			}
		}
		if request.RequirePromptRevision && promptRevision != request.RequiredPromptRevision {
			return wire.Command{}, errInputPolicyMismatch
		}
	}
	turnID := r.display.CurrentTurnID()
	if request.StartOnly {
		r.mu.Lock()
		turnActive := r.turnActive
		r.mu.Unlock()
		if turnID != "" || turnActive {
			return wire.Command{}, errActiveTurn
		}
	}
	if request.ExpectedTurnID != "" && request.ExpectedTurnID != turnID {
		return wire.Command{}, errTurnMismatch
	}
	if request.SteeringOnly && turnID == "" {
		return wire.Command{}, errNoActiveTurn
	}
	if isCompactCommand(messageCommandForRequest(request).Message) && turnID != "" {
		return wire.Command{}, fmt.Errorf("%w: wait for the current turn to finish before /compact", errActiveTurn)
	}

	message, err := inputMessageFromRequest(request)
	if err != nil {
		return wire.Command{}, err
	}
	if turnID != "" {
		ready, err := r.waitForTurnAdmission(ctx, turnID)
		if err != nil {
			return wire.Command{}, err
		}
		if !ready {
			if request.SteeringOnly {
				return wire.Command{}, errNoActiveTurn
			}
			turnID = ""
		}
	}
	if turnID != "" {
		steering := steeringCommandForRequest(request, turnID)
		r.display.QueueSteering(steering)
		err := r.agent.QueueTurnUserMessageWithAdmission(message, func() error {
			if err := validateCompletionSteering(ctx, r.display.Items, r.sessionID, request.Command); err != nil {
				return err
			}
			stored, appendErr := appendAcceptedSessionInput(ctx, r.display.Items, r.sessionID, steering)
			if appendErr == nil {
				steering = stored
			}
			return appendErr
		})
		if err == nil {
			r.noteTurnCommandSequence(steering.Sequence)
			if err := r.afterTurnUserInputQueued(ctx, turnID, steering.Steering.Text); err != nil {
				return wire.Command{}, err
			}
			return steering, nil
		}
		r.display.DiscardSteering(steering.ID)
		if errors.Is(err, agent.ErrCompacting) {
			return wire.Command{}, fmt.Errorf("%w: %v", errActiveTurn, err)
		}
		if request.SteeringOnly || !isStaleTurnUserInputError(err) {
			if isStaleTurnUserInputError(err) {
				return wire.Command{}, errNoActiveTurn
			}
			return wire.Command{}, err
		}
	}

	messageCommand := messageCommandForRequest(request)
	stored, err := appendAcceptedSessionInput(ctx, r.display.Items, r.sessionID, messageCommand)
	if err != nil {
		return wire.Command{}, err
	}
	if err := r.startMessageTurn(ctx, stored.ID, stored.Sequence, stored.Message); err != nil {
		return wire.Command{}, err
	}
	return stored, nil
}

func (r *workerRuntime) waitForTurnAdmission(ctx context.Context, turnID string) (bool, error) {
	r.mu.Lock()
	active := r.turnActive
	ready := r.turnReady
	done := r.turnDone
	r.mu.Unlock()
	if !active || ready == nil || done == nil {
		return false, nil
	}
	select {
	case <-ready:
		return r.display.CurrentTurnID() == turnID, nil
	case <-done:
		return false, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (r *workerRuntime) resolveApproval(ctx context.Context, decision wire.ApprovalDecision) (wire.ApprovalResult, *workerApprovalResolution) {
	result := wire.ApprovalResult{RequestID: decision.RequestID}
	if r.transport == nil {
		result.Error = "approval transport is not configured"
		return result, nil
	}
	resolution, err := r.transport.resolveApproval(ctx, decision.ApprovalID, decision.Decisions)
	if err != nil {
		result.Error = err.Error()
		return result, nil
	}
	result.Approval = &resolution.approval
	return result, &resolution
}

func (r *workerRuntime) releaseApproval(ctx context.Context, resolution workerApprovalResolution) error {
	if r.transport == nil {
		return fmt.Errorf("approval transport is not configured")
	}
	if r.runtimeWAL != nil && !r.runtimeWAL.Active() {
		if err := r.beginRuntimeWAL(ctx); err != nil {
			return err
		}
	}
	return r.transport.releaseApproval(ctx, resolution)
}

func (r *workerRuntime) handleCommand(ctx context.Context, command wire.Command) (<-chan error, error) {
	if err := r.currentFatalError(); err != nil {
		return nil, err
	}
	if err := validateWorkerCommand(command); err != nil {
		return nil, err
	}
	switch command.Type {
	case wire.CommandMessage:
		if err := authorizeRuntimeToolReplay(ctx, r.runtimeTools, command.ID); err != nil {
			return nil, err
		}
		return nil, r.startMessageTurn(ctx, command.ID, command.Sequence, command.Message)
	case wire.CommandPause:
		r.noteTurnCommandSequence(command.Sequence)
		return nil, r.pauseTurn(ctx, command.Pause)
	case wire.CommandSteering:
		r.noteTurnCommandSequence(command.Sequence)
		return nil, r.queueTurnUserInput(ctx, command)
	case wire.CommandProfile:
		return r.switchProfile(ctx, command.ID, command.Profile)
	case wire.CommandApprovalPolicy:
		return nil, r.setApprovalPolicy(ctx, command.ID, command.ApprovalPolicy)
	default:
		return nil, nil
	}
}

func validateWorkerCommand(command wire.Command) error {
	payloads := 0
	if command.Message != nil {
		payloads++
	}
	if command.Pause != nil {
		payloads++
	}
	if command.Steering != nil {
		payloads++
	}
	if command.Profile != nil {
		payloads++
	}
	if command.ApprovalPolicy != nil {
		payloads++
	}
	switch command.Type {
	case wire.CommandMessage:
		if command.Message == nil || payloads != 1 {
			return fmt.Errorf("invalid message command payload")
		}
	case wire.CommandPause:
		if command.Pause == nil || payloads != 1 {
			return fmt.Errorf("invalid pause command payload")
		}
	case wire.CommandSteering:
		if command.Steering == nil || payloads != 1 {
			return fmt.Errorf("invalid steering command payload")
		}
	case wire.CommandProfile:
		if command.Profile == nil || strings.TrimSpace(command.Profile.Name) == "" || payloads != 1 {
			return fmt.Errorf("invalid profile command payload")
		}
	case wire.CommandApprovalPolicy:
		if command.ApprovalPolicy == nil || payloads != 1 {
			return fmt.Errorf("invalid approval policy command payload")
		}
		if _, err := normalizeSessionApprovalPolicy(command.ApprovalPolicy.Policy, false); err != nil {
			return fmt.Errorf("invalid approval policy command payload: %w", err)
		}
	default:
		return nil
	}
	return nil
}

func (r *workerRuntime) setApprovalPolicy(ctx context.Context, commandID string, command *wire.ApprovalPolicyCommandPayload) error {
	target, err := normalizeSessionApprovalPolicy(command.Policy, false)
	if err != nil {
		return err
	}
	r.storeMu.Lock()
	defer r.storeMu.Unlock()
	sess, err := ensureWorkerSession(ctx, r.store, r.sessionID, r.workDir)
	if err != nil {
		return err
	}
	from := sess.ApprovalPolicy
	sess.ApprovalPolicy = target
	sess.UpdatedAt = time.Now().UTC()
	loweringPermissions := lowersPermissions(from, target)
	if loweringPermissions {
		// Stop bypassing approval before publishing the safer persisted value.
		// If persistence fails, keeping the Agent on Auto is the safe outcome.
		r.agent.SetApprovalPolicy(target)
	}
	if err := persistSessionApprovalPolicy(ctx, r.store, sess); err != nil {
		return fmt.Errorf("persist approval policy: %w", err)
	}
	if !loweringPermissions {
		r.agent.SetApprovalPolicy(target)
	}
	if err := r.display.ApprovalPolicyChanged(ctx, commandID, string(from), string(target)); err != nil {
		return fmt.Errorf("record approval policy change: %w", err)
	}
	return nil
}
