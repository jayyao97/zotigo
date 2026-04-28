package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.uber.org/zap"
)

// Client represents an LSP client connected to a language server.
type Client struct {
	config  ServerConfig
	cmd     *exec.Cmd
	conn    jsonrpc2.Conn
	server  protocol.Server
	rootURI protocol.URI

	mu           sync.RWMutex
	initialized  bool
	capabilities protocol.ServerCapabilities
	diagnostics  map[protocol.URI][]protocol.Diagnostic
}

// clientHandler implements protocol.Client to receive notifications from server.
type clientHandler struct {
	client *Client
}

func (h *clientHandler) Progress(ctx context.Context, params *protocol.ProgressParams) error {
	return nil
}

func (h *clientHandler) WorkDoneProgressCreate(ctx context.Context, params *protocol.WorkDoneProgressCreateParams) error {
	return nil
}

func (h *clientHandler) LogMessage(ctx context.Context, params *protocol.LogMessageParams) error {
	return nil
}

func (h *clientHandler) PublishDiagnostics(ctx context.Context, params *protocol.PublishDiagnosticsParams) error {
	h.client.mu.Lock()
	defer h.client.mu.Unlock()
	h.client.diagnostics[params.URI] = params.Diagnostics
	return nil
}

func (h *clientHandler) ShowMessage(ctx context.Context, params *protocol.ShowMessageParams) error {
	return nil
}

func (h *clientHandler) ShowMessageRequest(ctx context.Context, params *protocol.ShowMessageRequestParams) (*protocol.MessageActionItem, error) {
	return nil, nil
}

func (h *clientHandler) Telemetry(ctx context.Context, params interface{}) error {
	return nil
}

func (h *clientHandler) RegisterCapability(ctx context.Context, params *protocol.RegistrationParams) error {
	return nil
}

func (h *clientHandler) UnregisterCapability(ctx context.Context, params *protocol.UnregistrationParams) error {
	return nil
}

func (h *clientHandler) ApplyEdit(ctx context.Context, params *protocol.ApplyWorkspaceEditParams) (bool, error) {
	return false, nil
}

func (h *clientHandler) Configuration(ctx context.Context, params *protocol.ConfigurationParams) ([]interface{}, error) {
	return nil, nil
}

func (h *clientHandler) WorkspaceFolders(ctx context.Context) ([]protocol.WorkspaceFolder, error) {
	return nil, nil
}

// NewClient creates a new LSP client for the given server configuration.
func NewClient(config ServerConfig, rootPath string) *Client {
	return &Client{
		config:      config,
		rootURI:     protocol.URI("file://" + rootPath),
		diagnostics: make(map[protocol.URI][]protocol.Diagnostic),
	}
}

// Start starts the LSP server process and establishes the connection.
func (c *Client) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.initialized {
		return nil
	}

	// Start the server process
	c.cmd = exec.CommandContext(ctx, c.config.Command, c.config.Args...)
	c.cmd.Stderr = os.Stderr

	stdin, err := c.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	if err := c.cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return fmt.Errorf("failed to start LSP server: %w", err)
	}

	// Create JSON-RPC stream using the library's helper
	rwc := &readWriteCloser{reader: stdout, writer: stdin}
	stream := jsonrpc2.NewStream(rwc)

	// Create logger (silent)
	logger := zap.NewNop()

	// Create client handler for receiving notifications
	handler := &clientHandler{client: c}

	// Create connection and server proxy
	_, c.conn, c.server = protocol.NewClient(ctx, handler, stream, logger)

	// Initialize the server
	if err := c.initialize(ctx); err != nil {
		_ = c.Stop()
		return err
	}

	c.initialized = true
	return nil
}

// readWriteCloser combines reader and writer into io.ReadWriteCloser.
type readWriteCloser struct {
	reader io.ReadCloser
	writer io.WriteCloser
}

func (rwc *readWriteCloser) Read(p []byte) (n int, err error) {
	return rwc.reader.Read(p)
}

func (rwc *readWriteCloser) Write(p []byte) (n int, err error) {
	return rwc.writer.Write(p)
}

func (rwc *readWriteCloser) Close() error {
	_ = rwc.reader.Close()
	_ = rwc.writer.Close()
	return nil
}

// initialize sends the initialize request to the server.
func (c *Client) initialize(ctx context.Context) error {
	params := &protocol.InitializeParams{
		ProcessID: int32(os.Getpid()),
		RootURI:   protocol.DocumentURI(c.rootURI),
		Capabilities: protocol.ClientCapabilities{
			TextDocument: &protocol.TextDocumentClientCapabilities{
				Synchronization: &protocol.TextDocumentSyncClientCapabilities{
					DidSave: true,
				},
				Completion: &protocol.CompletionTextDocumentClientCapabilities{
					CompletionItem: &protocol.CompletionTextDocumentClientCapabilitiesItem{},
				},
				Hover: &protocol.HoverTextDocumentClientCapabilities{
					ContentFormat: []protocol.MarkupKind{protocol.Markdown, protocol.PlainText},
				},
				Definition:     &protocol.DefinitionTextDocumentClientCapabilities{},
				References:     &protocol.ReferencesTextDocumentClientCapabilities{},
				Implementation: &protocol.ImplementationTextDocumentClientCapabilities{},
				DocumentSymbol: &protocol.DocumentSymbolClientCapabilities{},
				PublishDiagnostics: &protocol.PublishDiagnosticsClientCapabilities{
					RelatedInformation: true,
				},
			},
			Workspace: &protocol.WorkspaceClientCapabilities{
				Symbol: &protocol.WorkspaceSymbolClientCapabilities{},
			},
		},
	}

	if c.config.InitOptions != nil {
		params.InitializationOptions = c.config.InitOptions
	}

	result, err := c.server.Initialize(ctx, params)
	if err != nil {
		return fmt.Errorf("initialize failed: %w", err)
	}

	c.capabilities = result.Capabilities

	// Send initialized notification
	if err := c.server.Initialized(ctx, &protocol.InitializedParams{}); err != nil {
		return fmt.Errorf("initialized notification failed: %w", err)
	}

	return nil
}

// Stop stops the LSP server.
func (c *Client) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.initialized {
		return nil
	}

	// Use a short timeout for shutdown to avoid hanging
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Send shutdown request (may timeout, that's ok)
	if c.server != nil {
		// Try to shutdown gracefully, but don't block forever
		done := make(chan struct{})
		go func() {
			_ = c.server.Shutdown(ctx)
			_ = c.server.Exit(ctx)
			close(done)
		}()

		select {
		case <-done:
			// Graceful shutdown completed
		case <-ctx.Done():
			// Timeout, proceed to force kill
		}
	}

	// Close connection
	if c.conn != nil {
		_ = c.conn.Close()
	}

	// Kill process if still running
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	}

	c.initialized = false
	return nil
}

// IsInitialized returns whether the client is initialized.
func (c *Client) IsInitialized() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.initialized
}

// Language returns the language this client handles.
func (c *Client) Language() string {
	return c.config.Language
}

// DidOpen notifies the server that a document was opened.
func (c *Client) DidOpen(ctx context.Context, uri, languageID, content string) error {
	if !c.IsInitialized() {
		return fmt.Errorf("client not initialized")
	}

	params := &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:        protocol.DocumentURI(uri),
			LanguageID: protocol.LanguageIdentifier(languageID),
			Version:    1,
			Text:       content,
		},
	}

	return c.server.DidOpen(ctx, params)
}

// DidClose notifies the server that a document was closed.
func (c *Client) DidClose(ctx context.Context, uri string) error {
	if !c.IsInitialized() {
		return fmt.Errorf("client not initialized")
	}

	params := &protocol.DidCloseTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{
			URI: protocol.DocumentURI(uri),
		},
	}

	return c.server.DidClose(ctx, params)
}

// Definition requests the definition location of a symbol.
func (c *Client) Definition(ctx context.Context, uri string, line, character int) ([]Location, error) {
	if !c.IsInitialized() {
		return nil, fmt.Errorf("client not initialized")
	}

	params := &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
			Position: protocol.Position{
				Line:      uint32(line),
				Character: uint32(character),
			},
		},
	}

	result, err := c.server.Definition(ctx, params)
	if err != nil {
		return nil, err
	}

	return c.convertLocations(result), nil
}

// References finds all references to a symbol.
func (c *Client) References(ctx context.Context, uri string, line, character int, includeDeclaration bool) ([]Location, error) {
	if !c.IsInitialized() {
		return nil, fmt.Errorf("client not initialized")
	}

	params := &protocol.ReferenceParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
			Position: protocol.Position{
				Line:      uint32(line),
				Character: uint32(character),
			},
		},
		Context: protocol.ReferenceContext{
			IncludeDeclaration: includeDeclaration,
		},
	}

	result, err := c.server.References(ctx, params)
	if err != nil {
		return nil, err
	}

	var locations []Location
	for _, loc := range result {
		locations = append(locations, FromProtocolLocation(loc))
	}
	return locations, nil
}

// Hover returns hover information for a position.
func (c *Client) Hover(ctx context.Context, uri string, line, character int) (*HoverResult, error) {
	if !c.IsInitialized() {
		return nil, fmt.Errorf("client not initialized")
	}

	params := &protocol.HoverParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
			Position: protocol.Position{
				Line:      uint32(line),
				Character: uint32(character),
			},
		},
	}

	result, err := c.server.Hover(ctx, params)
	if err != nil {
		return nil, err
	}

	if result == nil {
		return nil, nil
	}

	hover := &HoverResult{
		Contents: result.Contents.Value,
	}

	if result.Range != nil {
		r := FromProtocolRange(*result.Range)
		hover.Range = &r
	}

	return hover, nil
}

// Implementation finds the implementation of an interface method.
func (c *Client) Implementation(ctx context.Context, uri string, line, character int) ([]Location, error) {
	if !c.IsInitialized() {
		return nil, fmt.Errorf("client not initialized")
	}

	params := &protocol.ImplementationParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
			Position: protocol.Position{
				Line:      uint32(line),
				Character: uint32(character),
			},
		},
	}

	result, err := c.server.Implementation(ctx, params)
	if err != nil {
		return nil, err
	}

	return c.convertLocations(result), nil
}

// DocumentSymbols returns all symbols in a document.
func (c *Client) DocumentSymbols(ctx context.Context, uri string) ([]SymbolInformation, error) {
	if !c.IsInitialized() {
		return nil, fmt.Errorf("client not initialized")
	}

	params := &protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
	}

	result, err := c.server.DocumentSymbol(ctx, params)
	if err != nil {
		return nil, err
	}

	var symbols []SymbolInformation
	for _, item := range result {
		// Try direct type assertion first
		if si, ok := item.(protocol.SymbolInformation); ok {
			symbols = append(symbols, SymbolInformation{
				Name:          si.Name,
				Kind:          int(si.Kind),
				Location:      FromProtocolLocation(si.Location),
				ContainerName: si.ContainerName,
			})
			continue
		}
		if ds, ok := item.(protocol.DocumentSymbol); ok {
			symbols = append(symbols, c.flattenDocumentSymbol(uri, ds, "")...)
			continue
		}

		// Fallback: JSON re-marshal for map[string]interface{} case
		data, err := json.Marshal(item)
		if err != nil {
			continue
		}

		// Try DocumentSymbol first (more common for modern LSP servers)
		var ds protocol.DocumentSymbol
		if err := json.Unmarshal(data, &ds); err == nil && ds.Name != "" {
			symbols = append(symbols, c.flattenDocumentSymbol(uri, ds, "")...)
			continue
		}

		// Try SymbolInformation
		var si protocol.SymbolInformation
		if err := json.Unmarshal(data, &si); err == nil && si.Name != "" {
			symbols = append(symbols, SymbolInformation{
				Name:          si.Name,
				Kind:          int(si.Kind),
				Location:      FromProtocolLocation(si.Location),
				ContainerName: si.ContainerName,
			})
		}
	}

	return symbols, nil
}

// flattenDocumentSymbol recursively flattens DocumentSymbol hierarchy.
func (c *Client) flattenDocumentSymbol(uri string, ds protocol.DocumentSymbol, container string) []SymbolInformation {
	var symbols []SymbolInformation

	symbols = append(symbols, SymbolInformation{
		Name: ds.Name,
		Kind: int(ds.Kind),
		Location: Location{
			URI:   uri,
			Range: FromProtocolRange(ds.Range),
		},
		ContainerName: container,
	})

	for _, child := range ds.Children {
		symbols = append(symbols, c.flattenDocumentSymbol(uri, child, ds.Name)...)
	}

	return symbols
}

// WorkspaceSymbols searches for symbols in the workspace.
func (c *Client) WorkspaceSymbols(ctx context.Context, query string) ([]SymbolInformation, error) {
	if !c.IsInitialized() {
		return nil, fmt.Errorf("client not initialized")
	}

	params := &protocol.WorkspaceSymbolParams{
		Query: query,
	}

	result, err := c.server.Symbols(ctx, params)
	if err != nil {
		return nil, err
	}

	var symbols []SymbolInformation
	for _, si := range result {
		symbols = append(symbols, SymbolInformation{
			Name:          si.Name,
			Kind:          int(si.Kind),
			Location:      FromProtocolLocation(si.Location),
			ContainerName: si.ContainerName,
		})
	}

	return symbols, nil
}

// GetDiagnostics returns cached diagnostics for a file.
func (c *Client) GetDiagnostics(uri string) []Diagnostic {
	c.mu.RLock()
	defer c.mu.RUnlock()

	protocolDiags := c.diagnostics[protocol.URI(uri)]
	var diags []Diagnostic
	for _, d := range protocolDiags {
		diag := Diagnostic{
			Range:    FromProtocolRange(d.Range),
			Severity: int(d.Severity),
			Message:  d.Message,
			Source:   d.Source,
		}
		if d.Code != nil {
			diag.Code = fmt.Sprintf("%v", d.Code)
		}
		diags = append(diags, diag)
	}
	return diags
}

// convertLocations converts definition/implementation results to Location slice.
func (c *Client) convertLocations(result []protocol.Location) []Location {
	var locations []Location
	for _, loc := range result {
		locations = append(locations, FromProtocolLocation(loc))
	}
	return locations
}
