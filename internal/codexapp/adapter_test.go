package codexapp

import (
	"context"
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
	rpc      RPC
	releases int
}

func (h *fakeHost) Acquire(context.Context) (*Lease, error) {
	return &Lease{
		RPC: h.rpc, Version: "codex-test", SocketPath: "/tmp/codex.sock",
		release: func() error {
			h.releases++
			return nil
		},
	}, nil
}

func TestAdapterProbeUsesModelSlugAndScansAllPages(t *testing.T) {
	page := 0
	host := &fakeHost{rpc: fakeRPC{call: func(method string, _ any, result any) error {
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
	if host.releases != 1 {
		t.Fatalf("host releases = %d, want 1", host.releases)
	}
}

func TestAdapterWorkerReleasesHostLeaseOnProcessExit(t *testing.T) {
	host := &fakeHost{rpc: fakeRPC{call: func(string, any, any) error { return nil }}}
	var onExit func()
	adapter := NewAdapter(host, func(_ context.Context, _ zotigoruntime.WorkerLaunchSpec, socketPath string, release func()) error {
		if socketPath != "/tmp/codex.sock" {
			t.Fatalf("socket path = %q", socketPath)
		}
		onExit = release
		return nil
	})
	if err := adapter.StartWorker(context.Background(), zotigoruntime.WorkerLaunchSpec{}); err != nil {
		t.Fatal(err)
	}
	if host.releases != 0 {
		t.Fatalf("host released before worker exit")
	}
	onExit()
	if host.releases != 1 {
		t.Fatalf("host releases = %d, want 1", host.releases)
	}
}

func TestAdapterWorkerStartFailureReleasesHostLease(t *testing.T) {
	host := &fakeHost{rpc: fakeRPC{call: func(string, any, any) error { return nil }}}
	adapter := NewAdapter(host, func(context.Context, zotigoruntime.WorkerLaunchSpec, string, func()) error {
		return fmt.Errorf("start failed")
	})
	if err := adapter.StartWorker(context.Background(), zotigoruntime.WorkerLaunchSpec{}); err == nil {
		t.Fatal("worker start succeeded")
	}
	if host.releases != 1 {
		t.Fatalf("host releases = %d, want 1", host.releases)
	}
}
