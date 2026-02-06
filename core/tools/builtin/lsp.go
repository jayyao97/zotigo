package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/lsp"
)

// LSPTool provides Language Server Protocol operations for code intelligence.
type LSPTool struct {
	manager *lsp.Manager
}

// NewLSPTool creates a new LSP tool with the given manager.
func NewLSPTool(manager *lsp.Manager) *LSPTool {
	return &LSPTool{manager: manager}
}

func (t *LSPTool) Name() string { return "lsp" }
func (t *LSPTool) Description() string {
	return `Access code intelligence via Language Server Protocol. Use for go-to-definition, find-references, hover info, and diagnostics. Supports Go (gopls), TypeScript (typescript-language-server), Python (pylsp), Rust (rust-analyzer), C++ (clangd), and Java (jdtls).`
}

func (t *LSPTool) Schema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"operation": map[string]any{
				"type":        "string",
				"description": "The LSP operation to perform",
				"enum":        []string{"definition", "references", "hover", "implementation", "document_symbols", "workspace_symbols", "diagnostics", "list_languages"},
			},
			"file_path": map[string]any{
				"type":        "string",
				"description": "Absolute path to the source file (required for most operations)",
			},
			"line": map[string]any{
				"type":        "integer",
				"description": "Line number (0-indexed, required for position-based operations)",
			},
			"character": map[string]any{
				"type":        "integer",
				"description": "Character/column offset (0-indexed, required for position-based operations)",
			},
			"query": map[string]any{
				"type":        "string",
				"description": "Search query for workspace_symbols operation",
			},
			"language": map[string]any{
				"type":        "string",
				"description": "Language identifier for workspace_symbols (e.g., 'go', 'typescript', 'python')",
			},
			"include_declaration": map[string]any{
				"type":        "boolean",
				"description": "Include the declaration in references results (default: true)",
			},
		},
		"required": []string{"operation"},
	}
}

func (t *LSPTool) Execute(ctx context.Context, exec executor.Executor, argsJSON string) (any, error) {
	if t.manager == nil {
		return nil, fmt.Errorf("LSP manager not initialized")
	}

	var args struct {
		Operation          string `json:"operation"`
		FilePath           string `json:"file_path"`
		Line               int    `json:"line"`
		Character          int    `json:"character"`
		Query              string `json:"query"`
		Language           string `json:"language"`
		IncludeDeclaration *bool  `json:"include_declaration"`
	}

	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	switch args.Operation {
	case "list_languages":
		return t.listLanguages()

	case "definition":
		if args.FilePath == "" {
			return nil, fmt.Errorf("file_path is required for definition")
		}
		return t.definition(ctx, args.FilePath, args.Line, args.Character)

	case "references":
		if args.FilePath == "" {
			return nil, fmt.Errorf("file_path is required for references")
		}
		includeDecl := true
		if args.IncludeDeclaration != nil {
			includeDecl = *args.IncludeDeclaration
		}
		return t.references(ctx, args.FilePath, args.Line, args.Character, includeDecl)

	case "hover":
		if args.FilePath == "" {
			return nil, fmt.Errorf("file_path is required for hover")
		}
		return t.hover(ctx, args.FilePath, args.Line, args.Character)

	case "implementation":
		if args.FilePath == "" {
			return nil, fmt.Errorf("file_path is required for implementation")
		}
		return t.implementation(ctx, args.FilePath, args.Line, args.Character)

	case "document_symbols":
		if args.FilePath == "" {
			return nil, fmt.Errorf("file_path is required for document_symbols")
		}
		return t.documentSymbols(ctx, args.FilePath)

	case "workspace_symbols":
		if args.Language == "" {
			return nil, fmt.Errorf("language is required for workspace_symbols")
		}
		return t.workspaceSymbols(ctx, args.Language, args.Query)

	case "diagnostics":
		if args.FilePath == "" {
			return nil, fmt.Errorf("file_path is required for diagnostics")
		}
		return t.diagnostics(args.FilePath)

	default:
		return nil, fmt.Errorf("unknown operation: %s", args.Operation)
	}
}

func (t *LSPTool) listLanguages() (string, error) {
	configured := t.manager.GetConfiguredLanguages()
	available := t.manager.GetAvailableLanguages()

	availableSet := make(map[string]bool)
	for _, lang := range available {
		availableSet[lang] = true
	}

	var sb strings.Builder
	sb.WriteString("LSP Language Servers:\n\n")

	for _, lang := range configured {
		config, _ := t.manager.GetServerInfo(lang)
		status := "❌ Not installed"
		if availableSet[lang] {
			status = "✓ Available"
		}
		sb.WriteString(fmt.Sprintf("  %s (%s): %s\n", lang, config.Command, status))
		sb.WriteString(fmt.Sprintf("    Extensions: %s\n", strings.Join(config.Extensions, ", ")))
	}

	return sb.String(), nil
}

func (t *LSPTool) definition(ctx context.Context, filePath string, line, character int) (string, error) {
	locations, err := t.manager.Definition(ctx, filePath, line, character)
	if err != nil {
		return "", err
	}

	if len(locations) == 0 {
		return "No definition found", nil
	}

	return t.formatLocations("Definition", locations), nil
}

func (t *LSPTool) references(ctx context.Context, filePath string, line, character int, includeDecl bool) (string, error) {
	locations, err := t.manager.References(ctx, filePath, line, character, includeDecl)
	if err != nil {
		return "", err
	}

	if len(locations) == 0 {
		return "No references found", nil
	}

	return t.formatLocations("References", locations), nil
}

func (t *LSPTool) hover(ctx context.Context, filePath string, line, character int) (string, error) {
	result, err := t.manager.Hover(ctx, filePath, line, character)
	if err != nil {
		return "", err
	}

	if result == nil || result.Contents == "" {
		return "No hover information available", nil
	}

	return fmt.Sprintf("Hover Information:\n\n%s", result.Contents), nil
}

func (t *LSPTool) implementation(ctx context.Context, filePath string, line, character int) (string, error) {
	locations, err := t.manager.Implementation(ctx, filePath, line, character)
	if err != nil {
		return "", err
	}

	if len(locations) == 0 {
		return "No implementations found", nil
	}

	return t.formatLocations("Implementations", locations), nil
}

func (t *LSPTool) documentSymbols(ctx context.Context, filePath string) (string, error) {
	symbols, err := t.manager.DocumentSymbols(ctx, filePath)
	if err != nil {
		return "", err
	}

	if len(symbols) == 0 {
		return "No symbols found", nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Document Symbols (%d):\n\n", len(symbols)))

	for _, sym := range symbols {
		kind := lsp.SymbolKindString(sym.Kind)
		loc := fmt.Sprintf("%d:%d", sym.Location.Range.Start.Line+1, sym.Location.Range.Start.Character+1)
		if sym.ContainerName != "" {
			sb.WriteString(fmt.Sprintf("  [%s] %s.%s (line %s)\n", kind, sym.ContainerName, sym.Name, loc))
		} else {
			sb.WriteString(fmt.Sprintf("  [%s] %s (line %s)\n", kind, sym.Name, loc))
		}
	}

	return sb.String(), nil
}

func (t *LSPTool) workspaceSymbols(ctx context.Context, language, query string) (string, error) {
	symbols, err := t.manager.WorkspaceSymbols(ctx, language, query)
	if err != nil {
		return "", err
	}

	if len(symbols) == 0 {
		return "No symbols found", nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Workspace Symbols for '%s' (%d results):\n\n", query, len(symbols)))

	for _, sym := range symbols {
		kind := lsp.SymbolKindString(sym.Kind)
		// Extract just the file name from URI
		uri := sym.Location.URI
		if strings.HasPrefix(uri, "file://") {
			uri = uri[7:]
		}
		loc := fmt.Sprintf("%s:%d", uri, sym.Location.Range.Start.Line+1)
		sb.WriteString(fmt.Sprintf("  [%s] %s (%s)\n", kind, sym.Name, loc))
	}

	return sb.String(), nil
}

func (t *LSPTool) diagnostics(filePath string) (string, error) {
	diags, err := t.manager.GetDiagnostics(filePath)
	if err != nil {
		return "", err
	}

	if len(diags) == 0 {
		return "No diagnostics for this file", nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Diagnostics (%d):\n\n", len(diags)))

	for _, d := range diags {
		severity := lsp.SeverityString(d.Severity)
		loc := fmt.Sprintf("%d:%d", d.Range.Start.Line+1, d.Range.Start.Character+1)
		sb.WriteString(fmt.Sprintf("  [%s] %s (line %s)\n", severity, d.Message, loc))
		if d.Source != "" {
			sb.WriteString(fmt.Sprintf("    Source: %s\n", d.Source))
		}
	}

	return sb.String(), nil
}

func (t *LSPTool) formatLocations(title string, locations []lsp.Location) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s (%d):\n\n", title, len(locations)))

	for _, loc := range locations {
		uri := loc.URI
		if strings.HasPrefix(uri, "file://") {
			uri = uri[7:]
		}
		line := loc.Range.Start.Line + 1
		char := loc.Range.Start.Character + 1
		sb.WriteString(fmt.Sprintf("  %s:%d:%d\n", uri, line, char))
	}

	return sb.String()
}
