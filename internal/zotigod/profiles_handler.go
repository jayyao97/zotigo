package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/config"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

type publicProfile struct {
	Name          string `json:"name"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	ThinkingLevel string `json:"thinking_level,omitempty"`
}

type profilesResponse struct {
	DefaultProfile string          `json:"default_profile"`
	Profiles       []publicProfile `json:"profiles"`
}

type changeProfileRequest struct {
	Profile string `json:"profile"`
}

type changeProfileResponse struct {
	Profile   string `json:"profile"`
	Status    string `json:"status"`
	CommandID string `json:"command_id,omitempty"`
}

type sessionProfileUpdater interface {
	UpdateProfile(ctx context.Context, id string, profileName string, updatedAt time.Time) error
}

func persistSessionProfile(ctx context.Context, store zotigosession.Store, sess *zotigosession.Session) error {
	if updater, ok := store.(sessionProfileUpdater); ok {
		return updater.UpdateProfile(ctx, sess.ID, sess.ProfileName, sess.UpdatedAt)
	}
	return store.Put(ctx, sess)
}

func (h *handler) handleProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	workingDirectory, err := resolveWorkingDirectory(r.URL.Query().Get("working_directory"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	appConfig, err := config.NewManager().LoadForDir(workingDirectory)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load profiles: %v", err))
		return
	}
	if _, _, err := appConfig.ResolveProfile(""); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "default "+err.Error())
		return
	}

	profiles := make([]publicProfile, 0, len(appConfig.Profiles))
	for name, profile := range appConfig.Profiles {
		profiles = append(profiles, publicProfile{
			Name:          name,
			Provider:      profile.Provider,
			Model:         profile.Model,
			ThinkingLevel: profile.ThinkingLevel,
		})
	}
	sort.Slice(profiles, func(i, j int) bool {
		return profiles[i].Name < profiles[j].Name
	})

	writeAPIJSON(w, http.StatusOK, profilesResponse{
		DefaultProfile: appConfig.DefaultProfile,
		Profiles:       profiles,
	})
}

func (h *handler) handleSessionProfile(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req changeProfileRequest
	if err := readRequiredJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	target := strings.TrimSpace(req.Profile)
	if target == "" {
		writeAPIError(w, http.StatusBadRequest, "profile is required")
		return
	}
	unlockOperation := h.sessionOps.lock(id)
	defer unlockOperation()

	session, live := h.registry.Get(id)
	if !live {
		var ok bool
		var err error
		session, ok, err = h.storedSession(r.Context(), id)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load session: %v", err))
			return
		}
		if !ok {
			writeAPIError(w, http.StatusNotFound, "session not found")
			return
		}
	}
	workingDirectory := session.WorkingDirectory
	if workingDirectory == "" {
		workingDirectory = h.sessionWorkingDirectory(r.Context(), id)
	}
	exists, err := profileExistsForDirectory(workingDirectory, target)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load profiles: %v", err))
		return
	}
	if !exists {
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("profile %q not found", target))
		return
	}

	applyWithoutWorker := !live || session.State == SessionStateCreated ||
		(session.State == SessionStateRunning && !h.workers.Has(id))
	if applyWithoutWorker {
		if err := h.applyStoredProfile(r.Context(), id, target); err != nil {
			if errors.Is(err, zotigosession.ErrSessionLocked) {
				writeAPIErrorCode(w, http.StatusConflict, "session_in_use", "session is active in another process")
				return
			}
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("change profile: %v", err))
			return
		}
		if live {
			_, _ = h.registry.UpdateProfile(id, target)
		}
		writeAPIJSON(w, http.StatusOK, changeProfileResponse{Profile: target, Status: "applied"})
		return
	}
	if session.State == SessionStateEnded || session.State == SessionStateFailed {
		writeAPIError(w, http.StatusConflict, "profile change requires a resumable session")
		return
	}
	workerOnline := h.workers.Has(id)
	if !workerOnline && session.State == SessionStateStarting {
		workerOnline = h.waitForWorker(r.Context(), id)
	} else if !workerOnline {
		workerOnline = h.ensureWorkerOnline(r.Context(), id)
	}
	if !workerOnline {
		writeAPIError(w, http.StatusServiceUnavailable, "profile change requires an online worker")
		return
	}
	item, err := h.items.AppendItem(r.Context(), id, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemSessionCommand,
		Command: &zotigosession.DisplayCommand{
			Type:    sessionCommandProfile,
			Profile: target,
		},
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("append profile command: %v", err))
		return
	}
	h.sendCommand(r.Context(), id, profileCommandFromItem(item))
	writeAPIJSON(w, http.StatusAccepted, changeProfileResponse{Profile: target, Status: "pending", CommandID: item.ID})
}

func profileExistsForDirectory(workingDirectory string, profileName string) (bool, error) {
	appConfig, err := config.NewManager().LoadForDir(workingDirectory)
	if err != nil {
		return false, err
	}
	_, _, err = appConfig.ResolveProfile(profileName)
	return err == nil, nil
}

func (h *handler) applyStoredProfile(ctx context.Context, id string, profileName string) error {
	if h.store == nil {
		return fmt.Errorf("session store is not configured")
	}
	if err := h.store.Lock(ctx, id); err != nil {
		return err
	}
	applyErr := h.applyStoredProfileLocked(ctx, id, profileName)
	unlockErr := h.store.Unlock(context.Background(), id)
	if unlockErr != nil {
		unlockErr = fmt.Errorf("unlock session: %w", unlockErr)
	}
	return errors.Join(applyErr, unlockErr)
}

func (h *handler) applyStoredProfileLocked(ctx context.Context, id string, profileName string) error {
	sess, err := h.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if sess == nil {
		return fmt.Errorf("session not found")
	}
	from := sess.ProfileName
	previousUpdatedAt := sess.UpdatedAt
	if from == profileName {
		return h.ensureStoredProfileMarker(ctx, id, profileName)
	}
	sess.ProfileName = profileName
	sess.UpdatedAt = time.Now().UTC()
	if err := persistSessionProfile(ctx, h.store, sess); err != nil {
		if errors.Is(err, zotigosession.ErrProfileStateUncertain) {
			return &profileStateUncertainError{cause: err}
		}
		return err
	}
	if _, err := h.items.AppendItem(ctx, id, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemProfileChanged,
		Profile: &zotigosession.DisplayProfileChange{
			From: from,
			To:   profileName,
		},
	}); err != nil {
		sess.ProfileName = from
		sess.UpdatedAt = previousUpdatedAt
		if rollbackErr := persistSessionProfile(ctx, h.store, sess); rollbackErr != nil {
			return &profileStateUncertainError{cause: errors.Join(
				fmt.Errorf("append profile changed: %w", err),
				fmt.Errorf("rollback session profile: %w", rollbackErr),
			)}
		}
		return err
	}
	return nil
}

func (h *handler) ensureStoredProfileMarker(ctx context.Context, id string, profileName string) error {
	items, _, err := h.items.LoadItems(ctx, id)
	if err != nil {
		return err
	}

scanLatestProfileResult:
	for idx := len(items) - 1; idx >= 0; idx-- {
		item := items[idx]
		switch item.Type {
		case zotigosession.DisplayItemProfileChanged:
			if item.Profile != nil && item.Profile.To == profileName {
				return nil
			}
			break scanLatestProfileResult
		case zotigosession.DisplayItemProfileFailed:
			break scanLatestProfileResult
		}
	}
	_, err = h.items.AppendItem(ctx, id, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemProfileChanged,
		Profile: &zotigosession.DisplayProfileChange{
			From: profileName,
			To:   profileName,
		},
	})
	return err
}
