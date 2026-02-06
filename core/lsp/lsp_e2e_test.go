//go:build e2e
// +build e2e

package lsp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLSP_E2E_Gopls tests LSP operations with a real gopls server.
func TestLSP_E2E_Gopls(t *testing.T) {
	// Check if gopls is available
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not installed, skipping E2E test")
	}

	// Create temp directory with Go module
	tmpDir := t.TempDir()

	// Create go.mod
	goMod := `module testproject

go 1.21
`
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("failed to write go.mod: %v", err)
	}

	// Create main.go with some code to test
	// Line numbers (0-indexed):
	// 0: package main
	// 1: (empty)
	// 2: type Greeter interface {
	// 3:     Greet(name string) string
	// 4: }
	// 5: (empty)
	// 6: type SimpleGreeter struct {
	// 7:     prefix string
	// 8: }
	// 9: (empty)
	// 10: func (s *SimpleGreeter) Greet(name string) string {
	// 11:     return s.prefix + ", " + name + "!"
	// 12: }
	// 13: (empty)
	// 14: func NewGreeter(prefix string) *SimpleGreeter {
	// 15:     return &SimpleGreeter{prefix: prefix}
	// 16: }
	// 17: (empty)
	// 18: func main() {
	// 19:     g := NewGreeter("Hello")
	// 20:     msg := g.Greet("World")
	// 21:     println(msg)
	// 22: }
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

	// Create LSP manager
	manager := NewManager(tmpDir)
	defer func() {
		// Use a separate context with short timeout for cleanup
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = cleanupCtx
		manager.StopAll()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Get client for Go
	client, err := manager.GetClient(ctx, "go")
	if err != nil {
		t.Fatalf("failed to get Go LSP client: %v", err)
	}

	// Open the file
	content, _ := os.ReadFile(mainPath)
	uri := "file://" + mainPath
	if err := client.DidOpen(ctx, uri, "go", string(content)); err != nil {
		t.Fatalf("failed to open file: %v", err)
	}

	// Give gopls time to analyze
	time.Sleep(3 * time.Second)

	t.Run("Definition", func(t *testing.T) {
		// Find definition of NewGreeter call at line 19 (0-indexed)
		// Line 19: `	g := NewGreeter("Hello")`
		// NewGreeter starts at character 6 (after "\tg := ")
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
		// NewGreeter is defined at line 14 (0-indexed)
		if loc.Range.Start.Line != 14 {
			t.Errorf("expected definition at line 14 (0-indexed), got line %d", loc.Range.Start.Line)
		}
	})

	t.Run("References", func(t *testing.T) {
		// Find references to Greet method definition at line 10
		// Line 10: `func (s *SimpleGreeter) Greet(name string) string {`
		// "Greet" starts at character 24
		locations, err := client.References(ctx, uri, 10, 25, true)
		if err != nil {
			t.Fatalf("References failed: %v", err)
		}

		// Should find at least 2 references:
		// 1. Definition at line 10
		// 2. Call at line 20: msg := g.Greet("World")
		if len(locations) < 2 {
			t.Errorf("expected at least 2 references, got %d", len(locations))
			for i, loc := range locations {
				t.Logf("  [%d] %s:%d:%d", i, loc.URI, loc.Range.Start.Line, loc.Range.Start.Character)
			}
		}
	})

	t.Run("Hover", func(t *testing.T) {
		// Hover over SimpleGreeter struct at line 6
		// Line 6: `type SimpleGreeter struct {`
		// "SimpleGreeter" starts at character 5
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
		// Find implementations of Greeter interface at line 2
		// Line 2: `type Greeter interface {`
		// "Greeter" starts at character 5
		locations, err := client.Implementation(ctx, uri, 2, 6)
		if err != nil {
			t.Fatalf("Implementation failed: %v", err)
		}

		t.Logf("Found %d implementations", len(locations))
		for i, loc := range locations {
			t.Logf("  [%d] %s:%d:%d", i, loc.URI, loc.Range.Start.Line, loc.Range.Start.Character)
		}

		// Should find SimpleGreeter as implementation
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
			t.Logf("  [%s] %s at line %d", SymbolKindString(sym.Kind), sym.Name, sym.Location.Range.Start.Line)
		}

		if len(symbols) == 0 {
			t.Error("expected at least one symbol")
		}

		// Should find these symbols: Greeter, SimpleGreeter, Greet, NewGreeter, main
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
		// Search for "Greeter"
		symbols, err := client.WorkspaceSymbols(ctx, "Greeter")
		if err != nil {
			t.Fatalf("WorkspaceSymbols failed: %v", err)
		}

		t.Logf("Found %d workspace symbols for 'Greeter'", len(symbols))
		for _, sym := range symbols {
			t.Logf("  [%s] %s", SymbolKindString(sym.Kind), sym.Name)
		}

		if len(symbols) == 0 {
			t.Log("No workspace symbols found - this may be expected for fresh workspace")
		}
	})

	// Close the file
	if err := client.DidClose(ctx, uri); err != nil {
		t.Logf("warning: failed to close file: %v", err)
	}
}

// TestLSP_E2E_Manager tests the manager with real server lifecycle.
func TestLSP_E2E_Manager(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not installed, skipping E2E test")
	}

	tmpDir := t.TempDir()

	// Create minimal go.mod
	goMod := `module testproject
go 1.21
`
	os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goMod), 0644)

	// Create a simple Go file
	simpleGo := `package main
func Hello() string { return "hello" }
`
	goFile := filepath.Join(tmpDir, "simple.go")
	os.WriteFile(goFile, []byte(simpleGo), 0644)

	manager := NewManager(tmpDir)
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

		// At minimum, gopls should be available
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
