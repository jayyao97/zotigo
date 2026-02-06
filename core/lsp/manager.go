package lsp

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Manager manages multiple LSP clients for different languages.
type Manager struct {
	mu       sync.RWMutex
	clients  map[string]*Client // language -> client
	configs  map[string]ServerConfig
	rootPath string
}

// NewManager creates a new LSP manager.
func NewManager(rootPath string) *Manager {
	m := &Manager{
		clients:  make(map[string]*Client),
		configs:  make(map[string]ServerConfig),
		rootPath: rootPath,
	}

	// Register default server configurations
	m.registerDefaults()

	return m
}

// registerDefaults registers default LSP server configurations.
func (m *Manager) registerDefaults() {
	// Go (gopls)
	m.configs["go"] = ServerConfig{
		Language:   "go",
		Command:    "gopls",
		Args:       []string{"serve"},
		Extensions: []string{".go"},
	}

	// TypeScript/JavaScript (typescript-language-server)
	m.configs["typescript"] = ServerConfig{
		Language:   "typescript",
		Command:    "typescript-language-server",
		Args:       []string{"--stdio"},
		Extensions: []string{".ts", ".tsx", ".js", ".jsx"},
	}

	// Python (pylsp or pyright)
	m.configs["python"] = ServerConfig{
		Language:   "python",
		Command:    "pylsp",
		Args:       []string{},
		Extensions: []string{".py"},
	}

	// Rust (rust-analyzer)
	m.configs["rust"] = ServerConfig{
		Language:   "rust",
		Command:    "rust-analyzer",
		Args:       []string{},
		Extensions: []string{".rs"},
	}

	// C/C++ (clangd)
	m.configs["cpp"] = ServerConfig{
		Language:   "cpp",
		Command:    "clangd",
		Args:       []string{},
		Extensions: []string{".c", ".cpp", ".cc", ".h", ".hpp"},
	}

	// Java (jdtls)
	m.configs["java"] = ServerConfig{
		Language:   "java",
		Command:    "jdtls",
		Args:       []string{},
		Extensions: []string{".java"},
	}
}

// RegisterServer registers a custom LSP server configuration.
func (m *Manager) RegisterServer(config ServerConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configs[config.Language] = config
}

// GetClientForFile returns an LSP client appropriate for the given file.
// It automatically starts the server if needed.
func (m *Manager) GetClientForFile(ctx context.Context, filePath string) (*Client, error) {
	lang := m.detectLanguage(filePath)
	if lang == "" {
		return nil, fmt.Errorf("no LSP server configured for file: %s", filePath)
	}

	return m.GetClient(ctx, lang)
}

// GetClient returns an LSP client for the specified language.
// It automatically starts the server if needed.
func (m *Manager) GetClient(ctx context.Context, language string) (*Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Return existing client if available
	if client, ok := m.clients[language]; ok && client.IsInitialized() {
		return client, nil
	}

	// Get configuration
	config, ok := m.configs[language]
	if !ok {
		return nil, fmt.Errorf("no LSP server configured for language: %s", language)
	}

	// Check if server is available
	if !m.isServerAvailable(config.Command) {
		return nil, fmt.Errorf("LSP server '%s' not found in PATH for language: %s", config.Command, language)
	}

	// Create and start client
	client := NewClient(config, m.rootPath)
	if err := client.Start(ctx); err != nil {
		return nil, fmt.Errorf("failed to start LSP server for %s: %w", language, err)
	}

	m.clients[language] = client
	return client, nil
}

// detectLanguage determines the language based on file extension.
func (m *Manager) detectLanguage(filePath string) string {
	ext := strings.ToLower(filepath.Ext(filePath))

	for lang, config := range m.configs {
		for _, e := range config.Extensions {
			if e == ext {
				return lang
			}
		}
	}

	return ""
}

// isServerAvailable checks if an LSP server command is available.
func (m *Manager) isServerAvailable(command string) bool {
	_, err := exec.LookPath(command)
	return err == nil
}

// GetAvailableLanguages returns languages with available LSP servers.
func (m *Manager) GetAvailableLanguages() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var available []string
	for lang, config := range m.configs {
		if m.isServerAvailable(config.Command) {
			available = append(available, lang)
		}
	}
	return available
}

// GetConfiguredLanguages returns all configured languages (whether available or not).
func (m *Manager) GetConfiguredLanguages() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var languages []string
	for lang := range m.configs {
		languages = append(languages, lang)
	}
	return languages
}

// GetServerInfo returns information about a language server.
func (m *Manager) GetServerInfo(language string) (ServerConfig, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	config, ok := m.configs[language]
	return config, ok
}

// StopClient stops the LSP client for a specific language.
func (m *Manager) StopClient(language string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	client, ok := m.clients[language]
	if !ok {
		return nil
	}

	err := client.Stop()
	delete(m.clients, language)
	return err
}

// StopAll stops all LSP clients.
func (m *Manager) StopAll() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var lastErr error
	for lang, client := range m.clients {
		if err := client.Stop(); err != nil {
			lastErr = err
		}
		delete(m.clients, lang)
	}
	return lastErr
}

// Definition finds the definition of a symbol.
func (m *Manager) Definition(ctx context.Context, filePath string, line, character int) ([]Location, error) {
	client, err := m.GetClientForFile(ctx, filePath)
	if err != nil {
		return nil, err
	}

	uri := "file://" + filePath
	return client.Definition(ctx, uri, line, character)
}

// References finds all references to a symbol.
func (m *Manager) References(ctx context.Context, filePath string, line, character int, includeDeclaration bool) ([]Location, error) {
	client, err := m.GetClientForFile(ctx, filePath)
	if err != nil {
		return nil, err
	}

	uri := "file://" + filePath
	return client.References(ctx, uri, line, character, includeDeclaration)
}

// Hover returns hover information for a position.
func (m *Manager) Hover(ctx context.Context, filePath string, line, character int) (*HoverResult, error) {
	client, err := m.GetClientForFile(ctx, filePath)
	if err != nil {
		return nil, err
	}

	uri := "file://" + filePath
	return client.Hover(ctx, uri, line, character)
}

// Implementation finds implementations of an interface method.
func (m *Manager) Implementation(ctx context.Context, filePath string, line, character int) ([]Location, error) {
	client, err := m.GetClientForFile(ctx, filePath)
	if err != nil {
		return nil, err
	}

	uri := "file://" + filePath
	return client.Implementation(ctx, uri, line, character)
}

// DocumentSymbols returns all symbols in a document.
func (m *Manager) DocumentSymbols(ctx context.Context, filePath string) ([]SymbolInformation, error) {
	client, err := m.GetClientForFile(ctx, filePath)
	if err != nil {
		return nil, err
	}

	uri := "file://" + filePath
	return client.DocumentSymbols(ctx, uri)
}

// WorkspaceSymbols searches for symbols in the workspace.
func (m *Manager) WorkspaceSymbols(ctx context.Context, language, query string) ([]SymbolInformation, error) {
	client, err := m.GetClient(ctx, language)
	if err != nil {
		return nil, err
	}

	return client.WorkspaceSymbols(ctx, query)
}

// GetDiagnostics returns diagnostics for a file.
func (m *Manager) GetDiagnostics(filePath string) ([]Diagnostic, error) {
	lang := m.detectLanguage(filePath)
	if lang == "" {
		return nil, fmt.Errorf("no LSP server configured for file: %s", filePath)
	}

	m.mu.RLock()
	client, ok := m.clients[lang]
	m.mu.RUnlock()

	if !ok || !client.IsInitialized() {
		return nil, fmt.Errorf("LSP client not running for language: %s", lang)
	}

	uri := "file://" + filePath
	return client.GetDiagnostics(uri), nil
}

// OpenFile notifies the LSP server that a file was opened.
func (m *Manager) OpenFile(ctx context.Context, filePath, content string) error {
	client, err := m.GetClientForFile(ctx, filePath)
	if err != nil {
		return err
	}

	lang := m.detectLanguage(filePath)
	uri := "file://" + filePath
	return client.DidOpen(ctx, uri, lang, content)
}

// CloseFile notifies the LSP server that a file was closed.
func (m *Manager) CloseFile(ctx context.Context, filePath string) error {
	lang := m.detectLanguage(filePath)
	if lang == "" {
		return nil
	}

	m.mu.RLock()
	client, ok := m.clients[lang]
	m.mu.RUnlock()

	if !ok || !client.IsInitialized() {
		return nil
	}

	uri := "file://" + filePath
	return client.DidClose(ctx, uri)
}
