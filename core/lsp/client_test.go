package lsp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"go.lsp.dev/protocol"
)

type initializationServer struct {
	protocol.Server
	params      *protocol.InitializeParams
	initialized bool
}

func (s *initializationServer) Initialize(_ context.Context, params *protocol.InitializeParams) (*protocol.InitializeResult, error) {
	s.params = params
	return &protocol.InitializeResult{}, nil
}

func (s *initializationServer) Initialized(context.Context, *protocol.InitializedParams) error {
	s.initialized = true
	return nil
}

func TestInitializeWorkspaceFolders(t *testing.T) {
	root := t.TempDir()
	options := map[string]interface{}{"example": true}
	client := NewClient(ServerConfig{InitOptions: options}, root)
	server := &initializationServer{}
	client.server = server
	if err := client.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []protocol.WorkspaceFolder{{URI: string(client.rootURI), Name: filepath.Base(root)}}
	if !reflect.DeepEqual(server.params.WorkspaceFolders, want) {
		t.Fatalf("initialize folders = %#v, want %#v", server.params.WorkspaceFolders, want)
	}
	if server.params.Capabilities.Workspace == nil || !server.params.Capabilities.Workspace.WorkspaceFolders {
		t.Fatal("workspace folder capability not advertised")
	}
	if !server.initialized || !reflect.DeepEqual(server.params.InitializationOptions, options) {
		t.Fatal("initialization notification or options lost")
	}
	encoded, err := json.Marshal(server.params)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	var rootURI string
	if err := json.Unmarshal(wire["rootUri"], &rootURI); err != nil || rootURI != want[0].URI {
		t.Fatalf("legacy rootUri = %q, %v; want %q", rootURI, err, want[0].URI)
	}
	if _, exists := wire["workspaceFolders"]; !exists {
		t.Fatal("workspaceFolders missing on wire")
	}
	handler := &clientHandler{client: client}
	folders, err := handler.WorkspaceFolders(context.Background())
	if err != nil || !reflect.DeepEqual(folders, want) {
		t.Fatalf("workspace query = %#v, %v; want %#v", folders, err, want)
	}
}
