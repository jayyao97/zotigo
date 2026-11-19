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
	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

type publicProfile struct {
	Name            string `json:"name"`
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	ThinkingLevel   string `json:"thinking_level,omitempty"`
	MaxOutputTokens int64  `json:"max_output_tokens"`
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

func (h *handler) handleProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	query := r.URL.Query()
	scope := query.Get("scope")
	if scope != "" && scope != "global" {
		writeAPIError(w, http.StatusBadRequest, "scope must be global when specified")
		return
	}
	workingDirectory := ""
	if scope == "global" {
		if query.Has("working_directory") {
			writeAPIError(w, http.StatusBadRequest, "global scope cannot include working_directory")
			return
		}
	} else {
		var err error
		workingDirectory, err = resolveWorkingDirectory(query.Get("working_directory"))
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	appConfig, err := config.NewManager().LoadForDir(workingDirectory)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load profiles: %v", err))
		return
	}
	defaultProfileName := ""
	if strings.TrimSpace(appConfig.DefaultProfile) != "" {
		defaultProfileName, _, err = appConfig.ResolveProfile("")
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "default "+err.Error())
			return
		}
	}

	profiles := make([]publicProfile, 0, len(appConfig.Profiles))
	for name, profile := range appConfig.Profiles {
		maxOutputTokens, err := profile.EffectiveMaxOutputTokens()
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("profile %q: %v", name, err))
			return
		}
		profiles = append(profiles, publicProfile{
			Name:            name,
			Provider:        profile.Provider,
			Model:           profile.Model,
			ThinkingLevel:   profile.ThinkingLevel,
			MaxOutputTokens: maxOutputTokens,
		})
	}
	sort.Slice(profiles, func(i, j int) bool {
		return profiles[i].Name < profiles[j].Name
	})

	writeAPIJSON(w, http.StatusOK, profilesResponse{
		DefaultProfile: defaultProfileName,
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
	channelBound := false
	if h.channels != nil {
		bound, release, err := h.channels.GuardSessionProfileChange(r.Context(), id)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load channel binding: %v", err))
			return
		}
		defer release()
		channelBound = bound
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
	if session.Agent == string(zotigoruntime.AgentCodex) {
		writeAPIError(w, http.StatusBadRequest, "profile is only valid for agent zotigo")
		return
	}
	if channelBound {
		items, _, err := h.items.LoadItems(r.Context(), id)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load session history: %v", err))
			return
		}
		if session.Working || requireIdleSession(items) != nil || sessionlog.HasPendingHumanRequest(items) {
			writeAPIError(w, http.StatusConflict, "wait for the current Channel turn to finish before changing profile")
			return
		}
	}
	workingDirectory := session.WorkingDirectory
	if workingDirectory == "" {
		workingDirectory = h.sessionWorkingDirectory(r.Context(), id)
	}
	canonicalTarget, found, err := resolveProfileForDirectory(workingDirectory, target)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load profiles: %v", err))
		return
	}
	if !found {
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("profile %q not found", target))
		return
	}
	target = canonicalTarget

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
			Type:        wire.CommandProfile,
			Profile:     target,
			RequireIdle: channelBound,
		},
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("append profile command: %v", err))
		return
	}
	h.sendCommand(r.Context(), id, sessionlog.ProfileCommandFromItem(item))
	writeAPIJSON(w, http.StatusAccepted, changeProfileResponse{Profile: target, Status: "pending", CommandID: item.ID})
}

func resolveProfileForDirectory(workingDirectory string, profileName string) (string, bool, error) {
	appConfig, err := config.NewManager().LoadForDir(workingDirectory)
	if err != nil {
		return "", false, err
	}
	canonicalName, _, err := appConfig.ResolveProfile(profileName)
	if err != nil {
		return "", false, nil
	}
	return canonicalName, true, nil
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
	if err := workerbase.PersistSessionProfile(ctx, h.store, sess); err != nil {
		if errors.Is(err, zotigosession.ErrProfileStateUncertain) {
			return &workerbase.ProfileStateUncertainError{Cause: err}
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
		if rollbackErr := workerbase.PersistSessionProfile(ctx, h.store, sess); rollbackErr != nil {
			return &workerbase.ProfileStateUncertainError{Cause: errors.Join(
				fmt.Errorf("append profile changed: %w", err),
				fmt.Errorf("rollback session profile: %w", rollbackErr),
			)}
		}
		return err
	}
	if _, err := h.repairPendingProfileCommands(ctx, id, profileName); err != nil {
		return &workerbase.ProfileStateUncertainError{Cause: fmt.Errorf("repair pending profile commands: %w", err)}
	}
	return nil
}

func (h *handler) ensureStoredProfileMarker(ctx context.Context, id string, profileName string) error {
	items, _, err := h.items.LoadItems(ctx, id)
	if err != nil {
		return err
	}
	if !hasOfflineProfileIntent(items, profileName) {
		if _, err = h.items.AppendItem(ctx, id, zotigosession.DisplayItem{
			Type: zotigosession.DisplayItemProfileChanged,
			Profile: &zotigosession.DisplayProfileChange{
				From: profileName,
				To:   profileName,
			},
		}); err != nil {
			return err
		}
	}
	_, err = h.repairPendingProfileCommandsFromItems(ctx, id, profileName, items)
	if err != nil {
		return &workerbase.ProfileStateUncertainError{Cause: fmt.Errorf("repair pending profile commands: %w", err)}
	}
	return nil
}

func hasOfflineProfileIntent(items []zotigosession.DisplayItem, profileName string) bool {
	var latestProfileCommand uint64
	var latestIntent uint64
	var latestIntentProfile string
	for _, item := range items {
		if item.Command != nil && item.Command.Type == wire.CommandProfile {
			latestProfileCommand = item.Sequence
		}
		if item.Type == zotigosession.DisplayItemProfileChanged && item.Profile != nil && item.Profile.CommandID == "" {
			latestIntent = item.Sequence
			latestIntentProfile = item.Profile.To
		}
	}
	return latestIntent > latestProfileCommand && latestIntentProfile == profileName
}

func (h *handler) repairPendingProfileCommands(ctx context.Context, id string, profileName string) (bool, error) {
	items, _, err := h.items.LoadItems(ctx, id)
	if err != nil {
		return false, err
	}
	return h.repairPendingProfileCommandsFromItems(ctx, id, profileName, items)
}

func (h *handler) repairPendingProfileCommandsFromItems(ctx context.Context, id string, profileName string, items []zotigosession.DisplayItem) (bool, error) {
	completed := make(map[string]bool)
	for _, item := range items {
		if (item.Type == zotigosession.DisplayItemProfileChanged || item.Type == zotigosession.DisplayItemProfileFailed) && item.Profile != nil {
			completed[item.Profile.CommandID] = item.Profile.CommandID != ""
		}
	}
	hadPending := false
	for _, item := range items {
		if item.Command == nil || item.Command.Type != wire.CommandProfile || completed[item.ID] {
			continue
		}
		hadPending = true
		_, err := h.items.AppendItem(ctx, id, zotigosession.DisplayItem{
			Type:  zotigosession.DisplayItemProfileFailed,
			Error: "superseded by offline profile change",
			Profile: &zotigosession.DisplayProfileChange{
				CommandID: item.ID,
				From:      profileName,
				To:        item.Command.Profile,
			},
		})
		if err != nil {
			return hadPending, err
		}
	}
	return hadPending, nil
}
