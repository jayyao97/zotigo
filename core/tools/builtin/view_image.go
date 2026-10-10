package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"strings"

	_ "golang.org/x/image/webp"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/tools"
)

const maxViewImageBytes = 5 << 20
const maxViewImageDimension = 8192
const maxViewImagePixels = 16 << 20

// ViewImageTool returns image content, not a textual encoding of the file.
type ViewImageTool struct{}

func (*ViewImageTool) Name() string { return "view_image" }
func (*ViewImageTool) Description() string {
	return "View a PNG, JPEG, or WebP image from the execution environment's filesystem. Returns the image to the model for visual inspection. Use an absolute or working-directory-relative path. Limits: 5 MiB, 8192 pixels per side, and 16 megapixels. Resize larger images first."
}
func (*ViewImageTool) Schema() any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"path": map[string]any{"type": "string", "description": "Path to the image file"}},
		"required":             []string{"path"},
		"additionalProperties": false,
	}
}
func (*ViewImageTool) Classify(call tools.SafetyCall) tools.SafetyDecision {
	return tools.ReadOnlyScope("path")(call)
}
func (*ViewImageTool) Execute(ctx context.Context, exec executor.Executor, argsJSON string) (any, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Path) == "" {
		return nil, fmt.Errorf("path is required")
	}
	info, err := exec.Stat(ctx, args.Path)
	if err != nil {
		return nil, fmt.Errorf("stat image: %w", err)
	}
	if info.IsDir || (info.Mode != 0 && !info.Mode.IsRegular()) {
		return nil, fmt.Errorf("image must be a regular file")
	}
	if info.Size > maxViewImageBytes {
		return nil, fmt.Errorf("image exceeds 5 MiB")
	}
	data, err := exec.ReadFile(ctx, args.Path)
	if err != nil {
		return nil, fmt.Errorf("read image: %w", err)
	}
	if len(data) > maxViewImageBytes {
		return nil, fmt.Errorf("image exceeds 5 MiB")
	}
	mime := http.DetectContentType(data)
	switch mime {
	case "image/png", "image/jpeg", "image/webp":
	default:
		return nil, fmt.Errorf("unsupported image content %q; use PNG, JPEG, or WebP", mime)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("invalid image: %w", err)
	}
	// Bound decoded memory before validating the full image. Header-only checks
	// accept truncated files that would poison subsequent provider requests.
	if config.Width <= 0 || config.Height <= 0 || config.Width > maxViewImageDimension || config.Height > maxViewImageDimension || config.Width*config.Height > maxViewImagePixels {
		return nil, fmt.Errorf("image exceeds dimension or pixel limits; resize to at most 8192 pixels per side and 16 megapixels")
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		return nil, fmt.Errorf("invalid image: %w", err)
	}
	return protocol.ToolResult{
		Type: protocol.ToolResultTypeContent,
		Content: []protocol.ToolResultContentPart{
			{Type: protocol.ContentTypeText, Text: "Image: " + args.Path},
			{Type: protocol.ContentTypeImage, Image: &protocol.MediaPart{Data: data, MediaType: mime}},
		},
	}, nil
}
