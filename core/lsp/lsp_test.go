package lsp

import (
	"testing"
)

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
