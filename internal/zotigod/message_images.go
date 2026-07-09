package zotigod

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

const (
	maxMessageRequestBytes    = 28 << 20
	maxMessageImages          = 5
	maxMessageImageBytes      = 5 << 20
	maxMessageTotalImageBytes = 20 << 20
)

var errRequestBodyTooLarge = errors.New("request body too large")

type submitMessageImageRequest struct {
	MimeType   string `json:"mime_type"`
	DataBase64 string `json:"data_base64"`
}

type messageImage struct {
	MimeType  string
	Data      []byte
	SizeBytes int
	Width     int
	Height    int
	BlobPath  string
	LocalPath string
}

func readRequiredLimitedJSON(r *http.Request, value any, maxBytes int64) error {
	if r.Body == nil {
		return errors.New("request body is required")
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > maxBytes {
		return errRequestBodyTooLarge
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return errors.New("request body is required")
	}
	return sonic.Unmarshal(data, value)
}

func validateMessageImages(requests []submitMessageImageRequest) ([]messageImage, error) {
	if len(requests) == 0 {
		return nil, nil
	}
	if len(requests) > maxMessageImages {
		return nil, fmt.Errorf("images must contain at most %d items", maxMessageImages)
	}

	images := make([]messageImage, 0, len(requests))
	totalBytes := 0
	for idx, req := range requests {
		mimeType := strings.ToLower(strings.TrimSpace(req.MimeType))
		if !isAllowedMessageImageMimeType(mimeType) {
			return nil, fmt.Errorf("images[%d].mime_type must be image/png, image/jpeg, or image/webp", idx)
		}
		if strings.TrimSpace(req.DataBase64) == "" {
			return nil, fmt.Errorf("images[%d].data_base64 is required", idx)
		}
		data, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(req.DataBase64))
		if err != nil {
			return nil, fmt.Errorf("images[%d].data_base64 must be valid base64", idx)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("images[%d].data_base64 is empty", idx)
		}
		if len(data) > maxMessageImageBytes {
			return nil, fmt.Errorf("images[%d] exceeds %d bytes", idx, maxMessageImageBytes)
		}
		totalBytes += len(data)
		if totalBytes > maxMessageTotalImageBytes {
			return nil, fmt.Errorf("images exceed %d total bytes", maxMessageTotalImageBytes)
		}
		width, height, err := validateMessageImageData(mimeType, data)
		if err != nil {
			return nil, fmt.Errorf("images[%d] does not match %s: %w", idx, mimeType, err)
		}
		images = append(images, messageImage{
			MimeType:  mimeType,
			Data:      data,
			SizeBytes: len(data),
			Width:     width,
			Height:    height,
		})
	}
	return images, nil
}

func isAllowedMessageImageMimeType(mimeType string) bool {
	switch mimeType {
	case "image/png", "image/jpeg", "image/webp":
		return true
	default:
		return false
	}
}

func validateMessageImageData(mimeType string, data []byte) (int, int, error) {
	if mimeType == "image/webp" {
		if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
			return 0, 0, errors.New("invalid webp header")
		}
		return 0, 0, nil
	}

	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, err
	}
	switch mimeType {
	case "image/png":
		if format != "png" {
			return 0, 0, fmt.Errorf("decoded as %s", format)
		}
	case "image/jpeg":
		if format != "jpeg" {
			return 0, 0, fmt.Errorf("decoded as %s", format)
		}
	}
	return cfg.Width, cfg.Height, nil
}

func storeMessageImageBlobs(rootDir string, sessionID string, images []messageImage) ([]messageImage, error) {
	if len(images) == 0 {
		return images, nil
	}
	if rootDir == "" {
		return nil, errors.New("image persistence is not configured")
	}
	dirRel := filepath.Join("sessions", sessionID+".images")
	dir := filepath.Join(rootDir, dirRel)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create image directory: %w", err)
	}
	stored := make([]messageImage, len(images))
	copy(stored, images)
	for i, img := range stored {
		name, err := newMessageImageBlobName(img.MimeType)
		if err != nil {
			cleanupMessageImageBlobs(stored[:i])
			return nil, err
		}
		blobPath := filepath.Join(dirRel, name)
		path := filepath.Join(rootDir, blobPath)
		if err := os.WriteFile(path, img.Data, 0600); err != nil {
			cleanupMessageImageBlobs(stored[:i])
			return nil, fmt.Errorf("write image blob: %w", err)
		}
		stored[i].BlobPath = blobPath
		stored[i].LocalPath = path
	}
	return stored, nil
}

func cleanupMessageImageBlobs(images []messageImage) {
	for _, img := range images {
		if img.LocalPath == "" {
			continue
		}
		_ = os.Remove(img.LocalPath)
	}
}

func newMessageImageBlobName(mimeType string) (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate image blob name: %w", err)
	}
	return hex.EncodeToString(buf[:]) + messageImageExtension(mimeType), nil
}

func messageImageExtension(mimeType string) string {
	switch mimeType {
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

func publicImageURLFromBlobPath(blobPath string) string {
	sessionID, name, ok := parseMessageImageBlobPath(blobPath)
	if !ok {
		return ""
	}
	return "/sessions/" + url.PathEscape(sessionID) + "/images/" + url.PathEscape(name)
}

func parseMessageImageBlobPath(blobPath string) (sessionID string, name string, ok bool) {
	parts := strings.Split(filepath.ToSlash(blobPath), "/")
	if len(parts) != 3 || parts[0] != "sessions" || parts[2] == "" {
		return "", "", false
	}
	dir := parts[1]
	if !strings.HasSuffix(dir, ".images") {
		return "", "", false
	}
	sessionID = strings.TrimSuffix(dir, ".images")
	if sessionID == "" || strings.ContainsAny(parts[2], `/\`) || parts[2] == "." || parts[2] == ".." {
		return "", "", false
	}
	return sessionID, parts[2], true
}

func messageImageBlobPath(sessionID string, name string) (string, bool) {
	if sessionID == "" || name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", false
	}
	return filepath.Join("sessions", sessionID+".images", name), true
}

func messageImageRefs(sessionID string, images []messageImage) ([]zotigosession.ImageRef, error) {
	if len(images) == 0 {
		return nil, nil
	}
	now := time.Now().UTC()
	refs := make([]zotigosession.ImageRef, 0, len(images))
	for _, img := range images {
		refSessionID, name, ok := parseMessageImageBlobPath(img.BlobPath)
		if !ok || refSessionID != sessionID {
			return nil, fmt.Errorf("invalid image blob path: %s", img.BlobPath)
		}
		refs = append(refs, zotigosession.ImageRef{
			SessionID: sessionID,
			Name:      name,
			BlobPath:  img.BlobPath,
			MimeType:  img.MimeType,
			SizeBytes: img.SizeBytes,
			Width:     img.Width,
			Height:    img.Height,
			CreatedAt: now,
		})
	}
	return refs, nil
}

func imageRefNames(refs []zotigosession.ImageRef) []string {
	if len(refs) == 0 {
		return nil
	}
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Name)
	}
	return names
}
