//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/lsp"
)

// TestE2E_LSP_Gopls tests LSP operations with a real gopls server.
//
// Run: go test -tags=e2e -v -run TestE2E_LSP ./tests/e2e/
func TestE2E_LSP_Gopls(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not installed, skipping E2E test")
	}

	tmpDir := t.TempDir()

	goMod := "module testproject\n\ngo 1.21\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("failed to write go.mod: %v", err)
	}

	mainGo := `package main

type Greeter interface {
	Greet(name string) string
}

type SimpleGreeter struct {
	prefix string
}

func (s *SimpleGreeter) Greet(name string) string {
	return s.prefix + ", " + name + "!"
}

func NewGreeter(prefix string) *SimpleGreeter {
	return &SimpleGreeter{prefix: prefix}
}

func main() {
	g := NewGreeter("Hello")
	msg := g.Greet("World")
	println(msg)
}
`
	mainPath := filepath.Join(tmpDir, "main.go")
	if err := os.WriteFile(mainPath, []byte(mainGo), 0644); err != nil {
		t.Fatalf("failed to write main.go: %v", err)
	}

	manager := lsp.NewManager(tmpDir)
	defer manager.StopAll()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client, err := manager.GetClient(ctx, "go")
	if err != nil {
		t.Fatalf("failed to get Go LSP client: %v", err)
	}

	content, _ := os.ReadFile(mainPath)
	uri := "file://" + mainPath
	if err := client.DidOpen(ctx, uri, "go", string(content)); err != nil {
		t.Fatalf("failed to open file: %v", err)
	}

	time.Sleep(3 * time.Second)

	t.Run("Definition", func(t *testing.T) {
		locations, err := client.Definition(ctx, uri, 19, 7)
		if err != nil {
			t.Fatalf("Definition failed: %v", err)
		}
		if len(locations) == 0 {
			t.Fatal("expected at least one definition location")
		}
		loc := locations[0]
		if !strings.HasSuffix(loc.URI, "main.go") {
			t.Errorf("expected URI to end with main.go, got %s", loc.URI)
		}
		if loc.Range.Start.Line != 14 {
			t.Errorf("expected definition at line 14, got line %d", loc.Range.Start.Line)
		}
	})

	t.Run("References", func(t *testing.T) {
		locations, err := client.References(ctx, uri, 10, 25, true)
		if err != nil {
			t.Fatalf("References failed: %v", err)
		}
		if len(locations) < 2 {
			t.Errorf("expected at least 2 references, got %d", len(locations))
			for i, loc := range locations {
				t.Logf("  [%d] %s:%d:%d", i, loc.URI, loc.Range.Start.Line, loc.Range.Start.Character)
			}
		}
	})

	t.Run("Hover", func(t *testing.T) {
		hover, err := client.Hover(ctx, uri, 6, 7)
		if err != nil {
			t.Fatalf("Hover failed: %v", err)
		}
		if hover == nil {
			t.Fatal("expected hover result")
		}
		if hover.Contents == "" {
			t.Error("expected hover contents")
		}
		t.Logf("Hover contents: %s", hover.Contents)
	})

	t.Run("Implementation", func(t *testing.T) {
		locations, err := client.Implementation(ctx, uri, 2, 6)
		if err != nil {
			t.Fatalf("Implementation failed: %v", err)
		}
		t.Logf("Found %d implementations", len(locations))
		for i, loc := range locations {
			t.Logf("  [%d] %s:%d:%d", i, loc.URI, loc.Range.Start.Line, loc.Range.Start.Character)
		}
		if len(locations) == 0 {
			t.Log("No implementations found - this may be expected for small test files")
		}
	})

	t.Run("DocumentSymbols", func(t *testing.T) {
		symbols, err := client.DocumentSymbols(ctx, uri)
		if err != nil {
			t.Fatalf("DocumentSymbols failed: %v", err)
		}
		t.Logf("Found %d symbols", len(symbols))
		for _, sym := range symbols {
			t.Logf("  [%s] %s at line %d", lsp.SymbolKindString(sym.Kind), sym.Name, sym.Location.Range.Start.Line)
		}
		if len(symbols) == 0 {
			t.Error("expected at least one symbol")
		}
		expectedSymbols := []string{"Greeter", "SimpleGreeter", "Greet", "NewGreeter", "main"}
		foundCount := 0
		for _, expected := range expectedSymbols {
			for _, sym := range symbols {
				if sym.Name == expected {
					foundCount++
					break
				}
			}
		}
		if foundCount < 3 {
			t.Errorf("expected to find at least 3 of %v, found %d", expectedSymbols, foundCount)
		}
	})

	t.Run("WorkspaceSymbols", func(t *testing.T) {
		symbols, err := client.WorkspaceSymbols(ctx, "Greeter")
		if err != nil {
			t.Fatalf("WorkspaceSymbols failed: %v", err)
		}
		t.Logf("Found %d workspace symbols for 'Greeter'", len(symbols))
		for _, sym := range symbols {
			t.Logf("  [%s] %s", lsp.SymbolKindString(sym.Kind), sym.Name)
		}
		if len(symbols) == 0 {
			t.Log("No workspace symbols found - this may be expected for fresh workspace")
		}
	})

	if err := client.DidClose(ctx, uri); err != nil {
		t.Logf("warning: failed to close file: %v", err)
	}
}

func TestE2E_LSP_Manager(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not installed, skipping E2E test")
	}

	tmpDir := t.TempDir()

	goMod := "module testproject\ngo 1.21\n"
	os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goMod), 0644)

	simpleGo := "package main\nfunc Hello() string { return \"hello\" }\n"
	goFile := filepath.Join(tmpDir, "simple.go")
	os.WriteFile(goFile, []byte(simpleGo), 0644)

	manager := lsp.NewManager(tmpDir)
	defer manager.StopAll()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Run("GetClientForFile", func(t *testing.T) {
		client, err := manager.GetClientForFile(ctx, goFile)
		if err != nil {
			t.Fatalf("GetClientForFile failed: %v", err)
		}
		if client.Language() != "go" {
			t.Errorf("expected language 'go', got '%s'", client.Language())
		}
		if !client.IsInitialized() {
			t.Error("expected client to be initialized")
		}
	})

	t.Run("GetAvailableLanguages", func(t *testing.T) {
		available := manager.GetAvailableLanguages()
		foundGo := false
		for _, lang := range available {
			if lang == "go" {
				foundGo = true
				break
			}
		}
		if !foundGo {
			t.Error("expected 'go' to be in available languages")
		}
	})
}
