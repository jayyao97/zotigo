---
name: lsp-usage
description: Guide for using Language Server Protocol features for code intelligence. Activates when code navigation, analysis, or refactoring is needed.
aliases:
  - lsp
  - code-intel
  - go-to-definition
---

# LSP Usage Guide

You have access to the `lsp` tool for powerful code intelligence operations. Use this to navigate codebases, understand code structure, and perform precise code analysis.

## When to Use LSP

Use LSP operations when you need to:
- Find where a function, type, or variable is defined
- Discover all usages of a symbol across the codebase
- Understand what a function does (via hover info)
- Find all implementations of an interface
- Get an overview of symbols in a file
- Search for symbols across the entire workspace
- Check for compiler errors and warnings

## Available Operations

### 1. Definition (`definition`)
Find where a symbol is defined.

**Use when:**
- User asks "where is X defined?"
- You need to understand what a function does
- Following a function call to its source

**Example:**
```json
{
  "operation": "definition",
  "file_path": "/path/to/file.go",
  "line": 42,
  "character": 15
}
```

### 2. References (`references`)
Find all places where a symbol is used.

**Use when:**
- User asks "where is X used?"
- Planning a refactor and need to find all usages
- Understanding the impact of changing a function

**Example:**
```json
{
  "operation": "references",
  "file_path": "/path/to/file.go",
  "line": 42,
  "character": 15,
  "include_declaration": true
}
```

### 3. Hover (`hover`)
Get documentation and type information for a symbol.

**Use when:**
- You need to understand what a function does
- Checking the type signature of a function
- Getting quick documentation

**Example:**
```json
{
  "operation": "hover",
  "file_path": "/path/to/file.go",
  "line": 42,
  "character": 15
}
```

### 4. Implementation (`implementation`)
Find implementations of an interface or abstract method.

**Use when:**
- User asks "what implements interface X?"
- Understanding polymorphic code
- Finding concrete implementations

**Example:**
```json
{
  "operation": "implementation",
  "file_path": "/path/to/file.go",
  "line": 10,
  "character": 6
}
```

### 5. Document Symbols (`document_symbols`)
Get all symbols (functions, types, variables) in a file.

**Use when:**
- Getting an overview of a file's structure
- Understanding what's in a file without reading it all
- User asks "what functions are in this file?"

**Example:**
```json
{
  "operation": "document_symbols",
  "file_path": "/path/to/file.go"
}
```

### 6. Workspace Symbols (`workspace_symbols`)
Search for symbols across the entire workspace.

**Use when:**
- Looking for a symbol by name across the project
- User asks "where is the Config struct?"
- Finding all functions matching a pattern

**Example:**
```json
{
  "operation": "workspace_symbols",
  "language": "go",
  "query": "NewClient"
}
```

### 7. Diagnostics (`diagnostics`)
Get compiler errors and warnings for a file.

**Use when:**
- After making code changes, check for errors
- User reports compile errors
- Validating code correctness

**Example:**
```json
{
  "operation": "diagnostics",
  "file_path": "/path/to/file.go"
}
```

### 8. List Languages (`list_languages`)
Check which LSP servers are available.

**Use when:**
- Checking if LSP is available for a language
- Troubleshooting LSP issues

**Example:**
```json
{
  "operation": "list_languages"
}
```

## Supported Languages

| Language | Server | Extensions |
|----------|--------|------------|
| Go | gopls | .go |
| TypeScript | typescript-language-server | .ts, .tsx, .js, .jsx |
| Python | pylsp | .py |
| Rust | rust-analyzer | .rs |
| C/C++ | clangd | .c, .cpp, .cc, .h, .hpp |
| Java | jdtls | .java |

## Best Practices

1. **Position is 0-indexed**: Line and character positions start from 0, not 1. If a user says "line 42", use `line: 41`.

2. **Use absolute paths**: Always provide absolute file paths.

3. **Check availability first**: Use `list_languages` to verify the LSP server is installed before operations.

4. **Combine with file reading**: After finding a definition, read the file to show the actual code.

5. **Handle "not found" gracefully**: LSP may return empty results - inform the user clearly.

## Example Workflow: Understanding a Function

```
User: "What does the processRequest function do in server.go?"

1. Use hover to get the function signature and docs:
   lsp(operation="hover", file_path="/path/server.go", line=50, character=10)

2. If more detail needed, go to definition:
   lsp(operation="definition", file_path="/path/server.go", line=50, character=10)

3. Read the function code:
   read_file(path="/path/server.go", offset=45, limit=30)
```

## Example Workflow: Refactoring

```
User: "I want to rename getUserID to getUserIdentifier"

1. Find all references:
   lsp(operation="references", file_path="/path/user.go", line=20, character=5)

2. Review each location and update:
   - For each reference, use edit_file to make the change
   - Re-run references to confirm all updated
```

## Troubleshooting

- **"LSP server not found"**: The language server is not installed. Install it (e.g., `go install golang.org/x/tools/gopls@latest` for Go).
- **"client not initialized"**: The LSP server hasn't started. Try the operation again.
- **No results returned**: The symbol may not exist at that position. Verify the file path and position.
