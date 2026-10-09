package zotigod

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jayyao97/zotigo/core/config"
)

func TestProfilesGlobalScopeExcludesDaemonProjectConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, config.ConfigDirName)
	if err := os.MkdirAll(globalDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, config.ConfigFileName), []byte("default_profile: global\nprofiles:\n  global:\n    provider: openai\n    model: global-model\n"), 0600); err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, config.ProjectConfig), []byte("default_profile: project\nprofiles:\n  project:\n    provider: openai\n    model: project-model\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	for _, test := range []struct {
		query, profile string
		count          int
	}{
		{"", "project", 2},
		{"?scope=global", "global", 1},
	} {
		rec := httptest.NewRecorder()
		NewHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config/profiles"+test.query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", test.query, rec.Code, rec.Body.String())
		}
		var response profilesResponse
		if err := decodeAPIData(t, rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.DefaultProfile != test.profile || len(response.Profiles) != test.count {
			t.Fatalf("%s: %+v", test.query, response)
		}
	}
	for _, query := range []string{"?scope=unknown", "?scope=global&working_directory=/tmp"} {
		rec := httptest.NewRecorder()
		NewHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config/profiles"+query, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", query, rec.Code)
		}
	}
}
