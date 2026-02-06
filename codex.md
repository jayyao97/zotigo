# Codex CLI Source Analysis (English Notes)

This document summarizes implementation patterns observed in the Codex CLI codebase (`codex-rs`) and maps them to Zotigo-relevant design lessons.

Scope: architecture and engineering patterns, not feature parity guarantees.

---

## 1) Project Overview

Codex CLI is a Rust-based AI coding assistant with:
- Multi-crate architecture (large workspace)
- Interactive TUI
- Rich tool runtime
- Strong execution policy and sandbox controls
- MCP integration

Key takeaway for Zotigo:
- Keep protocol and tool orchestration decoupled from UI and provider layers.

---

## 2) CLI Entry and Commands

Observed pattern:
- Top-level command router with explicit subcommands
- Separate interactive and non-interactive flows
- Operational subcommands for login, review, sandbox, mcp, and resume

Design lesson:
- Keep command entry explicit and composable.
- Separate orchestration commands from runtime commands.

---

## 3) TUI Architecture

Observed pattern:
- Large dedicated UI modules for chat, history, markdown, diffs, and overlays
- Clear separation between presentation widgets and runtime state transitions
- Incremental transition to newer TUI architecture without hard-breaking old paths

Design lesson:
- UI complexity grows quickly; isolate rendering, input handling, and transcript state.
- Keep a migration-friendly architecture for future UI versions.

---

## 4) Core Engine

Observed responsibilities:
- Conversation loop and event lifecycle
- Tool call orchestration
- Message history handling
- Execution policy enforcement points
- Error normalization

Design lesson:
- Preserve a strict runtime event model.
- Keep tool orchestration deterministic and testable.

---

## 5) Tool System

Observed pattern:
- Tool registry + routing + orchestration layer
- Dedicated handlers per tool capability
- Support for both local and extended runtimes

Design lesson:
- Use explicit tool schemas and stable contracts.
- Keep tool execution policy-aware.

---

## 6) Security and Sandbox Model

Observed pattern:
- Platform-specific sandbox implementations
- Policy engine for execution approval/denial
- Rules for command classes and filesystem boundaries

Design lesson:
- Security should be layered:
  1. policy checks (command/path risk)
  2. execution constraints (timeout/resources)
  3. platform isolation when available

---

## 7) MCP Integration

Observed pattern:
- Dedicated protocol types and connection management
- Tool/resource abstractions exposed through MCP
- Multi-server capability with routing

Design lesson:
- Treat MCP as a first-class integration surface, not a plugin afterthought.

---

## 8) API and Protocol Surface

Observed pattern:
- Strongly typed protocol messages and events
- Serialization contracts that support client code generation and stable APIs

Design lesson:
- Keep protocol structures minimal, explicit, and forward-compatible.

---

## 9) Session and State Management

Observed pattern:
- Session persistence and resume support
- Conversation item model with ordered replay semantics

Design lesson:
- Persist enough to restore execution context, approvals, and transcript continuity.

---

## 10) Quality and Tooling

Observed pattern:
- Strong linting and style constraints
- Snapshot and integration test coverage in UI and runtime layers

Design lesson:
- Add tests around behavior boundaries (tool events, approval edges, resume behavior).

---

## 11) Performance and Build

Observed pattern:
- Production profile tuning and binary optimization

Design lesson:
- Keep build profile explicit and validate startup/runtime costs for TUI workflows.

---

## 12) What Zotigo Can Reuse Conceptually

1. Event-driven engine boundaries.
2. Registry-based tool runtime.
3. Layered safety model.
4. Resume-safe session architecture.
5. MCP-ready connection manager architecture.

---

## 13) What Zotigo Should Keep Different

1. Simpler Go-centric runtime and deployment story.
2. Clear executor abstraction for local/cloud portability.
3. Progressive feature rollout with stable CLI UX.

---

## 14) Actionable Checklist for Zotigo

Short term:
- Finish slash command routing in main TUI path.
- Complete Gemini provider.
- Harden patch conflict handling.

Mid term:
- Add MCP client/server manager.
- Add hooks lifecycle.
- Add E2B and Docker environments.

Long term:
- Web transport and API mode.
- Extension ecosystem and IDE companion.

