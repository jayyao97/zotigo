package zotigod

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	zotigosession "github.com/jayyao97/zotigo/core/session"
)

func TestCodexInputImageServesOnlyRecordedBoundedImages(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	router := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})
	session := createSession(t, router)
	other := createSession(t, router)
	data, _ := base64.StdEncoding.DecodeString(tinyPNGBase64())
	imagePath := filepath.Join(t.TempDir(), "clipboard.png")
	if err := os.WriteFile(imagePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	record, err := store.AppendDisplayItem(context.Background(), session.ID, zotigosession.DisplayItem{
		ID: "codex-user", Type: zotigosession.DisplayItemUserMessage,
		Content: []zotigosession.DisplayContentPart{{Type: "text", Text: "Screenshot"}, {Type: "image", Image: &zotigosession.DisplayMediaPart{URL: imagePath}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("/sessions/%s/images/codex-input-%d-1", session.ID, record.Sequence)
	protected := authenticatedHandler(router, "test-token", "")
	get := func(path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		protected.ServeHTTP(rec, req)
		return rec
	}
	if rec := get(url, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized: %d", rec.Code)
	}
	if rec := get(url, "test-token"); rec.Code != http.StatusOK || rec.Body.String() != string(data) || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("image: %d %s", rec.Code, rec.Body.String())
	}
	for _, path := range []string{
		fmt.Sprintf("/sessions/%s/images/codex-input-%d-1", other.ID, record.Sequence),
		fmt.Sprintf("/sessions/%s/images/codex-input-%d-0", session.ID, record.Sequence),
		fmt.Sprintf("/sessions/%s/images/codex-input-%d-99", session.ID, record.Sequence),
		fmt.Sprintf("/sessions/%s/images/codex-input-0-1", session.ID),
	} {
		if rec := get(path, "test-token"); rec.Code != http.StatusNotFound {
			t.Fatalf("invalid attachment %s: %d", path, rec.Code)
		}
	}
	for _, content := range [][]byte{[]byte("private non-image content"), make([]byte, maxMessageImageBytes+1)} {
		if err := os.WriteFile(imagePath, content, 0600); err != nil {
			t.Fatal(err)
		}
		if rec := get(url, "test-token"); rec.Code != http.StatusNotFound {
			t.Fatalf("invalid image: %d", rec.Code)
		}
	}
	if err := os.Remove(imagePath); err != nil {
		t.Fatal(err)
	}
	if rec := get(url, "test-token"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing image: %d", rec.Code)
	}
	if err := os.Mkdir(imagePath, 0700); err != nil {
		t.Fatal(err)
	}
	if rec := get(url, "test-token"); rec.Code != http.StatusNotFound {
		t.Fatalf("directory: %d", rec.Code)
	}
}
