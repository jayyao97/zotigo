package providers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	imagedraw "image/draw"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"sync"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/jayyao97/zotigo/core/protocol"
)

const maxImageCacheBytes = 32 << 20
const omittedImage = "[Historical image omitted to fit the request byte budget. Refer to its source and read it again if needed.]"

type imageCacheKey struct {
	digest [32]byte
	level  int
}
type imageCache struct {
	mu    sync.Mutex
	items map[imageCacheKey]*protocol.MediaPart
	order []imageCacheKey
	bytes int
}

// Only immutable prepared bytes are cached, bounded independently of session size.
func (c *imageCache) prepare(media *protocol.MediaPart, level int) (*protocol.MediaPart, error) {
	if media == nil {
		return nil, fmt.Errorf("missing image")
	}
	data := media.Data
	if len(data) == 0 {
		if !strings.HasPrefix(media.URL, "data:") {
			return media, nil
		}
		_, encoded, ok := strings.Cut(media.URL, ";base64,")
		if !ok || len(encoded) > 32<<20 {
			return nil, fmt.Errorf("invalid or oversized image data URL")
		}
		var err error
		data, err = base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("invalid image encoding")
		}
	}
	key := imageCacheKey{sha256.Sum256(data), level}
	c.mu.Lock()
	defer c.mu.Unlock()
	if prepared := c.items[key]; prepared != nil {
		return prepared, nil
	}
	prepared, err := prepareImage(data, level)
	if err != nil {
		return nil, err
	}
	for len(c.order) > 0 && (c.bytes+len(prepared.Data) > maxImageCacheBytes || len(c.order) >= 128) {
		old := c.order[0]
		c.order = c.order[1:]
		c.bytes -= len(c.items[old].Data)
		delete(c.items, old)
	}
	if c.items == nil {
		c.items = make(map[imageCacheKey]*protocol.MediaPart)
	}
	c.items[key] = prepared
	c.order = append(c.order, key)
	c.bytes += len(prepared.Data)
	return prepared, nil
}

func prepareImage(data []byte, level int) (*protocol.MediaPart, error) {
	if len(data) > 24<<20 {
		return nil, fmt.Errorf("image input exceeds 24 MiB")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("invalid image")
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 16<<20 {
		return nil, fmt.Errorf("image exceeds 16 megapixels")
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("invalid image")
	}
	maxDimension, maxBytes := 2048>>level, (1<<20)>>level
	width, height := config.Width, config.Height
	if width > maxDimension || height > maxDimension {
		if width >= height {
			height = max(1, height*maxDimension/width)
			width = maxDimension
		} else {
			width = max(1, width*maxDimension/height)
			height = maxDimension
		}
	}
	if width == config.Width && height == config.Height && len(data) <= maxBytes && (format == "png" || format == "jpeg" || format == "webp") {
		return &protocol.MediaPart{Data: data, MediaType: "image/" + format}, nil
	}
	resized := image.NewNRGBA(image.Rect(0, 0, width, height))
	draw.ApproxBiLinear.Scale(resized, resized.Bounds(), decoded, decoded.Bounds(), draw.Src, nil)
	var encoded bytes.Buffer
	// Keep lossless screenshots and transparency when they fit. Otherwise use a
	// white-backed JPEG; the original image remains in the transcript/on disk.
	if format == "png" {
		if err := png.Encode(&encoded, resized); err != nil {
			return nil, err
		}
		if encoded.Len() <= maxBytes {
			return &protocol.MediaPart{Data: encoded.Bytes(), MediaType: "image/png"}, nil
		}
	}
	opaque := image.NewRGBA(resized.Bounds())
	imagedraw.Draw(opaque, opaque.Bounds(), &image.Uniform{C: color.White}, image.Point{}, imagedraw.Src)
	imagedraw.Draw(opaque, opaque.Bounds(), resized, image.Point{}, imagedraw.Over)
	exif := jpegExif(data)
	for {
		encoded.Reset()
		if err := jpeg.Encode(&encoded, opaque, &jpeg.Options{Quality: 85}); err != nil {
			return nil, err
		}
		if encoded.Len()+len(exif) <= maxBytes {
			jpegBytes := bytes.Clone(encoded.Bytes()[:2])
			jpegBytes = append(jpegBytes, exif...)
			jpegBytes = append(jpegBytes, encoded.Bytes()[2:]...)
			return &protocol.MediaPart{Data: jpegBytes, MediaType: "image/jpeg"}, nil
		}
		next := image.NewRGBA(image.Rect(0, 0, max(1, opaque.Bounds().Dx()/2), max(1, opaque.Bounds().Dy()/2)))
		draw.ApproxBiLinear.Scale(next, next.Bounds(), opaque, opaque.Bounds(), draw.Src, nil)
		opaque = next
	}
}

// Preserve JPEG EXIF (especially orientation) across re-encoding. The standard
// decoder leaves orientation unapplied; dropping it rotates camera photographs.
// Segment lengths are bounded by the JPEG format and checked before slicing.
func jpegExif(data []byte) []byte {
	if len(data) < 2 || data[0] != 0xff || data[1] != 0xd8 {
		return nil
	}
	for pos := 2; pos+4 <= len(data) && data[pos] == 0xff; {
		marker := data[pos+1]
		if marker == 0xda || marker == 0xd9 {
			break
		}
		length := int(binary.BigEndian.Uint16(data[pos+2 : pos+4]))
		end := pos + 2 + length
		if length < 2 || end > len(data) {
			break
		}
		if marker == 0xe1 && bytes.HasPrefix(data[pos+4:end], []byte("Exif\x00\x00")) {
			return data[pos:end]
		}
		pos = end
	}
	return nil
}

// prepareMessages never mutates persisted history. Tool call/result envelopes,
// signatures, text and source paths survive even when image pixels are omitted.
func (c *imageCache) prepareMessages(ctx context.Context, messages []protocol.Message, level, budget int) ([]protocol.Message, int, error) {
	out := append([]protocol.Message(nil), messages...)
	currentTurn := len(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == protocol.RoleUser && !messages[i].IsContextualUser() {
			currentTurn = i
			break
		}
	}
	type imageSlot struct {
		message int
		cost    int
		omit    func()
	}
	var slots []imageSlot
	for i := range out {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		out[i].Content = append([]protocol.ContentPart(nil), out[i].Content...)
		for j := range out[i].Content {
			part := &out[i].Content[j]
			if part.Type == protocol.ContentTypeImage {
				prepared, err := c.prepare(part.Image, level)
				if err != nil {
					return nil, 0, fmt.Errorf("prepare message image: %w; resize or replace the source image", err)
				}
				part.Image = prepared
				slots = append(slots, imageSlot{i, imageWireBytes(prepared), func() { *part = protocol.ContentPart{Type: protocol.ContentTypeText, Text: omittedImage} }})
			}
			if part.ToolResult != nil && part.ToolResult.Type == protocol.ToolResultTypeContent {
				result := *part.ToolResult
				result.Content = append([]protocol.ToolResultContentPart(nil), result.Content...)
				part.ToolResult = &result
				for k := range result.Content {
					content := &result.Content[k]
					if content.Type != protocol.ContentTypeImage {
						continue
					}
					prepared, err := c.prepare(content.Image, level)
					if err != nil {
						return nil, 0, fmt.Errorf("prepare tool image: %w; resize or replace the source image", err)
					}
					content.Image = prepared
					slots = append(slots, imageSlot{i, imageWireBytes(prepared), func() { *content = protocol.ToolResultContentPart{Type: protocol.ContentTypeText, Text: omittedImage} }})
				}
			}
		}
	}
	remaining := budget
	// Current-turn inputs must not silently disappear, including every image in
	// the latest parallel tool batch. Let the wire guard reject if they cannot fit.
	for _, slot := range slots {
		if slot.message >= currentTurn {
			remaining -= slot.cost
		}
	}
	omitted := 0
	for i := len(slots) - 1; i >= 0; i-- {
		slot := slots[i]
		if slot.message >= currentTurn {
			continue
		}
		if slot.cost <= remaining {
			remaining -= slot.cost
		} else {
			slot.omit()
			omitted++
		}
	}
	return out, omitted, nil
}

func imageWireBytes(media *protocol.MediaPart) int {
	if media == nil {
		return 0
	}
	if len(media.Data) > 0 {
		return base64.StdEncoding.EncodedLen(len(media.Data)) + 256
	}
	return len(media.URL) + len(media.FileID) + 256
}
