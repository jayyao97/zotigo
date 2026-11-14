package zotigod

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	_ "github.com/jayyao97/zotigo/core/providers/anthropic"
	_ "github.com/jayyao97/zotigo/core/providers/deepseek"
	_ "github.com/jayyao97/zotigo/core/providers/gemini"
	_ "github.com/jayyao97/zotigo/core/providers/openai"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/wiring"
)

func (r *workerRuntime) switchProfile(ctx context.Context, commandID string, command *profileCommandPayload) (<-chan error, error) {
	completion := make(chan error, 1)
	predecessor, ordered := r.nextProfileOrder()
	target := strings.TrimSpace(command.Name)
	epoch, err := r.nextProfileEpoch()
	if err != nil {
		close(ordered)
		close(completion)
		return completion, err
	}
	r.agent.SupersedePendingRuntimeProfile()
	appConfig, err := config.NewManager().LoadForDir(r.workDir)
	if err != nil {
		r.completeProfileFailure(predecessor, ordered, completion, commandID, target, fmt.Errorf("load profiles: %w", err))
		return completion, nil
	}
	_, profile, err := appConfig.ResolveProfile(target)
	if err != nil {
		r.completeProfileFailure(predecessor, ordered, completion, commandID, target, err)
		return completion, nil
	}
	runtimeProfile, err := wiring.NewRuntimeProfile(wiring.AgentConfig{
		Config:               appConfig,
		ProfileName:          target,
		Profile:              profile,
		Observer:             r.observer,
		ConfigureClassifier:  true,
		ApprovalInstructions: r.promptConfig.ApprovalInstructions,
		ReviewAllTools:       r.promptConfig.ReviewAllTools,
	})
	if err != nil {
		r.completeProfileFailure(predecessor, ordered, completion, commandID, target, err)
		return completion, nil
	}
	runtimeProfile.BeforeApply = func() error {
		<-predecessor
		if err := r.currentFatalError(); err != nil {
			return err
		}
		from := r.agent.ActiveProfileName()
		commitCtx, cancel := context.WithTimeout(context.Background(), workerHTTPTimeout)
		defer cancel()
		return r.commitLatestProfileSwitch(commitCtx, epoch, commandID, from, target, profile.Model)
	}
	result := r.agent.QueueRuntimeProfile(runtimeProfile)
	go r.finishProfileSwitch(predecessor, ordered, commandID, target, result, completion)
	return completion, nil
}

func (r *workerRuntime) nextProfileOrder() (<-chan struct{}, chan struct{}) {
	r.profileOrderMu.Lock()
	defer r.profileOrderMu.Unlock()
	predecessor := r.profileOrderTail
	if predecessor == nil {
		ready := make(chan struct{})
		close(ready)
		predecessor = ready
	}
	ordered := make(chan struct{})
	r.profileOrderTail = ordered
	return predecessor, ordered
}

func (r *workerRuntime) nextProfileEpoch() (uint64, error) {
	r.profileMu.Lock()
	defer r.profileMu.Unlock()
	if err := r.currentFatalError(); err != nil {
		return 0, err
	}
	r.profileEpoch++
	return r.profileEpoch, nil
}

func (r *workerRuntime) commitLatestProfileSwitch(ctx context.Context, epoch uint64, commandID string, from string, target string, model string) error {
	r.profileMu.Lock()
	defer r.profileMu.Unlock()
	if r.profileEpoch != epoch {
		return agent.ErrRuntimeProfileSuperseded
	}
	err := r.commitProfileSwitch(ctx, commandID, from, target)
	if err == nil {
		r.hookModel = model
	}
	var uncertain *profileStateUncertainError
	if errors.As(err, &uncertain) {
		r.fail(uncertain)
	}
	return err
}

func (r *workerRuntime) turnHookModel() string {
	r.profileMu.Lock()
	defer r.profileMu.Unlock()
	return r.hookModel
}

func (r *workerRuntime) finishProfileSwitch(predecessor <-chan struct{}, ordered chan struct{}, commandID string, target string, result <-chan error, completion chan<- error) {
	defer close(completion)
	err := <-result
	<-predecessor
	defer close(ordered)
	if fatalErr := r.currentFatalError(); fatalErr != nil {
		completion <- fatalErr
		return
	}
	var uncertain *profileStateUncertainError
	if errors.As(err, &uncertain) {
		r.fail(uncertain)
		completion <- uncertain
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if errors.Is(err, agent.ErrRuntimeProfileSuperseded) {
		err = r.recordProfileFailure(ctx, commandID, target, agent.ErrRuntimeProfileSuperseded)
		if err != nil {
			err = &profileCompletionUncertainError{cause: err}
			r.fail(err)
		}
		completion <- err
		return
	}
	if err != nil {
		err = r.recordProfileFailure(ctx, commandID, target, err)
		if err != nil {
			err = &profileCompletionUncertainError{cause: err}
			r.fail(err)
		}
		completion <- err
		return
	}
	completion <- nil
}

func (r *workerRuntime) completeProfileFailure(predecessor <-chan struct{}, ordered chan struct{}, completion chan<- error, commandID string, target string, cause error) {
	defer close(completion)
	<-predecessor
	defer close(ordered)
	if fatalErr := r.currentFatalError(); fatalErr != nil {
		completion <- fatalErr
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := r.recordProfileFailure(ctx, commandID, target, cause)
	if err != nil {
		err = &profileCompletionUncertainError{cause: err}
		r.fail(err)
	}
	completion <- err
}

type profileStateUncertainError struct {
	cause error
}

func (e *profileStateUncertainError) Error() string {
	return "profile state is uncertain: " + e.cause.Error()
}
func (e *profileStateUncertainError) Unwrap() error { return e.cause }

type profileCompletionUncertainError struct{ cause error }

func (e *profileCompletionUncertainError) Error() string {
	return "profile command completion is uncertain: " + e.cause.Error()
}
func (e *profileCompletionUncertainError) Unwrap() error { return e.cause }

func (r *workerRuntime) fail(err error) {
	if err == nil {
		return
	}
	r.fatalMu.Lock()
	if r.fatalErr == nil {
		r.fatalErr = err
	}
	fatalErr := r.fatalErr
	r.fatalMu.Unlock()
	if r.fatalCh == nil {
		return
	}
	select {
	case r.fatalCh <- fatalErr:
	default:
	}
}

func (r *workerRuntime) currentFatalError() error {
	r.fatalMu.Lock()
	defer r.fatalMu.Unlock()
	return r.fatalErr
}

func (r *workerRuntime) commitProfileSwitch(ctx context.Context, commandID string, from string, target string) error {
	r.storeMu.Lock()
	defer r.storeMu.Unlock()
	sess, err := ensureWorkerSession(ctx, r.store, r.sessionID, r.workDir)
	if err != nil {
		return err
	}
	previousProfile := sess.ProfileName
	previousUpdatedAt := sess.UpdatedAt
	sess.ProfileName = target
	sess.UpdatedAt = time.Now().UTC()
	if err := persistSessionProfile(ctx, r.store, sess); err != nil {
		if errors.Is(err, zotigosession.ErrProfileStateUncertain) {
			return &profileStateUncertainError{cause: err}
		}
		return err
	}
	if err := r.display.ProfileChanged(ctx, commandID, from, target); err != nil {
		sess.ProfileName = previousProfile
		sess.UpdatedAt = previousUpdatedAt
		if rollbackErr := persistSessionProfile(ctx, r.store, sess); rollbackErr != nil {
			return &profileStateUncertainError{cause: errors.Join(
				fmt.Errorf("append profile changed: %w", err),
				fmt.Errorf("rollback session profile: %w", rollbackErr),
			)}
		}
		return fmt.Errorf("append profile changed: %w", err)
	}
	return nil
}

func (r *workerRuntime) recordProfileFailure(ctx context.Context, commandID string, target string, err error) error {
	if appendErr := r.display.ProfileFailed(ctx, commandID, r.agent.ActiveProfileName(), target, err); appendErr != nil {
		return appendErr
	}
	return nil
}
