package codexapp

import (
	"context"
	"errors"
	"fmt"
	"testing"

	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

type fakeRPC struct {
	call func(string, any, any) error
}

func (f fakeRPC) Call(_ context.Context, method string, params any, result any) error {
	return f.call(method, params, result)
}

func (fakeRPC) Notify(string, any) error { return nil }

type fakeHost struct {
	rpc RPC
}

func (h fakeHost) Ensure(context.Context) (RPC, string, error) { return h.rpc, "codex-test", nil }
func (fakeHost) SocketPath() string                            { return "/tmp/codex.sock" }

func TestAdapterFindWorkspaceScansAllPagesAndPrefersMetadata(t *testing.T) {
	root := t.TempDir()
	page := 0
	host := fakeHost{rpc: fakeRPC{call: func(method string, _ any, result any) error {
		if method != "project/list" {
			return fmt.Errorf("unexpected method %s", method)
		}
		response := result.(*struct {
			Data       []project `json:"data"`
			NextCursor *string   `json:"nextCursor"`
		})
		page++
		if page == 1 {
			next := "page-2"
			response.Data = []project{{ID: "root-only", Roots: []projectRoot{{Path: root}}, Metadata: map[string]string{}}}
			response.NextCursor = &next
			return nil
		}
		response.Data = []project{{ID: "metadata", Roots: []projectRoot{{Path: root}}, Metadata: map[string]string{"zotigod.workspace_id": "workspace-1"}}}
		return nil
	}}}
	adapter := NewAdapter(host, nil)
	match, err := adapter.FindWorkspace(context.Background(), zotigoruntime.WorkspaceSpec{WorkspaceID: "workspace-1", Name: "Workspace", RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if match == nil || match.ID != "metadata" || page != 2 {
		t.Fatalf("match = %#v, pages=%d", match, page)
	}
}

func TestAdapterProbeUsesModelSlugAndScansAllPages(t *testing.T) {
	page := 0
	host := fakeHost{rpc: fakeRPC{call: func(method string, _ any, result any) error {
		if method != "model/list" {
			return fmt.Errorf("unexpected method %s", method)
		}
		response := result.(*struct {
			Data []struct {
				ID                        string `json:"id"`
				Model                     string `json:"model"`
				DisplayName               string `json:"displayName"`
				IsDefault                 bool   `json:"isDefault"`
				SupportedReasoningEfforts []struct {
					ReasoningEffort string `json:"reasoningEffort"`
				} `json:"supportedReasoningEfforts"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		})
		page++
		model := struct {
			ID                        string `json:"id"`
			Model                     string `json:"model"`
			DisplayName               string `json:"displayName"`
			IsDefault                 bool   `json:"isDefault"`
			SupportedReasoningEfforts []struct {
				ReasoningEffort string `json:"reasoningEffort"`
			} `json:"supportedReasoningEfforts"`
		}{ID: "catalog-id", Model: "gpt-5.6-luna", DisplayName: "Luna"}
		response.Data = append(response.Data, model)
		if page == 1 {
			next := "page-2"
			response.NextCursor = &next
		}
		return nil
	}}}
	capabilities, err := NewAdapter(host, nil).Probe(context.Background(), zotigoruntime.ProbeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if page != 2 || len(capabilities.Models) != 2 || capabilities.Models[0].ID != "gpt-5.6-luna" {
		t.Fatalf("capabilities=%#v pages=%d", capabilities, page)
	}
}

func TestAdapterCreateWorkspaceSendsStableProjectMetadata(t *testing.T) {
	root := t.TempDir()
	host := fakeHost{rpc: fakeRPC{call: func(method string, params any, result any) error {
		if method != "project/create" {
			return fmt.Errorf("unexpected method %s", method)
		}
		request := params.(map[string]any)
		if request["idempotencyKey"] != "stable-key" {
			return fmt.Errorf("idempotency key = %v", request["idempotencyKey"])
		}
		response := result.(*struct {
			Project project `json:"project"`
		})
		response.Project = project{
			ID: "project-1", Name: "Workspace", Roots: []projectRoot{{Path: root}},
			Metadata: map[string]string{"zotigod.workspace_id": "workspace-1"},
		}
		return nil
	}}}
	adapter := NewAdapter(host, nil)
	created, err := adapter.CreateWorkspace(context.Background(), zotigoruntime.WorkspaceCreateIntent{
		WorkspaceSpec:  zotigoruntime.WorkspaceSpec{WorkspaceID: "workspace-1", Name: "Workspace", RootPath: root},
		IdempotencyKey: "stable-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "project-1" {
		t.Fatalf("created = %#v", created)
	}
}

func TestAdapterFindWorkspaceSkipsProjectWithoutExactlyOneRoot(t *testing.T) {
	root := t.TempDir()
	host := fakeHost{rpc: fakeRPC{call: func(method string, _ any, result any) error {
		if method != "project/list" {
			return fmt.Errorf("unexpected method %s", method)
		}
		response := result.(*struct {
			Data       []project `json:"data"`
			NextCursor *string   `json:"nextCursor"`
		})
		response.Data = []project{
			{ID: "no-roots", Metadata: map[string]string{"zotigod.workspace_id": "workspace-1"}},
			{ID: "many-roots", Roots: []projectRoot{{Path: root}, {Path: root}}, Metadata: map[string]string{"zotigod.workspace_id": "workspace-1"}},
		}
		return nil
	}}}
	match, err := NewAdapter(host, nil).FindWorkspace(context.Background(), zotigoruntime.WorkspaceSpec{
		WorkspaceID: "workspace-1", Name: "Workspace", RootPath: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if match != nil {
		t.Fatalf("invalid-root Project matched: %#v", match)
	}
}

func TestAdapterCreateWorkspaceRecognizesDeletedIdempotencyKey(t *testing.T) {
	root := t.TempDir()
	host := fakeHost{rpc: fakeRPC{call: func(method string, _ any, _ any) error {
		if method != "project/create" {
			return fmt.Errorf("unexpected method %s", method)
		}
		return &RPCError{Code: -32600, Message: "idempotency key refers to deleted project"}
	}}}
	_, err := NewAdapter(host, nil).CreateWorkspace(context.Background(), zotigoruntime.WorkspaceCreateIntent{
		WorkspaceSpec:  zotigoruntime.WorkspaceSpec{WorkspaceID: "workspace-1", Name: "Workspace", RootPath: root},
		IdempotencyKey: "deleted-key",
	})
	if !errors.Is(err, zotigoruntime.ErrWorkspaceCreateTombstone) {
		t.Fatalf("create error = %v", err)
	}
}
