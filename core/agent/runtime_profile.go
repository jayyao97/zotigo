package agent

import (
	"context"
	"errors"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/providers"
)

var ErrRuntimeProfileSuperseded = errors.New("runtime profile switch superseded")

// RuntimeProfile contains the profile-dependent runtime state prepared by a host.
type RuntimeProfile struct {
	Name                        string
	Config                      config.ProfileConfig
	Provider                    providers.Provider
	Classifier                  SafetyClassifier
	ClassifierProfileName       string
	ClassifierProfile           config.ProfileConfig
	ClassifierUnavailableReason string
	ForceManualApproval         bool
	// BeforeApply commits host-owned state. A failure leaves the active runtime unchanged.
	BeforeApply func() error
}

type runtimeProfileRequest struct {
	profile RuntimeProfile
	result  chan error
}

type profileAwareTool interface {
	SetProfile(config.ProfileConfig)
}

// QueueRuntimeProfile applies immediately when no runtime goroutine is active.
// Otherwise the latest request is applied before the next provider generation.
func (a *Agent) QueueRuntimeProfile(profile RuntimeProfile) <-chan error {
	result := make(chan error, 1)
	a.mu.Lock()
	if a.pendingProfile != nil {
		a.pendingProfile.result <- ErrRuntimeProfileSuperseded
		close(a.pendingProfile.result)
	}
	a.pendingProfile = &runtimeProfileRequest{profile: profile, result: result}
	applyNow := a.canApplyRuntimeProfileLocked()
	if applyNow {
		a.beginProfileActivityLocked()
	}
	a.mu.Unlock()
	if applyNow {
		go func() {
			defer a.endProfileActivity()
			a.applyPendingRuntimeProfile(false)
		}()
	}
	return result
}

// SupersedePendingRuntimeProfile invalidates an older request before a newer
// profile is prepared. This preserves last-request-wins when preparation fails.
func (a *Agent) SupersedePendingRuntimeProfile() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pendingProfile == nil {
		return
	}
	a.pendingProfile.result <- ErrRuntimeProfileSuperseded
	close(a.pendingProfile.result)
	a.pendingProfile = nil
}

// ActiveProfileName returns the profile currently used for new generations.
func (a *Agent) ActiveProfileName() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.profileName
}

func (a *Agent) endRuntimeActivity() {
	a.mu.Lock()
	if a.runtimeActivity > 0 {
		a.runtimeActivity--
	}
	applyNow := a.canApplyRuntimeProfileLocked()
	if applyNow {
		a.beginProfileActivityLocked()
	} else {
		a.closeRuntimeIdleLocked()
	}
	a.mu.Unlock()
	if applyNow {
		a.applyPendingRuntimeProfile(false)
		a.endProfileActivity()
	}
}

func (a *Agent) beginRuntimeActivityLocked() {
	if a.runtimeActivity == 0 && a.profileActivity == 0 {
		closeRuntimeIdle(a.runtimeIdle)
		a.runtimeIdle = make(chan struct{})
	}
	a.runtimeActivity++
}

func (a *Agent) beginProfileActivityLocked() {
	if a.runtimeActivity == 0 && a.profileActivity == 0 {
		closeRuntimeIdle(a.runtimeIdle)
		a.runtimeIdle = make(chan struct{})
	}
	a.profileActivity++
}

func (a *Agent) endProfileActivity() {
	a.mu.Lock()
	if a.profileActivity > 0 {
		a.profileActivity--
	}
	a.closeRuntimeIdleLocked()
	a.mu.Unlock()
}

func (a *Agent) closeRuntimeIdleLocked() {
	if a.runtimeActivity == 0 && a.profileActivity == 0 {
		closeRuntimeIdle(a.runtimeIdle)
	}
}

// WaitForRuntimeIdle waits until all Agent-owned execution goroutines exit.
func (a *Agent) WaitForRuntimeIdle(ctx context.Context) error {
	for {
		a.mu.RLock()
		idle := a.runtimeIdle
		a.mu.RUnlock()
		select {
		case <-idle:
			a.mu.RLock()
			current := a.runtimeIdle
			active := a.runtimeActivity > 0 || a.profileActivity > 0
			a.mu.RUnlock()
			if current == idle && !active {
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func closeRuntimeIdle(idle chan struct{}) {
	if idle == nil {
		return
	}
	select {
	case <-idle:
	default:
		close(idle)
	}
}

func (a *Agent) canApplyRuntimeProfileLocked() bool {
	return a.runtimeActivity == 0 && len(a.pendingActions) == 0 && len(a.deferredActions) == 0
}

func (a *Agent) applyPendingRuntimeProfile(generationBoundary bool) {
	a.profileApplyMu.Lock()
	defer a.profileApplyMu.Unlock()

	for {
		a.mu.Lock()
		if !generationBoundary && !a.canApplyRuntimeProfileLocked() {
			a.mu.Unlock()
			return
		}
		request := a.pendingProfile
		a.pendingProfile = nil
		a.mu.Unlock()
		if request == nil {
			return
		}

		err := validateRuntimeProfile(request.profile)
		if err == nil && request.profile.BeforeApply != nil {
			err = request.profile.BeforeApply()
		}
		if err == nil {
			a.mu.Lock()
			a.applyRuntimeProfileLocked(request.profile)
			a.mu.Unlock()
		}
		request.result <- err
		close(request.result)
		if !generationBoundary {
			return
		}
	}
}

func validateRuntimeProfile(profile RuntimeProfile) error {
	if profile.Name == "" || profile.Provider == nil {
		return errors.New("runtime profile requires name and provider")
	}
	return nil
}

func (a *Agent) applyRuntimeProfileLocked(profile RuntimeProfile) {
	a.profileName = profile.Name
	a.cfg = profile.Config
	a.provider = profile.Provider
	a.classifier = profile.Classifier
	a.classifierProfileName = profile.ClassifierProfileName
	a.classifierProfile = profile.ClassifierProfile
	a.classifierUnavailableReason = profile.ClassifierUnavailableReason
	a.profileForcesManual = profile.ForceManualApproval
	a.refreshApprovalPolicy()
	if a.compressor != nil {
		a.compressor.SetContextWindow(profile.Config.ContextWindow)
	}
	if tool, ok := a.tools["spawn"].(profileAwareTool); ok {
		tool.SetProfile(profile.Config)
	}
}
