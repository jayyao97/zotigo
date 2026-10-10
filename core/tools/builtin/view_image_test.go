package builtin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/tools"
)

func TestViewImage(t *testing.T) {
	root := t.TempDir()
	exec, err := executor.NewLocalExecutor(root)
	if err != nil {
		t.Fatal(err)
	}
	defer exec.Close()
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	// Content, rather than the extension, determines the media type.
	if err := os.WriteFile(filepath.Join(root, "image.bin"), pngData.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	tool := &ViewImageTool{}
	value, err := tool.Execute(context.Background(), exec, `{"path":"image.bin"}`)
	if err != nil {
		t.Fatal(err)
	}
	result := value.(protocol.ToolResult)
	if result.Type != protocol.ToolResultTypeContent || len(result.Content) != 2 {
		t.Fatalf("result: %+v", result)
	}
	media := result.Content[1].Image
	if media == nil || media.MediaType != "image/png" || !bytes.Equal(media.Data, pngData.Bytes()) {
		t.Fatal("image bytes were not preserved")
	}
	for _, path := range []string{"", "missing.png", ".", "text.png", "large.png", "broken.png"} {
		t.Run(path, func(t *testing.T) {
			if path == "text.png" || path == "broken.png" || path == "large.png" {
				data := []byte("plain text")
				if path == "broken.png" {
					data = []byte("\x89PNG\r\n\x1a\n")
				}
				if path == "large.png" {
					data = make([]byte, maxViewImageBytes+1)
				}
				if err := os.WriteFile(filepath.Join(root, path), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			args, _ := json.Marshal(map[string]string{"path": path})
			if _, err := tool.Execute(context.Background(), exec, string(args)); err == nil {
				t.Fatal("invalid image accepted")
			}
		})
	}
	decision := tool.Classify(tools.SafetyCall{Arguments: `{"path":"image.bin"}`, WorkDir: root, Executor: exec})
	if decision.Level != tools.LevelSafe {
		t.Fatalf("in-scope image: %+v", decision)
	}
	outside, _ := json.Marshal(map[string]string{"path": filepath.Join(t.TempDir(), "image.png")})
	decision = tool.Classify(tools.SafetyCall{Arguments: string(outside), WorkDir: root, Executor: exec})
	if decision.Level == tools.LevelSafe {
		t.Fatal("out-of-scope image bypassed read policy")
	}
}

func TestViewImageRejectsIncompleteAndOversizedImages(t *testing.T) {
	var pngData, jpegData bytes.Buffer
	picture := image.NewRGBA(image.Rect(0, 0, 2, 2))
	if err := png.Encode(&pngData, picture); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&jpegData, picture, nil); err != nil {
		t.Fatal(err)
	}
	// Preserve the JPEG header so DecodeConfig succeeds, but remove scan data.
	jpegEnd := bytes.Index(jpegData.Bytes(), []byte{0xff, 0xda})
	if jpegEnd < 0 {
		t.Fatal("missing JPEG scan")
	}
	jpegEnd += 2 + int(binary.BigEndian.Uint16(jpegData.Bytes()[jpegEnd+2:]))
	root := t.TempDir()
	exec, err := executor.NewLocalExecutor(root)
	if err != nil {
		t.Fatal(err)
	}
	defer exec.Close()
	for name, data := range map[string][]byte{
		"truncated.png": pngData.Bytes()[:33],
		"truncated.jpg": jpegData.Bytes()[:jpegEnd],
		"invalid.webp":  []byte("RIFFxxxxWEBPVP8 "),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
				t.Fatal(err)
			}
			args, _ := json.Marshal(map[string]string{"path": name})
			result, err := (&ViewImageTool{}).Execute(context.Background(), exec, string(args))
			if err == nil || result != nil {
				t.Fatal("incomplete image returned as model content")
			}
		})
	}
	for _, dimensions := range [][2]uint32{{8193, 1}, {5000, 5000}} {
		header := append([]byte(nil), pngData.Bytes()[:33]...)
		binary.BigEndian.PutUint32(header[16:20], dimensions[0])
		binary.BigEndian.PutUint32(header[20:24], dimensions[1])
		binary.BigEndian.PutUint32(header[29:33], crc32.ChecksumIEEE(header[12:29]))
		if err := os.WriteFile(filepath.Join(root, "oversized.png"), header, 0600); err != nil {
			t.Fatal(err)
		}
		_, err := (&ViewImageTool{}).Execute(context.Background(), exec, `{"path":"oversized.png"}`)
		if err == nil || !strings.Contains(err.Error(), "dimension or pixel limits") {
			t.Fatalf("dimensions not bounded before decoding: %v", err)
		}
	}
}

func TestViewImageWebP(t *testing.T) {
	// A complete one-pixel WebP, kept inline to avoid an external fixture dependency.
	data, err := base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "sample.webp"), data, 0600); err != nil {
		t.Fatal(err)
	}
	exec, err := executor.NewLocalExecutor(root)
	if err != nil {
		t.Fatal(err)
	}
	defer exec.Close()
	result, err := (&ViewImageTool{}).Execute(context.Background(), exec, `{"path":"sample.webp"}`)
	if err != nil {
		t.Fatal(err)
	}
	media := result.(protocol.ToolResult).Content[1].Image
	if media.MediaType != "image/webp" || !bytes.Equal(media.Data, data) {
		t.Fatal("WebP bytes changed")
	}
}
