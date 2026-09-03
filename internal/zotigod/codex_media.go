package zotigod

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

type codexImageStore interface {
	PutImageRefs(context.Context, []zotigosession.ImageRef) error
	DeleteImageRefs(context.Context, string, []string) error
}

type codexStoreRoot interface {
	RootDir() string
}

var errCodexImageUnavailable = errors.New("codex image media unavailable")

func codexImageGenerationCompleted(item codexThreadItem) bool {
	return strings.EqualFold(strings.TrimSpace(item.Status), "completed")
}

func codexGeneratedImageDisplayItem(ctx context.Context, store zotigosession.Store, rootDir, sessionID, turnID string, item codexThreadItem, createdAt time.Time) (zotigosession.DisplayItem, func(), error) {
	media, cleanup, err := persistCodexImage(ctx, store, rootDir, sessionID, item)
	if err != nil {
		return zotigosession.DisplayItem{}, func() {}, err
	}
	return zotigosession.DisplayItem{
		ID: item.ID, Type: zotigosession.DisplayItemAssistantMessage, Role: string(protocol.RoleAssistant),
		Content: []zotigosession.DisplayContentPart{{Type: string(protocol.ContentTypeImage), Image: media}},
		Turn:    &zotigosession.DisplayTurn{ID: turnID}, CreatedAt: createdAt,
	}, cleanup, nil
}

func persistCodexImage(ctx context.Context, store zotigosession.Store, rootDir, sessionID string, item codexThreadItem) (*zotigosession.DisplayMediaPart, func(), error) {
	data, err := codexGeneratedImageBytes(item)
	if err != nil {
		return nil, func() {}, err
	}
	mimeType := strings.TrimSpace(strings.Split(http.DetectContentType(data), ";")[0])
	if !isAllowedMessageImageMimeType(mimeType) {
		return nil, func() {}, fmt.Errorf("%w: generated image %q has unsupported MIME type %q", errCodexImageUnavailable, item.ID, mimeType)
	}
	width, height, err := validateMessageImageData(mimeType, data)
	if err != nil {
		return nil, func() {}, fmt.Errorf("%w: validate generated image %q: %v", errCodexImageUnavailable, item.ID, err)
	}
	stored, err := storeMessageImageBlobs(rootDir, sessionID, []messageImage{{
		MimeType: mimeType, Data: data, SizeBytes: len(data), Width: width, Height: height,
	}})
	if err != nil {
		return nil, func() {}, fmt.Errorf("persist Codex generated image %q: %w", item.ID, err)
	}
	refs, err := messageImageRefs(sessionID, stored)
	if err != nil {
		cleanupMessageImageBlobs(stored)
		return nil, func() {}, err
	}
	imageStore, ok := store.(codexImageStore)
	if !ok {
		cleanupMessageImageBlobs(stored)
		return nil, func() {}, errors.New("session store does not support image references")
	}
	if err := imageStore.PutImageRefs(ctx, refs); err != nil {
		cleanupMessageImageBlobs(stored)
		return nil, func() {}, fmt.Errorf("index Codex generated image %q: %w", item.ID, err)
	}
	cleanup := func() {
		_ = imageStore.DeleteImageRefs(context.Background(), sessionID, imageRefNames(refs))
		cleanupMessageImageBlobs(stored)
	}
	image := stored[0]
	return &zotigosession.DisplayMediaPart{
		URL: publicImageURLFromBlobPath(image.BlobPath), MediaType: image.MimeType,
		SizeBytes: image.SizeBytes, Width: image.Width, Height: image.Height,
	}, cleanup, nil
}

func codexGeneratedImageBytes(item codexThreadItem) ([]byte, error) {
	var savedPathErr error
	if item.SavedPath != nil && strings.TrimSpace(*item.SavedPath) != "" {
		file, err := os.Open(*item.SavedPath)
		if err == nil {
			defer func() { _ = file.Close() }()
			data, readErr := io.ReadAll(io.LimitReader(file, maxMessageTotalImageBytes+1))
			if readErr != nil {
				return nil, fmt.Errorf("%w: read generated image %q: %v", errCodexImageUnavailable, item.ID, readErr)
			}
			if len(data) > maxMessageTotalImageBytes {
				return nil, fmt.Errorf("%w: generated image %q exceeds %d bytes", errCodexImageUnavailable, item.ID, maxMessageTotalImageBytes)
			}
			return data, nil
		}
		savedPathErr = err
	}
	encoded, _ := item.Result.(string)
	if strings.TrimSpace(encoded) == "" && savedPathErr != nil {
		return nil, fmt.Errorf("%w: generated image %q source cannot be read", errCodexImageUnavailable, item.ID)
	}
	data, err := decodeCodexImagePayload(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: decode generated image %q: %v", errCodexImageUnavailable, item.ID, err)
	}
	return data, nil
}

func codexUnavailableImageDisplayItem(itemID, turnID string, createdAt time.Time) zotigosession.DisplayItem {
	return zotigosession.DisplayItem{
		ID: itemID, Type: zotigosession.DisplayItemError, Error: "generated image media is unavailable",
		Turn: &zotigosession.DisplayTurn{ID: turnID}, CreatedAt: createdAt,
	}
}

func decodeCodexImagePayload(encoded string) ([]byte, error) {
	if comma := strings.Index(encoded, ";base64,"); strings.HasPrefix(encoded, "data:") && comma >= 0 {
		encoded = encoded[comma+len(";base64,"):]
	}
	encoded = strings.TrimSpace(encoded)
	if base64.StdEncoding.DecodedLen(len(encoded)) > maxMessageTotalImageBytes {
		return nil, fmt.Errorf("image exceeds %d bytes", maxMessageTotalImageBytes)
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) == 0 {
		return nil, errors.New("invalid base64 payload")
	}
	if len(data) > maxMessageTotalImageBytes {
		return nil, fmt.Errorf("image exceeds %d bytes", maxMessageTotalImageBytes)
	}
	return data, nil
}

func codexDynamicToolResultContent(value any) []zotigosession.DisplayToolResultContentPart {
	var items []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL string `json:"imageUrl"`
	}
	if !decodeCodexContent(value, &items) {
		return nil
	}
	content := make([]zotigosession.DisplayToolResultContentPart, 0, len(items))
	for _, item := range items {
		switch item.Type {
		case "inputText":
			content = append(content, zotigosession.DisplayToolResultContentPart{Type: string(protocol.ContentTypeText), Text: item.Text})
		case "inputImage":
			content = append(content, zotigosession.DisplayToolResultContentPart{Type: string(protocol.ContentTypeImage), Image: &zotigosession.DisplayMediaPart{URL: item.ImageURL}})
		}
	}
	return content
}

func codexMCPToolResultContent(value any) ([]zotigosession.DisplayToolResultContentPart, error) {
	var result struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Data     string `json:"data"`
			MIMEType string `json:"mimeType"`
		} `json:"content"`
	}
	if !decodeCodexContent(value, &result) {
		return nil, nil
	}
	content := make([]zotigosession.DisplayToolResultContentPart, 0, len(result.Content))
	var mediaErr error
	for _, item := range result.Content {
		switch item.Type {
		case "text":
			content = append(content, zotigosession.DisplayToolResultContentPart{Type: string(protocol.ContentTypeText), Text: item.Text})
		case "image":
			data, err := decodeCodexImagePayload(item.Data)
			if err != nil {
				content = append(content, unavailableToolImageContent())
				mediaErr = errors.Join(mediaErr, fmt.Errorf("decode Codex MCP image: %w", err))
				continue
			}
			content = append(content, zotigosession.DisplayToolResultContentPart{Type: string(protocol.ContentTypeImage), Image: &zotigosession.DisplayMediaPart{Data: data, MediaType: item.MIMEType}})
		}
	}
	return content, mediaErr
}

func unavailableToolImageContent() zotigosession.DisplayToolResultContentPart {
	return zotigosession.DisplayToolResultContentPart{Type: string(protocol.ContentTypeText), Text: "[image content is unavailable]"}
}

func decodeCodexContent(value any, target any) bool {
	encoded, err := sonic.Marshal(value)
	return err == nil && sonic.Unmarshal(encoded, target) == nil
}

func codexMCPResultWithoutContent(value any) any {
	encoded, err := sonic.Marshal(value)
	if err != nil {
		return nil
	}
	var result map[string]any
	if err := sonic.Unmarshal(encoded, &result); err != nil {
		return nil
	}
	delete(result, "content")
	if len(result) == 0 {
		return nil
	}
	return result
}

func persistCodexToolResultMedia(ctx context.Context, store zotigosession.Store, rootDir, sessionID string, result *zotigosession.DisplayToolResult) (func(), error) {
	cleanups := make([]func(), 0)
	cleanupAll := func() {
		for index := len(cleanups) - 1; index >= 0; index-- {
			cleanups[index]()
		}
	}
	for index := range result.Content {
		media := result.Content[index].Image
		if media == nil || len(media.Data) == 0 && !strings.HasPrefix(media.URL, "data:") {
			continue
		}
		item := codexThreadItem{ID: result.ToolCallID, Result: media.URL}
		if len(media.Data) > 0 {
			item.Result = base64.StdEncoding.EncodeToString(media.Data)
		}
		stored, cleanup, err := persistCodexImage(ctx, store, rootDir, sessionID, item)
		if err != nil {
			if errors.Is(err, errCodexImageUnavailable) {
				result.Content[index] = unavailableToolImageContent()
				continue
			}
			cleanupAll()
			return func() {}, err
		}
		result.Content[index].Image = stored
		cleanups = append(cleanups, cleanup)
	}
	return cleanupAll, nil
}

func codexSessionStoreRoot(store zotigosession.Store) string {
	if rooted, ok := store.(codexStoreRoot); ok {
		return rooted.RootDir()
	}
	return ""
}

func codexSessionStoreFromItems(items displayItemSource) zotigosession.Store {
	switch source := items.(type) {
	case storedDisplayItemSource:
		return source.store
	case eventingDisplayItemSource:
		return codexSessionStoreFromItems(source.source)
	case eventingOffsetDisplayItemSource:
		return codexSessionStoreFromItems(source.source)
	default:
		return nil
	}
}
