package lsp

import (
	"testing"
)

func TestServerConfig(t *testing.T) {
	config := ServerConfig{
		Language:   "go",
		Command:    "gopls",
		Args:       []string{"serve"},
		Extensions: []string{".go"},
	}

	if config.Language != "go" {
		t.Errorf("expected language 'go', got '%s'", config.Language)
	}
	if config.Command != "gopls" {
		t.Errorf("expected command 'gopls', got '%s'", config.Command)
	}
	if len(config.Extensions) != 1 || config.Extensions[0] != ".go" {
		t.Errorf("unexpected extensions: %v", config.Extensions)
	}
}

func TestSeverityString(t *testing.T) {
	tests := []struct {
		severity int
		expected string
	}{
		{SeverityError, "Error"},
		{SeverityWarning, "Warning"},
		{SeverityInformation, "Information"},
		{SeverityHint, "Hint"},
		{99, "Unknown"},
	}

	for _, tc := range tests {
		result := SeverityString(tc.severity)
		if result != tc.expected {
			t.Errorf("SeverityString(%d) = %s, expected %s", tc.severity, result, tc.expected)
		}
	}
}

func TestSymbolKindToString(t *testing.T) {
	tests := []struct {
		kind     int
		expected string
	}{
		{SymbolKindFile, "File"},
		{SymbolKindModule, "Module"},
		{SymbolKindClass, "Class"},
		{SymbolKindMethod, "Method"},
		{SymbolKindFunction, "Function"},
		{SymbolKindVariable, "Variable"},
		{SymbolKindInterface, "Interface"},
		{SymbolKindStruct, "Struct"},
		{999, "Symbol"},
	}

	for _, tc := range tests {
		result := SymbolKindString(tc.kind)
		if result != tc.expected {
			t.Errorf("SymbolKindString(%d) = %s, expected %s", tc.kind, result, tc.expected)
		}
	}
}

func TestNewManager(t *testing.T) {
	m := NewManager("/test/path")

	if m.rootPath != "/test/path" {
		t.Errorf("expected rootPath '/test/path', got '%s'", m.rootPath)
	}

	// Check default languages are registered
	configured := m.GetConfiguredLanguages()
	if len(configured) < 6 {
		t.Errorf("expected at least 6 configured languages, got %d", len(configured))
	}

	// Check specific languages
	languages := []string{"go", "typescript", "python", "rust", "cpp", "java"}
	for _, lang := range languages {
		config, ok := m.GetServerInfo(lang)
		if !ok {
			t.Errorf("expected language '%s' to be configured", lang)
		}
		if config.Language != lang {
			t.Errorf("expected config.Language '%s', got '%s'", lang, config.Language)
		}
	}
}

func TestManager_DetectLanguage(t *testing.T) {
	m := NewManager("/test")

	tests := []struct {
		filePath string
		expected string
	}{
		{"/path/to/main.go", "go"},
		{"/path/to/app.ts", "typescript"},
		{"/path/to/component.tsx", "typescript"},
		{"/path/to/app.js", "typescript"},
		{"/path/to/script.py", "python"},
		{"/path/to/main.rs", "rust"},
		{"/path/to/main.cpp", "cpp"},
		{"/path/to/main.c", "cpp"},
		{"/path/to/header.h", "cpp"},
		{"/path/to/App.java", "java"},
		{"/path/to/unknown.xyz", ""},
	}

	for _, tc := range tests {
		result := m.detectLanguage(tc.filePath)
		if result != tc.expected {
			t.Errorf("detectLanguage(%s) = '%s', expected '%s'", tc.filePath, result, tc.expected)
		}
	}
}

func TestManager_RegisterServer(t *testing.T) {
	m := NewManager("/test")

	customConfig := ServerConfig{
		Language:   "custom",
		Command:    "custom-lsp",
		Args:       []string{"--stdio"},
		Extensions: []string{".custom"},
	}

	m.RegisterServer(customConfig)

	config, ok := m.GetServerInfo("custom")
	if !ok {
		t.Fatal("custom server not registered")
	}
	if config.Command != "custom-lsp" {
		t.Errorf("expected command 'custom-lsp', got '%s'", config.Command)
	}

	// Check extension detection works
	lang := m.detectLanguage("/path/test.custom")
	if lang != "custom" {
		t.Errorf("expected language 'custom', got '%s'", lang)
	}
}

func TestPosition_Conversion(t *testing.T) {
	pos := Position{Line: 10, Character: 5}
	protoPos := pos.ToProtocol()

	if protoPos.Line != 10 {
		t.Errorf("expected Line 10, got %d", protoPos.Line)
	}
	if protoPos.Character != 5 {
		t.Errorf("expected Character 5, got %d", protoPos.Character)
	}

	// Test reverse conversion
	converted := FromProtocolPosition(protoPos)
	if converted.Line != 10 {
		t.Errorf("expected Line 10, got %d", converted.Line)
	}
	if converted.Character != 5 {
		t.Errorf("expected Character 5, got %d", converted.Character)
	}
}

func TestLocation_Types(t *testing.T) {
	loc := Location{
		URI: "file:///path/to/file.go",
		Range: Range{
			Start: Position{Line: 10, Character: 5},
			End:   Position{Line: 10, Character: 15},
		},
	}

	if loc.URI != "file:///path/to/file.go" {
		t.Errorf("unexpected URI: %s", loc.URI)
	}
	if loc.Range.Start.Line != 10 {
		t.Errorf("unexpected start line: %d", loc.Range.Start.Line)
	}
	if loc.Range.End.Character != 15 {
		t.Errorf("unexpected end character: %d", loc.Range.End.Character)
	}
}

func TestDiagnostic_Types(t *testing.T) {
	diag := Diagnostic{
		Range: Range{
			Start: Position{Line: 5, Character: 10},
			End:   Position{Line: 5, Character: 20},
		},
		Severity: SeverityError,
		Code:     "E001",
		Source:   "gopls",
		Message:  "undefined: foo",
	}

	if diag.Severity != SeverityError {
		t.Errorf("expected severity Error, got %d", diag.Severity)
	}
	if diag.Message != "undefined: foo" {
		t.Errorf("unexpected message: %s", diag.Message)
	}
	if diag.Source != "gopls" {
		t.Errorf("unexpected source: %s", diag.Source)
	}
}

func TestHoverResult_Types(t *testing.T) {
	r := Range{Start: Position{Line: 1, Character: 0}, End: Position{Line: 1, Character: 10}}
	hover := HoverResult{
		Contents: "func Foo() string",
		Range:    &r,
	}

	if hover.Contents != "func Foo() string" {
		t.Errorf("unexpected contents: %s", hover.Contents)
	}
	if hover.Range == nil {
		t.Error("expected Range to be set")
	}
}

func TestSymbolInformation_Types(t *testing.T) {
	sym := SymbolInformation{
		Name: "MyFunction",
		Kind: SymbolKindFunction,
		Location: Location{
			URI: "file:///path/to/file.go",
			Range: Range{
				Start: Position{Line: 10, Character: 0},
				End:   Position{Line: 20, Character: 1},
			},
		},
		ContainerName: "main",
	}

	if sym.Name != "MyFunction" {
		t.Errorf("unexpected name: %s", sym.Name)
	}
	if sym.Kind != SymbolKindFunction {
		t.Errorf("unexpected kind: %d", sym.Kind)
	}
	if sym.ContainerName != "main" {
		t.Errorf("unexpected container: %s", sym.ContainerName)
	}
}

func TestCompletionItem_Types(t *testing.T) {
	item := CompletionItem{
		Label:      "myFunc",
		Kind:       3, // Function
		Detail:     "func(x int) string",
		InsertText: "myFunc(${1:x})",
	}

	if item.Label != "myFunc" {
		t.Errorf("unexpected label: %s", item.Label)
	}
	if item.Detail != "func(x int) string" {
		t.Errorf("unexpected detail: %s", item.Detail)
	}
}
