# Zotigo Implementation Plan

This document tracks the implementation status and roadmap for Zotigo, a protocol-centric CLI agent written in Go.

Last updated: 2026-02-06 (aligned with branch `feat_init`)

---

## Product Goals

Zotigo is designed to be a practical local coding agent with:
- A provider-agnostic conversation protocol.
- Safe tool execution with user approval.
- Session persistence and resume.
- Extensible architecture for future web/API deployment.

Target deployment modes:
1. CLI mode (local terminal, local file operations)
2. Web UI mode (browser UI, sandbox execution)
3. API mode (stateless HTTP interface)

---

## Architecture Layers

### Transport Layer
Purpose: define how clients communicate with the agent.

Current:
- `ChannelTransport` implemented.

Planned:
- WebSocket transport
- HTTP/SSE transport

### Core Layer
Purpose: run the conversation loop and orchestrate tool calls.

Current:
- Protocol types implemented.
- Agent loop implemented (tool-call -> execute -> continue).
- Runner implemented with non-blocking approval flow.

### Environment Layer
Purpose: bind an executor and a state store into one runtime profile.

Current:
- Local environment implemented.
- Custom environment implemented.

Planned:
- E2B environment
- Docker environment

### Tools Layer
Purpose: expose filesystem, shell, search, code intelligence, and git capabilities.

Current:
- Filesystem tools, edit/patch, shell, grep/glob, git, and lsp tools implemented.

---

## Capability Matrix

| Capability | Zotigo Status | Notes |
|---|---|---|
| Transport abstraction | Implemented | Channel transport only |
| Executor abstraction | Implemented | Local executor in production path |
| State abstraction | Implemented | File store with session locks |
| Environment abstraction | Implemented | Local + custom |
| TUI | Implemented | Bubble Tea based |
| Filesystem tools | Implemented | Read/write/list/edit/patch |
| Shell tool | Implemented | Timeout + workdir support |
| Search tools (grep/glob) | Implemented | Uses `rg`/`fd` with fallback |
| LSP integration | Implemented (baseline) | Language server binary required in PATH |
| Sandbox safety | Implemented (policy guard) | Command/path checks, not OS-level isolation |
| Multi-provider | Partial | OpenAI + Anthropic implemented; Gemini pending |
| Loop detection | Implemented | Repeated-call protection |
| Context compression | Implemented | Partition + summary + tool-output truncation |
| Slash command framework | Partial | Framework implemented; TUI main input path not fully wired |
| Snapshot/rewind | Partial | Commands implemented; depends on `snap-commit` |
| MCP | Pending | Not implemented |
| Hooks | Pending | Not implemented |
| Sub-agents | Pending | Not implemented |
| Web tools | Pending | Not implemented |
| Skills | Partial | Base system implemented, deeper integration pending |
| Extensions | Pending | Not implemented |

---

## P0/P1 Status Sync

### 1) Shell Tool
Status: Implemented

Implemented scope:
- Command execution and output capture
- Timeout control
- Working directory support
- Stdout/stderr formatting

Gaps:
- Explicit env injection
- Interactive shell handling

### 2) Search Tools (grep/glob)
Status: Implemented

Implemented scope:
- Regex/text search with include/context/max-count controls
- File glob search with type/depth/count
- Preference for external fast tools (`rg`, `fd`) and fallback commands

### 3) Slash Commands
Status: Partially integrated

Implemented scope:
- Registry + parser + built-in command set
- Commands include help/clear/model/compress/stats/snapshot/rewind/skills

Gaps:
- Current TUI input path does not fully route through command registry
- `cost` command is still a placeholder

### 4) Snapshot/Rewind
Status: Implemented with external dependency

Implemented scope:
- `/snapshot`, `/rewind`, `/snapshots`
- Friendly error message when dependency is missing

Dependency:
- `snap-commit`

### 5) Edit/Patch
Status: Implemented (baseline)

Implemented scope:
- Exact string replacement with uniqueness checks
- `replace_all` support
- Baseline unified diff patch application

Gaps:
- Stronger conflict handling and atomic guarantees

### 6) LSP
Status: Implemented (baseline)

Implemented scope:
- Definition, references, hover, implementation
- Document/workspace symbols
- Diagnostics
- Language server manager and auto-selection by extension

Dependency:
- Language server binaries installed and available in PATH

### 7) Loop Detector
Status: Implemented

### 8) Context Compression
Status: Implemented

### 9) Sandbox
Status: Implemented (policy guard)

Current implementation:
- Command risk classification (blocked/high/normal)
- Path allowlist checks

Note:
- This is not an OS-level isolation sandbox (e.g., container/Landlock)

### 10) Providers
Implemented:
- OpenAI Chat Completions path
- Anthropic Messages path

Pending:
- Gemini provider
- Ollama provider
- OpenAI Responses API full implementation

---

## P2 Roadmap

### 11) MCP
Planned components:
- `core/mcp/types.go`
- `core/mcp/client.go`
- `core/mcp/server.go`
- `core/mcp/transport/{stdio,sse}.go`
- `core/mcp/manager.go`

### 12) Hooks
Planned components:
- Hook types, registry, runner
- Built-in git hooks

### 13) Sub-agent System
Planned components:
- Agent types, execution manager, registry
- Built-in exploration/planning sub-agents

### 14) Web Tools
Planned components:
- `web_fetch`
- `web_search`

---

## P3 Roadmap

### 15) IDE Integration
- Dedicated extension project
- Diagnostics + file change integration

### 16) Skills
Current:
- Base skill loading/activation/injection implemented

Planned:
- Better governance and conflict rules
- Deeper integration into command/tool workflows
- Better observability and debugging UX

### 17) Extensions
- Extension packaging format
- Install/uninstall/enable/disable lifecycle
- Extension-provided tools/hooks

---

## Implementation Phases

Phase 0 (P0): Core abstractions
- Status: Complete

Phase 1 (P0): Core tools
- Status: Complete with one integration gap (slash command routing in main TUI input path)

Phase 2 (P1): Enhanced capabilities
- Status: In progress
- Completed in this phase: edit/patch baseline, snapshots via command, LSP baseline, loop detection, compressor, policy sandbox, Anthropic provider

Phase 3 (P2): Advanced integrations
- Status: Pending

Phase 4 (P3): Ecosystem
- Status: Pending

---

## Practical Strengths Today

1. Clean layered design with stable internal abstractions.
2. Good local developer workflow via single binary and local session persistence.
3. Strong baseline tool set for coding tasks.
4. Built-in safety guard for risky operations.
5. Extensible provider and tool architecture.

---

## External Tool Philosophy

Zotigo should reuse proven external tools where possible and degrade gracefully.

Recommended optional dependencies:
- `rg` for text search
- `fd` for glob/file discovery
- `snap-commit` for snapshots
- Language servers (`gopls`, `typescript-language-server`, `pylsp`, `rust-analyzer`)

---

## Completed Refactors (Highlights)

1. Tool interface now executes through `Executor`.
2. Agent is executor-injected and provider-agnostic.
3. Runner supports non-blocking approval continuation.
4. Environment abstraction unifies executor + store pairing.

