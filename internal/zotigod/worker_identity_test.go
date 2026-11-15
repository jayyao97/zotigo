package zotigod

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

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
			if got := response.Header.Get(wire.WorkspaceHeader); got != want {
				t.Fatalf("workspace = %q, want %q", got, want)
			}
		})
	}
}
