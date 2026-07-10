package zotigod

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/jayyao97/zotigo/core/config"
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
	if _, ok := appConfig.Profiles[appConfig.DefaultProfile]; !ok {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("default profile %q not found", appConfig.DefaultProfile))
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
