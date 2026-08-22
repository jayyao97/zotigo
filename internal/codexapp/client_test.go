package codexapp

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestClientCallsJSONRPCOverUnixSocket(t *testing.T) {
	runtimeDir, err := os.MkdirTemp("/tmp", "zca-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	socketPath := filepath.Join(runtimeDir, "codex.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, upgradeErr := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if upgradeErr != nil {
			return
		}
		defer conn.Close()
		var request map[string]any
		if readErr := conn.ReadJSON(&request); readErr != nil {
			return
		}
		_ = conn.WriteJSON(map[string]any{
			"jsonrpc": "2.0", "id": request["id"],
			"result": map[string]any{"project": map[string]any{"id": "project-1"}},
		})
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := Dial(ctx, socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var response struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	if err := client.Call(ctx, "project/read", map[string]any{"projectId": "project-1"}, &response); err != nil {
		t.Fatal(err)
	}
	if response.Project.ID != "project-1" {
		t.Fatalf("project id = %q", response.Project.ID)
	}
}
