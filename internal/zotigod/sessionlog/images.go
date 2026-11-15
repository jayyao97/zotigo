package sessionlog

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
)

const (
	MaxMessageImages          = 5
	MaxMessageImageBytes      = 5 << 20
	MaxMessageTotalImageBytes = 20 << 20
)

func IsAllowedMessageImageMimeType(mimeType string) bool {
	switch mimeType {
	case "image/png", "image/jpeg", "image/webp":
		return true
	default:
		return false
	}
}

func ValidateMessageImageData(mimeType string, data []byte) (int, int, error) {
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

func StoreMessageImageBlobs(rootDir string, sessionID string, images []MessageImage) ([]MessageImage, error) {
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
	stored := make([]MessageImage, len(images))
	copy(stored, images)
	for i, img := range stored {
		name, err := newMessageImageBlobName(img.MimeType)
		if err != nil {
			CleanupMessageImageBlobs(stored[:i])
			return nil, err
		}
		blobPath := filepath.Join(dirRel, name)
		path := filepath.Join(rootDir, blobPath)
		if err := os.WriteFile(path, img.Data, 0600); err != nil {
			CleanupMessageImageBlobs(stored[:i])
			return nil, fmt.Errorf("write image blob: %w", err)
		}
		stored[i].BlobPath = blobPath
		stored[i].LocalPath = path
	}
	return stored, nil
}

func CleanupMessageImageBlobs(images []MessageImage) {
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

func PublicImageURLFromBlobPath(blobPath string) string {
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

func MessageImageBlobPath(sessionID string, name string) (string, bool) {
	if sessionID == "" || name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", false
	}
	return filepath.Join("sessions", sessionID+".images", name), true
}

func MessageImageRefs(sessionID string, images []MessageImage) ([]zotigosession.ImageRef, error) {
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

func ImageRefNames(refs []zotigosession.ImageRef) []string {
	if len(refs) == 0 {
		return nil
	}
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Name)
	}
	return names
}
