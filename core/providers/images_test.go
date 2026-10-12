package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
)

func testPNG(t *testing.T, width, height int, noise bool) []byte {
	t.Helper()
	pixels := image.NewNRGBA(image.Rect(0, 0, width, height))
	var state uint32 = 12345
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			state = 1664525*state + 1013904223
			pixel := color.NRGBA{R: 20, G: 40, B: 60, A: 128}
			if noise {
				pixel = color.NRGBA{R: byte(state >> 24), G: byte(state >> 16), B: byte(state >> 8), A: 255}
			}
			pixels.SetNRGBA(x, y, pixel)
		}
	}
	var data bytes.Buffer
	if err := png.Encode(&data, pixels); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestPrepareImageBoundedPreview(t *testing.T) {
	data := testPNG(t, 640, 480, true)
	for level := 0; level < 3; level++ {
		prepared, err := prepareImage(data, level)
		if err != nil {
			t.Fatal(err)
		}
		decoded, format, err := image.Decode(bytes.NewReader(prepared.Data))
		if err != nil {
			t.Fatal(err)
		}
		if len(prepared.Data) > (1<<20)>>level || decoded.Bounds().Dx() > 2048>>level || decoded.Bounds().Dy() > 2048>>level {
			t.Fatalf("unbounded preview level=%d bytes=%d bounds=%v", level, len(prepared.Data), decoded.Bounds())
		}
		if prepared.MediaType != "image/"+format {
			t.Fatal("incorrect MIME")
		}
	}
	transparent := testPNG(t, 2400, 400, false)
	prepared, err := prepareImage(transparent, 0)
	if err != nil {
		t.Fatal(err)
	}
	decoded, format, err := image.Decode(bytes.NewReader(prepared.Data))
	if err != nil || format != "png" {
		t.Fatalf("lossless preview: %v %s", err, format)
	}
	_, _, _, alpha := decoded.At(0, 0).RGBA()
	if alpha == 65535 {
		t.Fatal("transparency lost for a small PNG")
	}
	if _, err := prepareImage(data[:len(data)/2], 0); err == nil {
		t.Fatal("accepted truncated image")
	}
}

func TestPrepareImagePreservesJPEGOrientationMetadata(t *testing.T) {
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2400, 100)), nil); err != nil {
		t.Fatal(err)
	}
	exifData := []byte("Exif\x00\x00II\x2a\x00\x08\x00\x00\x00\x01\x00\x12\x01\x03\x00\x01\x00\x00\x00\x06\x00\x00\x00\x00\x00\x00\x00")
	exif := append([]byte{0xff, 0xe1, 0, byte(len(exifData) + 2)}, exifData...)
	data := append(bytes.Clone(encoded.Bytes()[:2]), exif...)
	data = append(data, encoded.Bytes()[2:]...)
	prepared, err := prepareImage(data, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(jpegExif(prepared.Data), exif) {
		t.Fatal("EXIF orientation lost during resize")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(prepared.Data))
	if err != nil || config.Width != 2048 {
		t.Fatalf("invalid resized JPEG: %+v %v", config, err)
	}
}

func TestPrepareMessagesPreservesHistoryAndCurrentTurn(t *testing.T) {
	data := testPNG(t, 32, 32, false)
	media := &protocol.MediaPart{Data: data, MediaType: "image/png"}
	tool := func(id string) protocol.Message {
		return protocol.NewToolMessage([]protocol.ToolResult{{ToolCallID: id, ToolName: "view_image", Type: protocol.ToolResultTypeContent, Content: []protocol.ToolResultContentPart{
			{Type: protocol.ContentTypeText, Text: "Image: /workspace/frame.png"},
			{Type: protocol.ContentTypeImage, Image: media},
		}}})
	}
	latest := protocol.NewUserMessage("Compare these two images")
	latest.Content = append(latest.Content, protocol.ContentPart{Type: protocol.ContentTypeImage, Image: media}, protocol.ContentPart{Type: protocol.ContentTypeImage, Image: media})
	messages := []protocol.Message{protocol.NewUserMessage("old task"), tool("old"), latest, tool("current")}
	before, _ := json.Marshal(messages)
	var cache imageCache
	out, omitted, err := cache.prepareMessages(context.Background(), messages, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if omitted != 1 {
		t.Fatalf("omitted %d", omitted)
	}
	old := out[1].Content[0].ToolResult
	if old.ToolCallID != "old" || old.Content[0].Text != "Image: /workspace/frame.png" || old.Content[1].Type != protocol.ContentTypeText {
		t.Fatal("tool result envelope/source lost")
	}
	if out[2].Content[1].Image == nil || out[2].Content[2].Image == nil || out[3].Content[0].ToolResult.Content[1].Image == nil {
		t.Fatal("current-turn image removed")
	}
	after, _ := json.Marshal(messages)
	if !bytes.Equal(before, after) {
		t.Fatal("persisted history mutated")
	}
	fromURL, err := cache.prepare(&protocol.MediaPart{URL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)}, 0)
	if err != nil || !bytes.Equal(fromURL.Data, data) {
		t.Fatalf("data URL: %v", err)
	}
	if len(cache.items) != 1 {
		t.Fatal("same image was not cached")
	}
}
