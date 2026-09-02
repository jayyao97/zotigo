package zotigod

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jayyao97/zotigo/core/skills"
)

type publicSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Scope       string `json:"scope"`
	Enabled     bool   `json:"enabled"`
}

type publicSkillDiagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Scope   string `json:"scope,omitempty"`
	Name    string `json:"name,omitempty"`
}

type skillsResponse struct {
	Skills      []publicSkill           `json:"skills"`
	Diagnostics []publicSkillDiagnostic `json:"diagnostics"`
}

func (h *handler) handleSkills(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	forceReload := false
	if raw := r.URL.Query().Get("force_reload"); raw != "" {
		var err error
		forceReload, err = strconv.ParseBool(raw)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "force_reload must be true or false")
			return
		}
	}
	sessionID := strings.TrimSpace(r.URL.Query().Get("session_id"))
	if !validSkillsSessionID(sessionID) {
		writeAPIError(w, http.StatusBadRequest, "invalid session_id")
		return
	}
	workspace, err := h.skillsWorkspace(r.Context(), sessionID)
	if err != nil {
		if errors.Is(err, errSessionNotFound) {
			writeAPIError(w, http.StatusNotFound, "session not found")
		} else {
			h.logSkillsError("load session", sessionID, err)
			writeAPIError(w, http.StatusInternalServerError, "failed to load session")
		}
		return
	}
	manager, err := h.skillManager(workspace, forceReload)
	if err != nil {
		h.logSkillsError("load skills", sessionID, err)
		writeAPIError(w, http.StatusInternalServerError, "failed to load skills")
		return
	}
	response := skillsResponse{Skills: make([]publicSkill, 0), Diagnostics: make([]publicSkillDiagnostic, 0)}
	for _, skill := range manager.List() {
		response.Skills = append(response.Skills, publicSkill{
			Name: skill.Name, Description: skill.Description, Scope: skill.Source.String(), Enabled: skill.IsEnabled(),
		})
	}
	for _, diagnostic := range manager.Diagnostics() {
		response.Diagnostics = append(response.Diagnostics, publicSkillDiagnostic{
			Code: diagnostic.Code, Message: publicSkillDiagnosticMessage(diagnostic), Scope: diagnostic.Scope, Name: diagnostic.Name,
		})
	}
	writeAPIJSON(w, http.StatusOK, response)
}

func (h *handler) logSkillsError(operation, sessionID string, err error) {
	if h.logger != nil {
		h.logger.Printf("%s for session %q: %v", operation, sessionID, err)
	}
}

func validSkillsSessionID(sessionID string) bool {
	return sessionID == "" || (sessionID != "." && sessionID != ".." && !strings.ContainsAny(sessionID, `/\`))
}

func publicSkillDiagnosticMessage(diagnostic skills.Diagnostic) string {
	switch diagnostic.Code {
	case "scan_failed":
		return "failed to scan skill directory"
	case "invalid_skill":
		return "SKILL.md is invalid"
	case "missing_skill_file":
		return "skill directory is missing SKILL.md"
	}
	return diagnostic.Message
}

func (h *handler) skillsWorkspace(ctx context.Context, sessionID string) (string, error) {
	if sessionID == "" {
		return "", nil
	}
	if h.store != nil {
		stored, err := h.store.Get(ctx, sessionID)
		if err != nil {
			return "", err
		}
		if stored != nil {
			if stored.ID != sessionID {
				return "", errSessionNotFound
			}
			return strings.TrimSpace(stored.WorkingDirectory), nil
		}
	}
	session, ok := h.registry.Get(sessionID)
	if !ok {
		return "", errSessionNotFound
	}
	return strings.TrimSpace(session.WorkingDirectory), nil
}

func (h *handler) skillManager(workspace string, forceReload bool) (*skills.SkillManager, error) {
	workspace = strings.TrimSpace(workspace)
	h.skillsMu.Lock()
	if h.skillManagers == nil {
		h.skillManagers = make(map[string]*skills.SkillManager)
	}
	manager := h.skillManagers[workspace]
	if manager == nil {
		manager = skills.NewSkillManager(workspace)
		h.skillManagers[workspace] = manager
	}
	h.skillsMu.Unlock()
	if forceReload {
		return manager, manager.Reload()
	}
	return manager, manager.EnsureLoaded()
}

func (h *handler) resolveSessionSkills(ctx context.Context, sessionID string, names []string) ([]*skills.SkillDefinition, error) {
	workspace, err := h.skillsWorkspace(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	manager, err := h.skillManager(workspace, true)
	if err != nil {
		return nil, err
	}
	return manager.ResolveExplicit(names)
}
