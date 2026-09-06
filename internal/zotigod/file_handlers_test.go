package zotigod

import (
	"encoding/json"
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
}
