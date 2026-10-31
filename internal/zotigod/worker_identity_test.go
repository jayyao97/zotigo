package zotigod

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestCodexThreadIdentityOnStartAndResume(t *testing.T) {
	for _, dynamic := range []bool{false, true} {
		params := codexThreadParams(codexWorkerConfig{
			workerClientConfig: workerClientConfig{SessionID: "sess_child"},
			WorkspaceID:        "workspace_current", DeveloperInstructions: "Existing instructions.",
		}, dynamic)
		text, _ := params["developerInstructions"].(string)
		for _, want := range []string{"Existing instructions.", "session_id: sess_child", "workspace_id: workspace_current"} {
			if !strings.Contains(text, want) {
				t.Fatalf("instructions missing %q: %s", want, text)
			}
		}
	}
}

func TestWorkerIdentityComesFromCatalog(t *testing.T) {
	for _, assigned := range []bool{false, true} {
		t.Run(map[bool]string{false: "directory", true: "workspace"}[assigned], func(t *testing.T) {
			h, _, catalog, workspace := newChannelProvisionFixture(t)
			session := newSession(workspace.RootPath, "test")
			session.State = SessionStateStarting
			h.registry.Add(session)
			if assigned {
				if _, err := catalog.AssignSession(context.Background(), session.ID, workspace.ID); err != nil {
					t.Fatal(err)
				}
			}
			h.workers = newWorkerRegistry()
			server := httptest.NewServer(http.HandlerFunc(h.handleWorkerConnect))
			defer server.Close()
			conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/internal/workers/connect?session_id="+session.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			want := ""
			if assigned {
				want = workspace.ID
			}
			if got := response.Header.Get(workerWorkspaceHeader); got != want {
				t.Fatalf("workspace = %q, want %q", got, want)
			}
		})
	}
}
