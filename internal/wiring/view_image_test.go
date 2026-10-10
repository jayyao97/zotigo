package wiring

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

type imageInspectionProvider struct {
	calls    int
	image    []byte
	disabled bool
}

func (*imageInspectionProvider) Name() string { return "image-inspection-test" }
func (p *imageInspectionProvider) StreamChat(_ context.Context, messages []protocol.Message, available []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	events := make(chan protocol.Event, 2)
	defer close(events)
	p.calls++
	if p.calls == 1 {
		found := false
		for _, tool := range available {
			found = found || tool.Name() == "view_image"
		}
		if p.disabled {
			if found {
				return nil, fmt.Errorf("view_image advertised for text-only host")
			}
			events <- protocol.NewTextDeltaEvent("No image tool available")
			events <- protocol.NewFinishEvent(protocol.FinishReasonStop)
			return events, nil
		}
		if !found {
			return nil, fmt.Errorf("view_image is not registered")
		}
		events <- protocol.Event{Type: protocol.EventTypeToolCallEnd, ToolCall: &protocol.ToolCall{ID: "image-call", Name: "view_image", Arguments: `{"path":"sample.png"}`}}
		events <- protocol.NewFinishEvent(protocol.FinishReasonToolCalls)
		return events, nil
	}
	last := messages[len(messages)-1]
	if last.Role != protocol.RoleTool {
		return nil, fmt.Errorf("image was injected as %s", last.Role)
	}
	for _, part := range last.Content {
		if part.ToolResult == nil || part.ToolResult.ToolCallID != "image-call" {
			continue
		}
		for _, content := range part.ToolResult.Content {
			if content.Image != nil {
				p.image = append([]byte(nil), content.Image.Data...)
			}
		}
	}
	if len(p.image) == 0 {
		return nil, fmt.Errorf("next model request has no image")
	}
	events <- protocol.NewTextDeltaEvent("image received")
	events <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	return events, nil
}

func TestTextOnlyHostDoesNotAdvertiseViewImage(t *testing.T) {
	provider := &imageInspectionProvider{disabled: true}
	providers.Register("image-disabled-test", func(config.ProfileConfig) (providers.Provider, error) { return provider, nil })
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: "image-disabled-test"}, exec)
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterDefaultTools(ag, ToolSetConfig{DisableViewImage: true}); err != nil {
		t.Fatal(err)
	}
	events, err := ag.Run(context.Background(), "List available tools")
	if err != nil {
		t.Fatal(err)
	}
	for event := range events {
		if event.Type == protocol.EventTypeError {
			t.Fatalf("agent error: %+v", event)
		}
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls: %d", provider.calls)
	}
}

func TestDefaultViewImageReachesNextModelRequest(t *testing.T) {
	provider := &imageInspectionProvider{}
	providers.Register("image-inspection-test", func(config.ProfileConfig) (providers.Provider, error) { return provider, nil })
	root := t.TempDir()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sample.png"), data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	exec, err := executor.NewLocalExecutor(root)
	if err != nil {
		t.Fatal(err)
	}
	defer exec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: "image-inspection-test"}, exec)
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterDefaultTools(ag, ToolSetConfig{}); err != nil {
		t.Fatal(err)
	}
	events, err := ag.Run(context.Background(), "Inspect sample.png")
	if err != nil {
		t.Fatal(err)
	}
	var finish protocol.FinishReason
	for event := range events {
		if event.Type == protocol.EventTypeError {
			t.Fatalf("agent error: %+v", event)
		}
		if event.Type == protocol.EventTypeFinish {
			finish = event.FinishReason
		}
	}
	if finish != protocol.FinishReasonStop || provider.calls != 2 || !bytes.Equal(provider.image, data.Bytes()) {
		t.Fatalf("image round trip failed: calls=%d finish=%s", provider.calls, finish)
	}
}
