package zotigod

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
)

const (
	maxMessageRequestBytes = 28 << 20
)

var errRequestBodyTooLarge = errors.New("request body too large")

type submitMessageImageRequest struct {
	MimeType   string `json:"mime_type"`
	DataBase64 string `json:"data_base64"`
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

func validateMessageImages(requests []submitMessageImageRequest) ([]sessionlog.MessageImage, error) {
	if len(requests) == 0 {
		return nil, nil
	}
	if len(requests) > sessionlog.MaxMessageImages {
		return nil, fmt.Errorf("images must contain at most %d items", sessionlog.MaxMessageImages)
	}

	images := make([]sessionlog.MessageImage, 0, len(requests))
	totalBytes := 0
	for idx, req := range requests {
		mimeType := strings.ToLower(strings.TrimSpace(req.MimeType))
		if !sessionlog.IsAllowedMessageImageMimeType(mimeType) {
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
		if len(data) > sessionlog.MaxMessageImageBytes {
			return nil, fmt.Errorf("images[%d] exceeds %d bytes", idx, sessionlog.MaxMessageImageBytes)
		}
		totalBytes += len(data)
		if totalBytes > sessionlog.MaxMessageTotalImageBytes {
			return nil, fmt.Errorf("images exceed %d total bytes", sessionlog.MaxMessageTotalImageBytes)
		}
		width, height, err := sessionlog.ValidateMessageImageData(mimeType, data)
		if err != nil {
			return nil, fmt.Errorf("images[%d] does not match %s: %w", idx, mimeType, err)
		}
		images = append(images, sessionlog.MessageImage{
			MimeType:  mimeType,
			Data:      data,
			SizeBytes: len(data),
			Width:     width,
			Height:    height,
		})
	}
	return images, nil
}
