package zotigod

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
)

func TestWorkspaceFilesStayOnDaemonAndRejectConflicts(t *testing.T) {
	root := t.TempDir()
	catalog, err := zotigoworkspace.Open(filepath.Join(root, "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = catalog.Close() }()
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}, handlerOptions{catalog: catalog})
	capabilities := requestCatalog(t, handler, http.MethodGet, "/files/capabilities", "")
	var fileCapabilities map[string]bool
	decodeCatalogData(t, capabilities, &fileCapabilities)
	if !fileCapabilities["read"] || !fileCapabilities["write"] || !fileCapabilities["list"] || !fileCapabilities["image"] {
		t.Fatalf("file capabilities %+v", fileCapabilities)
	}
	projectRec := requestCatalog(t, handler, http.MethodPost, "/projects", `{"name":"files"}`)
	var project zotigoworkspace.Project
	decodeCatalogData(t, projectRec, &project)
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	record := requestCatalog(t, handler, http.MethodPost, "/projects/"+project.ID+"/sources", `{"path":`+quotedJSON(t, source)+`,"folder_mode":"direct"}`)
	if record.Code != 201 {
		t.Fatalf("source %d: %s", record.Code, record.Body)
	}
	file := filepath.Join(source, "hello.txt")
	if err := os.WriteFile(file, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	input := fileRequest{Path: "hello.txt", BasePath: source, BaseKind: "directory"}
	payload, _ := json.Marshal(input)
	opened := requestCatalog(t, handler, http.MethodPost, "/files/open", string(payload))
	if opened.Code != 200 {
		t.Fatalf("open %d: %s", opened.Code, opened.Body)
	}
	var result struct {
		Kind string           `json:"kind"`
		File textFileSnapshot `json:"file"`
	}
	decodeCatalogData(t, opened, &result)
	if result.File.Content != "original" {
		t.Fatalf("read %+v", result)
	}
	imagePath := filepath.Join(source, "preview.png")
	imageData := append([]byte("\x89PNG\r\n\x1a\n"), []byte("preview")...)
	if err := os.WriteFile(imagePath, imageData, 0600); err != nil {
		t.Fatal(err)
	}
	imageOpened := requestCatalog(t, handler, http.MethodPost, "/files/open", `{"path":`+quotedJSON(t, imagePath)+`}`)
	if imageOpened.Code != 200 {
		t.Fatalf("open image %d: %s", imageOpened.Code, imageOpened.Body)
	}
	var imageResult struct {
		Kind string            `json:"kind"`
		File imageFileSnapshot `json:"file"`
	}
	decodeCatalogData(t, imageOpened, &imageResult)
	if imageResult.Kind != "image" || imageResult.File.MediaType != "image/png" || imageResult.File.DataBase64 != "iVBORw0KGgpwcmV2aWV3" {
		t.Fatalf("image read %+v", imageResult)
	}
	spoofedImage := filepath.Join(source, "spoofed.png")
	if err := os.WriteFile(spoofedImage, []byte("not an image"), 0600); err != nil {
		t.Fatal(err)
	}
	spoofedOpened := requestCatalog(t, handler, http.MethodPost, "/files/open", `{"path":`+quotedJSON(t, spoofedImage)+`}`)
	var spoofedResult struct {
		Kind string `json:"kind"`
	}
	decodeCatalogData(t, spoofedOpened, &spoofedResult)
	if spoofedResult.Kind != "text" {
		t.Fatalf("spoofed image read %+v", spoofedResult)
	}
	input = fileRequest{Path: file, Content: "saved remotely", ExpectedMtimeMs: result.File.MtimeMs}
	payload, _ = json.Marshal(input)
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- requestCatalog(t, handler, http.MethodPost, "/files/save", string(payload)).Code
		}()
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for code := range codes {
		counts[code]++
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatalf("concurrent save results %v", counts)
	}
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(source, "escape")
	if err := os.Symlink(outside, link); err == nil {
		blocked := requestCatalog(t, handler, http.MethodPost, "/files/open", `{"path":`+quotedJSON(t, link)+`}`)
		if blocked.Code != 403 {
			t.Fatalf("symlink escape %d", blocked.Code)
		}
	}
	denied := requestCatalog(t, handler, http.MethodPost, "/files/open", `{"path":`+quotedJSON(t, outside)+`}`)
	if denied.Code != 403 {
		t.Fatalf("outside read %d", denied.Code)
	}
	t.Run("directory browsing and source selection boundaries", func(t *testing.T) {
		canonicalSource, err := filepath.EvalSymlinks(source)
		if err != nil {
			t.Fatal(err)
		}
		child := filepath.Join(source, "child")
		if err := os.Mkdir(child, 0700); err != nil {
			t.Fatal(err)
		}
		var listing directoryListing
		listed := requestCatalog(t, handler, http.MethodPost, "/files/list", `{"path":`+quotedJSON(t, source)+`}`)
		if listed.Code != 200 {
			t.Fatalf("list %d: %s", listed.Code, listed.Body)
		}
		decodeCatalogData(t, listed, &listing)
		if listing.ParentPath != nil || listing.Entries[0].Name != "child" {
			t.Fatalf("listing %+v", listing)
		}
		for _, entry := range listing.Entries {
			if entry.Name == "escape" && entry.Kind != "unavailable" {
				t.Fatalf("escape exposed: %+v", entry)
			}
		}
		listed = requestCatalog(t, handler, http.MethodPost, "/files/list", `{"path":`+quotedJSON(t, child)+`}`)
		decodeCatalogData(t, listed, &listing)
		if listing.ParentPath == nil || *listing.ParentPath != canonicalSource || len(listing.Entries) != 0 {
			t.Fatalf("child %+v", listing)
		}
		for _, outsidePath := range []string{root, filepath.Join(source, ".."), link} {
			denied := requestCatalog(t, handler, http.MethodPost, "/files/list", `{"path":`+quotedJSON(t, outsidePath)+`}`)
			if denied.Code != 403 {
				t.Fatalf("outside listing %d: %s", denied.Code, denied.Body)
			}
		}
		listed = requestCatalog(t, handler, http.MethodPost, "/sources/directories", `{"path":`+quotedJSON(t, root)+`}`)
		if listed.Code != 200 {
			t.Fatalf("source directories %d: %s", listed.Code, listed.Body)
		}
		decodeCatalogData(t, listed, &listing)
		for _, entry := range listing.Entries {
			if entry.Kind != "directory" {
				t.Fatalf("source picker exposed file %+v", entry)
			}
		}
		for i := range directoryEntryLimit + 1 {
			if err := os.WriteFile(filepath.Join(child, fmt.Sprintf("entry-%04d", i)), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		listed = requestCatalog(t, handler, http.MethodPost, "/files/list", `{"path":`+quotedJSON(t, child)+`}`)
		decodeCatalogData(t, listed, &listing)
		if !listing.Truncated || len(listing.Entries) != directoryEntryLimit {
			t.Fatalf("limit: %d %v", len(listing.Entries), listing.Truncated)
		}
		listed = requestCatalog(t, handler, http.MethodPost, "/files/list", `{ "path": "" }`)
		decodeCatalogData(t, listed, &listing)
		if len(listing.Entries) != 1 || listing.Entries[0].Path != canonicalSource {
			t.Fatalf("roots %+v", listing)
		}
	})
}

func TestWorkspaceImageMediaTypes(t *testing.T) {
	tests := []struct {
		extension string
		data      []byte
		mediaType string
	}{
		{".png", []byte("\x89PNG\r\n\x1a\n"), "image/png"},
		{".jpg", []byte{0xff, 0xd8, 0xff}, "image/jpeg"},
		{".gif", []byte("GIF89a"), "image/gif"},
		{".webp", []byte("RIFF0000WEBP"), "image/webp"},
		{".avif", []byte("0000ftypavif"), "image/avif"},
		{".bmp", []byte("BM"), "image/bmp"},
		{".ico", []byte{0, 0, 1, 0}, "image/x-icon"},
		{".svg", []byte(`<?xml version="1.0"?><!-- preview --><svg></svg>`), "image/svg+xml"},
	}
	for _, test := range tests {
		t.Run(test.extension, func(t *testing.T) {
			if got := imageMediaType(test.extension, test.data); got != test.mediaType {
				t.Fatalf("imageMediaType(%q) = %q, want %q", test.extension, got, test.mediaType)
			}
		})
	}
}
