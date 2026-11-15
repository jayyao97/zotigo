package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/jayyao97/zotigo/core/config"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

var (
	errWorkerConnectTimeout = errors.New("worker did not connect before timeout")
	errWorkerLaunchTimeout  = errors.New("worker did not start before timeout")
	errSessionUnavailable   = errors.New("session is unavailable")
)

func (h *handler) ensureSessionRunning(ctx context.Context, id string) (Session, error) {
	releaseWorkspace := func() {}
	if h.catalog != nil {
		organization, err := h.catalog.GetSessionOrganization(ctx, id)
		if err == nil && organization.WorkspaceID != nil {
			_, release, lockErr := h.lockWorkspaceForUse(ctx, *organization.WorkspaceID)
			err = lockErr
			releaseWorkspace = release
			if err != nil {
				return Session{}, fmt.Errorf("lock session workspace: %w", err)
			}
		} else if err != nil && !errors.Is(err, zotigoworkspace.ErrNotFound) {
			return Session{}, fmt.Errorf("load session organization: %w", err)
		}
	}
	defer releaseWorkspace()
	return h.ensureSessionRunningInWorkspace(ctx, id)
}

// The caller holds the workspace lifecycle fence, if the session is assigned.
func (h *handler) ensureSessionRunningInWorkspace(ctx context.Context, id string) (Session, error) {
	h.updateMu.RLock()
	defer h.updateMu.RUnlock()
	if h.updater != nil && h.updater.Draining() {
		return Session{}, fmt.Errorf("%w: daemon is waiting to apply an update", errSessionUnavailable)
	}
	unlock := h.sessionOps.lock(id)
	if unavailable, err := h.ensureSessionActivatable(ctx, id); err != nil {
		unlock()
		return Session{}, fmt.Errorf("check session availability: %w", err)
	} else if unavailable != "" {
		unlock()
		return Session{}, fmt.Errorf("%w: %s", errSessionUnavailable, unavailable)
	}
	session, launched, err := h.ensureSessionStartedLocked(ctx, id)
	unlock()
	if err != nil {
		return Session{}, err
	}
	if launched {
		h.launchWorkerInBackground(id, session.workerActivation)
	}
	if !h.sessionUsesWorker(session) || (!launched && session.State != SessionStateStarting) {
		session.Live = true
		return h.sessionWithStoredMetadata(ctx, session)
	}
	if err := h.waitForRunningWorker(ctx, id); err != nil {
		return Session{}, err
	}
	if running, ok := h.registry.Get(id); ok && (running.State == SessionStateRunning || running.State == SessionStatePaused) && h.workers.Has(id) {
		running.Live = true
		return h.sessionWithStoredMetadata(ctx, running)
	}
	return Session{}, errWorkerConnectTimeout
}

func (h *handler) launchWorkerInBackground(id string, activation uint64) {
	launchCtx := context.Background()
	cancelLaunch := func() {}
	if h.workerLaunchTimeout > 0 {
		launchCtx, cancelLaunch = context.WithTimeout(launchCtx, h.workerLaunchTimeout)
	}
	var launchWatchdog *time.Timer
	if h.workerLaunchTimeout > 0 {
		launchWatchdog = time.AfterFunc(h.workerLaunchTimeout, func() {
			unlock := h.sessionOps.lock(id)
			var failed Session
			var failErr error
			if session, ok := h.registry.Get(id); ok && session.workerActivation == activation && !h.workers.Has(id) {
				failed, failErr = h.registry.FailStartingWithCode(id, "worker_start_timeout", errWorkerLaunchTimeout.Error())
				cancelLaunch()
			}
			unlock()
			if failErr == nil && failed.ID != "" {
				h.dispatchSessionEnd(failed)
			}
		})
	}
	go func() {
		defer cancelLaunch()
		if err := h.launchWorker(launchCtx, id); err != nil {
			if launchWatchdog != nil {
				launchWatchdog.Stop()
			}
			unlock := h.sessionOps.lock(id)
			var failed Session
			var failErr error
			if session, ok := h.registry.Get(id); ok && session.workerActivation == activation {
				code := "worker_start_failed"
				message := fmt.Sprintf("start worker: %v", err)
				if errors.Is(err, context.DeadlineExceeded) {
					code = "worker_start_timeout"
					message = errWorkerLaunchTimeout.Error()
				}
				failed, failErr = h.registry.FailStartingWithCode(id, code, message)
			}
			unlock()
			if failErr == nil && failed.ID != "" {
				h.dispatchSessionEnd(failed)
			}
			return
		}
		if launchWatchdog != nil {
			launchWatchdog.Stop()
		}
		cancelLaunch()
		session, ok := h.registry.Get(id)
		if !ok || session.workerActivation != activation || session.State != SessionStateStarting || h.workers.Has(id) {
			return
		}
		connectCtx, cancelConnect := context.WithCancel(context.Background())
		defer cancelConnect()
		var watchdog *time.Timer
		if h.workerConnectTimeout > 0 {
			watchdog = time.AfterFunc(h.workerConnectTimeout, func() {
				unlock := h.sessionOps.lock(id)
				var failed Session
				var failErr error
				if session, ok := h.registry.Get(id); ok && session.workerActivation == activation && !h.workers.Has(id) {
					failed, failErr = h.registry.FailStartingWithCode(id, "worker_connect_timeout", errWorkerConnectTimeout.Error())
					cancelConnect()
				}
				unlock()
				if failErr == nil && failed.ID != "" {
					h.dispatchSessionEnd(failed)
				}
			})
		}
		_ = h.waitForRunningWorker(connectCtx, id)
		if watchdog != nil {
			watchdog.Stop()
		}
	}()
}

func (h *handler) waitForRunningWorker(ctx context.Context, id string) error {
	for {
		session, ok, changed := h.registry.Watch(id)
		if !ok {
			return errSessionNotFound
		}
		switch session.State {
		case SessionStateCreated:
			return errWorkerDisconnectedBeforeReady
		case SessionStateRunning, SessionStatePausing:
			if h.workers.Has(id) {
				return nil
			}
			return errWorkerDisconnectedBeforeReady
		case SessionStatePaused:
			if h.workers.Has(id) {
				return nil
			}
		case SessionStateFailed:
			if session.Error == errWorkerConnectTimeout.Error() {
				return errWorkerConnectTimeout
			}
			if session.ErrorCode == "worker_start_timeout" {
				return errWorkerLaunchTimeout
			}
			if isRuntimeOccupiedSession(session) {
				return fmt.Errorf("%w: %s", workerbase.ErrRuntimeOccupied, runtimeOccupiedMessage(session))
			}
			if session.Error != "" {
				return errors.New(session.Error)
			}
			return errors.New("worker failed to start")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (h *handler) ensureSessionStartedLocked(ctx context.Context, id string) (Session, bool, error) {
	for {
		session, ok, err := h.loadSessionIntoRegistry(ctx, id)
		if err != nil {
			return Session{}, false, fmt.Errorf("load session: %w", err)
		}
		if !ok {
			return Session{}, false, errSessionNotFound
		}

		switch session.State {
		case SessionStateRunning, SessionStatePausing:
			if h.workers.Has(id) || !h.sessionUsesWorker(session) {
				return session, false, nil
			}
			if err := h.validateSessionProfile(ctx, session); err != nil {
				return Session{}, false, err
			}
			session, err = h.registry.RestartWorker(id)
			if errors.Is(err, errInvalidSessionTransition) {
				continue
			}
			if err != nil {
				return Session{}, false, err
			}
			return session, true, nil
		case SessionStateStarting:
			if !h.sessionUsesWorker(session) {
				return Session{}, false, errInvalidSessionTransition
			}
			return session, false, nil
		case SessionStatePaused:
			if h.workers.Has(id) {
				return session, false, nil
			}
			if !h.sessionUsesWorker(session) {
				return Session{}, false, errInvalidSessionTransition
			}
			if err := h.validateSessionProfile(ctx, session); err != nil {
				return Session{}, false, err
			}
			session, err = h.registry.RestartWorker(id)
			if errors.Is(err, errInvalidSessionTransition) {
				continue
			}
			if err != nil {
				return Session{}, false, err
			}
			return session, true, nil
		case SessionStateCreated:
			if err := h.validateSessionProfile(ctx, session); err != nil {
				return Session{}, false, err
			}
			session, err = h.registry.Start(id)
			if errors.Is(err, errInvalidSessionTransition) {
				continue
			}
			if err != nil {
				return Session{}, false, err
			}
			return session, true, nil
		case SessionStateFailed:
			if !isRuntimeOccupiedSession(session) && session.ErrorCode != "worker_connect_timeout" && session.ErrorCode != "worker_start_timeout" {
				return Session{}, false, errInvalidSessionTransition
			}
			session, err = h.registry.RetryFailedWorker(id)
			if errors.Is(err, errInvalidSessionTransition) {
				continue
			}
			if err != nil {
				return Session{}, false, err
			}
			return session, true, nil
		default:
			return Session{}, false, errInvalidSessionTransition
		}
	}
}

func (h *handler) validateSessionProfile(ctx context.Context, session Session) error {
	if zotigoruntime.AgentKind(session.Agent) == zotigoruntime.AgentCodex {
		return nil
	}
	workingDirectory := session.WorkingDirectory
	if workingDirectory == "" {
		workingDirectory = h.sessionWorkingDirectory(ctx, session.ID)
	}
	appConfig, err := config.NewManager().LoadForDir(workingDirectory)
	if err != nil {
		return fmt.Errorf("load session profile configuration: %w", err)
	}
	if _, _, err := appConfig.ResolveProfile(session.ProfileName); err != nil {
		return fmt.Errorf("%w: %v", errSessionProfileNotFound, err)
	}
	return nil
}

func (h *handler) sessionWithStoredMetadata(ctx context.Context, session Session) (Session, error) {
	if h.store == nil {
		return session, nil
	}
	stored, err := h.store.Get(ctx, session.ID)
	if err != nil {
		return Session{}, fmt.Errorf("load session metadata: %w", err)
	}
	return h.decorateSession(ctx, session, stored), nil
}

func (h *handler) writeEnsureRunningError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errSessionNotFound):
		writeAPIError(w, http.StatusNotFound, "session not found")
	case errors.Is(err, errInvalidSessionTransition):
		writeAPIError(w, http.StatusConflict, "invalid session state transition")
	case errors.Is(err, errSessionUnavailable):
		writeAPIError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errWorkerConnectTimeout):
		writeAPIError(w, http.StatusServiceUnavailable, errWorkerConnectTimeout.Error())
	case errors.Is(err, errWorkerLaunchTimeout):
		writeAPIError(w, http.StatusServiceUnavailable, errWorkerLaunchTimeout.Error())
	case errors.Is(err, errWorkerDisconnectedBeforeReady):
		writeAPIError(w, http.StatusServiceUnavailable, errWorkerDisconnectedBeforeReady.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeAPIError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, errSessionProfileNotFound):
		writeAPIErrorCode(w, http.StatusConflict, "profile_not_found", err.Error())
	case errors.Is(err, workerbase.ErrRuntimeOccupied):
		writeAPIErrorCode(w, http.StatusConflict, "runtime_occupied", err.Error())
	default:
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("start session: %v", err))
	}
}

func (h *handler) launchWorker(ctx context.Context, id string) error {
	spec, err := h.runtimeLaunchSpec(ctx, id)
	if err != nil {
		return err
	}
	adapter, err := h.runtimes.adapter(spec.Agent)
	if err != nil {
		return err
	}
	return adapter.StartWorker(ctx, spec)
}

func (h *handler) sessionUsesWorker(session Session) bool {
	agentKind := zotigoruntime.AgentKind(session.Agent)
	if agentKind == "" {
		agentKind = zotigoruntime.AgentZotigo
	}
	if agentKind == zotigoruntime.AgentZotigo {
		return h.launcher != nil
	}
	_, err := h.runtimes.adapter(agentKind)
	return err == nil
}

func (h *handler) sessionWorkingDirectory(ctx context.Context, id string) string {
	if session, ok := h.registry.Get(id); ok && session.WorkingDirectory != "" {
		return session.WorkingDirectory
	}
	if h.store != nil {
		if session, err := h.store.Get(ctx, id); err == nil && session != nil && session.WorkingDirectory != "" {
			return session.WorkingDirectory
		}
	}
	cwd, _ := os.Getwd()
	return cwd
}

func (h *handler) waitForWorker(ctx context.Context, id string) bool {
	if h.workerConnectTimeout <= 0 {
		return h.workers.Has(id)
	}
	waitCtx, cancel := context.WithTimeout(ctx, h.workerConnectTimeout)
	defer cancel()
	return h.workers.Wait(waitCtx.Done(), id)
}
