package providers_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	_ "github.com/jayyao97/zotigo/core/providers/anthropic"
	_ "github.com/jayyao97/zotigo/core/providers/gemini"
	_ "github.com/jayyao97/zotigo/core/providers/openai"
)

func wideImage(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 2400, 32))
	var data bytes.Buffer
	if err := png.Encode(&data, img); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func finishSSE(w http.ResponseWriter, provider string) {
	w.Header().Set("Content-Type", "text/event-stream")
	switch provider {
	case "openai-response", "openai-response-failed":
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")
	case "anthropic":
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	case "gemini":
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}\n\n")
	default:
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
}

func TestSerializedImageHistoryBudgetAcrossProviders(t *testing.T) {
	// A valid large ancillary chunk reproduces wire-size pressure without
	// repeatedly encoding megapixels of noise under the race detector in CI.
	// Pixel/quality bounds are exercised separately by TestPrepareImageBoundedPreview.
	data := wideImage(t)
	chunkData := append([]byte("Comment\x00"), bytes.Repeat([]byte("x"), 2<<20)...)
	chunk := make([]byte, 8, len(chunkData)+12)
	binary.BigEndian.PutUint32(chunk[:4], uint32(len(chunkData)))
	copy(chunk[4:8], "tEXt")
	chunk = append(chunk, chunkData...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(chunk[4:]))
	data = append(append(bytes.Clone(data[:len(data)-12]), chunk...), data[len(data)-12:]...)
	messages := []protocol.Message{protocol.NewUserMessage("Inspect frames")}
	for i := 0; i < 15; i++ {
		messages[0].Content = append(messages[0].Content, protocol.ContentPart{Type: protocol.ContentTypeImage, Image: &protocol.MediaPart{Data: data, MediaType: "image/png"}})
	}
	original, _ := json.Marshal(messages)
	historyBytes := len(original)
	if historyBytes < 32<<20 {
		t.Fatal("fixture must reproduce a >32 MiB history")
	}
	for _, name := range []string{"anthropic", "openai", "openai-response", "gemini"} {
		t.Run(name, func(t *testing.T) {
			var sizes []int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				sizes = append(sizes, len(body))
				if !json.Valid(body) {
					t.Error("invalid provider request JSON")
				}
				if len(body) > 24<<20 {
					t.Error("oversized request reached server")
				}
				finishSSE(w, name)
			}))
			defer server.Close()
			cfg := config.ProfileConfig{Provider: name, Model: "test", APIKey: "test", BaseURL: server.URL}
			if name == "openai-response" {
				cfg.Provider = "openai"
				cfg.Params = map[string]any{"mode": "response"}
			}
			p, err := providers.NewProvider(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := drainProvider(t, p, messages); err != nil {
				t.Fatal(err)
			}
			if len(sizes) != 1 {
				t.Fatalf("requests %v", sizes)
			}
			if len(messages[0].Content) != 16 || messages[0].Content[1].Image == nil || !bytes.Equal(messages[0].Content[1].Image.Data, data) {
				t.Fatal("history mutated")
			}
			t.Logf("history=%d bytes, actual SDK request=%d bytes", historyBytes, sizes[0])
		})
	}
}

func drainProvider(t *testing.T, p providers.Provider, messages []protocol.Message) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := p.StreamChat(ctx, messages, nil)
	if err != nil {
		return err
	}
	for event := range stream {
		if event.Error != nil {
			err = event.Error
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	return err
}

func Test413RecoveryAndTerminalErrors(t *testing.T) {
	data := wideImage(t)
	for _, scenario := range []string{"recover", "always413", "auth", "textOnly"} {
		t.Run(scenario, func(t *testing.T) {
			var sizes []int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				sizes = append(sizes, len(body))
				if scenario == "auth" {
					w.WriteHeader(401)
					_, _ = io.WriteString(w, `{"error":{"message":"unauthorized"}}`)
					return
				}
				if len(sizes) == 1 || scenario == "always413" {
					w.WriteHeader(413)
					_, _ = io.WriteString(w, `{"error":{"type":"request_too_large","message":"too large"}}`)
					return
				}
				finishSSE(w, "anthropic")
			}))
			defer server.Close()
			p, err := providers.NewProvider(config.ProfileConfig{Provider: "anthropic", Model: "test", APIKey: "test", BaseURL: server.URL, Params: map[string]any{"max_request_bytes": 2 << 20}})
			if err != nil {
				t.Fatal(err)
			}
			message := protocol.NewUserMessage("Inspect image")
			message.Content = append(message.Content, protocol.ContentPart{Type: protocol.ContentTypeImage, Image: &protocol.MediaPart{Data: data, MediaType: "image/png"}})
			if scenario == "textOnly" {
				message = protocol.NewUserMessage(strings.Repeat("x", 3<<20))
			}
			err = drainProvider(t, p, []protocol.Message{message})
			switch scenario {
			case "recover":
				if err != nil || len(sizes) < 2 {
					t.Fatalf("recovery failed: %v sizes=%v", err, sizes)
				}
			case "auth":
				if err == nil || errors.Is(err, providers.ErrRequestTooLarge) || len(sizes) != 1 {
					t.Fatalf("auth retried: %v sizes=%v", err, sizes)
				}
			case "always413", "textOnly":
				if !errors.Is(err, providers.ErrRequestTooLarge) {
					t.Fatalf("wrong error: %v", err)
				}
			}
			if scenario == "textOnly" && len(sizes) != 0 {
				t.Fatal("oversized text reached server")
			}
			if len(sizes) > 3 {
				t.Fatalf("unbounded retries: %v", sizes)
			}
			for i := 1; i < len(sizes); i++ {
				if sizes[i] >= sizes[i-1] {
					t.Fatalf("retried same/larger request: %v", sizes)
				}
			}
		})
	}
}

func TestImageRecoveryKeepsRoomForText(t *testing.T) {
	for _, upstream413 := range []bool{false, true} {
		t.Run(fmt.Sprint(upstream413), func(t *testing.T) {
			data := wideImage(t)
			if !upstream413 {
				// Keep the initial image under the dimension limit so its ancillary
				// bytes survive until the retry's tighter per-image size budget.
				var buf bytes.Buffer
				if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 32, 32))); err != nil {
					t.Fatal(err)
				}
				data = buf.Bytes()
				payload := append([]byte("Comment\x00"), bytes.Repeat([]byte("x"), 700<<10)...)
				chunk := make([]byte, 8)
				binary.BigEndian.PutUint32(chunk[:4], uint32(len(payload)))
				copy(chunk[4:], "tEXt")
				chunk = append(chunk, payload...)
				chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(chunk[4:]))
				data = append(append(bytes.Clone(data[:len(data)-12]), chunk...), data[len(data)-12:]...)
			}
			var sizes []int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				sizes = append(sizes, len(body))
				if upstream413 && len(sizes) == 1 {
					w.WriteHeader(413)
					_, _ = io.WriteString(w, `{"error":{"type":"request_too_large","message":"too large"}}`)
					return
				}
				finishSSE(w, "anthropic")
			}))
			defer server.Close()
			p, err := providers.NewProvider(config.ProfileConfig{Provider: "anthropic", Model: "test", APIKey: "test", BaseURL: server.URL, Params: map[string]any{"max_request_bytes": 1 << 20}})
			if err != nil {
				t.Fatal(err)
			}
			message := protocol.NewUserMessage(strings.Repeat("x", 600<<10))
			message.Content = append(message.Content, protocol.ContentPart{Type: protocol.ContentTypeImage, Image: &protocol.MediaPart{Data: data, MediaType: "image/png"}})
			if err := drainProvider(t, p, []protocol.Message{message}); err != nil {
				t.Fatal(err)
			}
			want := 1
			if upstream413 {
				want = 2
			}
			if len(sizes) != want {
				t.Fatalf("requests=%v", sizes)
			}
			for i, size := range sizes {
				if size > 1<<20 || (i > 0 && size >= sizes[i-1]) {
					t.Fatalf("invalid retry sizes: %v", sizes)
				}
			}
		})
	}
}
