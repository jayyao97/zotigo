// Package lsp provides Language Server Protocol client functionality
// for code intelligence features like diagnostics, go-to-definition, and references.
package lsp

import "go.lsp.dev/protocol"

// ServerConfig configures an LSP server for a specific language.
type ServerConfig struct {
	// Language identifier (e.g., "go", "typescript", "python")
	Language string
	// Command to start the LSP server
	Command string
	// Arguments for the server command
	Args []string
	// File extensions this server handles
	Extensions []string
	// InitializationOptions for the server
	InitOptions map[string]any
}

// Location represents a location in a source file.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// Range represents a range in a text document.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Position represents a position in a text document.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Diagnostic represents a diagnostic, such as a compiler error or warning.
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity"` // 1=Error, 2=Warning, 3=Information, 4=Hint
	Code     string `json:"code,omitempty"`
	Source   string `json:"source,omitempty"`
	Message  string `json:"message"`
}

// HoverResult contains the result of a hover request.
type HoverResult struct {
	Contents string `json:"contents"`
	Range    *Range `json:"range,omitempty"`
}

// CompletionItem represents a completion item.
type CompletionItem struct {
	Label      string `json:"label"`
	Kind       int    `json:"kind,omitempty"`
	Detail     string `json:"detail,omitempty"`
	InsertText string `json:"insertText,omitempty"`
}

// SymbolInformation represents information about a symbol.
type SymbolInformation struct {
	Name          string   `json:"name"`
	Kind          int      `json:"kind"`
	Location      Location `json:"location"`
	ContainerName string   `json:"containerName,omitempty"`
}

// DiagnosticSeverity constants
const (
	SeverityError       = 1
	SeverityWarning     = 2
	SeverityInformation = 3
	SeverityHint        = 4
)

// SymbolKind constants
const (
	SymbolKindFile          = 1
	SymbolKindModule        = 2
	SymbolKindNamespace     = 3
	SymbolKindPackage       = 4
	SymbolKindClass         = 5
	SymbolKindMethod        = 6
	SymbolKindProperty      = 7
	SymbolKindField         = 8
	SymbolKindConstructor   = 9
	SymbolKindEnum          = 10
	SymbolKindInterface     = 11
	SymbolKindFunction      = 12
	SymbolKindVariable      = 13
	SymbolKindConstant      = 14
	SymbolKindStringLiteral = 15
	SymbolKindNumber        = 16
	SymbolKindBoolean       = 17
	SymbolKindArray         = 18
	SymbolKindObject        = 19
	SymbolKindKey           = 20
	SymbolKindNull          = 21
	SymbolKindEnumMember    = 22
	SymbolKindStruct        = 23
	SymbolKindEvent         = 24
	SymbolKindOperator      = 25
	SymbolKindTypeParameter = 26
)

// SeverityString returns a human-readable string for a diagnostic severity.
func SeverityString(severity int) string {
	switch severity {
	case SeverityError:
		return "Error"
	case SeverityWarning:
		return "Warning"
	case SeverityInformation:
		return "Information"
	case SeverityHint:
		return "Hint"
	default:
		return "Unknown"
	}
}

// SymbolKindString returns a human-readable string for a symbol kind.
func SymbolKindString(kind int) string {
	switch kind {
	case SymbolKindFile:
		return "File"
	case SymbolKindModule:
		return "Module"
	case SymbolKindNamespace:
		return "Namespace"
	case SymbolKindPackage:
		return "Package"
	case SymbolKindClass:
		return "Class"
	case SymbolKindMethod:
		return "Method"
	case SymbolKindProperty:
		return "Property"
	case SymbolKindField:
		return "Field"
	case SymbolKindConstructor:
		return "Constructor"
	case SymbolKindEnum:
		return "Enum"
	case SymbolKindInterface:
		return "Interface"
	case SymbolKindFunction:
		return "Function"
	case SymbolKindVariable:
		return "Variable"
	case SymbolKindConstant:
		return "Constant"
	case SymbolKindStruct:
		return "Struct"
	default:
		return "Symbol"
	}
}

// ToProtocolPosition converts our Position to protocol.Position.
func (p Position) ToProtocol() protocol.Position {
	return protocol.Position{
		Line:      uint32(p.Line),
		Character: uint32(p.Character),
	}
}

// FromProtocolPosition converts protocol.Position to our Position.
func FromProtocolPosition(p protocol.Position) Position {
	return Position{
		Line:      int(p.Line),
		Character: int(p.Character),
	}
}

// FromProtocolRange converts protocol.Range to our Range.
func FromProtocolRange(r protocol.Range) Range {
	return Range{
		Start: FromProtocolPosition(r.Start),
		End:   FromProtocolPosition(r.End),
	}
}

// FromProtocolLocation converts protocol.Location to our Location.
func FromProtocolLocation(l protocol.Location) Location {
	return Location{
		URI:   string(l.URI),
		Range: FromProtocolRange(l.Range),
	}
}
