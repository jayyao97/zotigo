# Gemini CLI Source Analysis (English Notes)

This document summarizes major architectural patterns from Gemini CLI and highlights practical lessons applicable to Zotigo.

Scope: engineering patterns and product architecture, not strict 1:1 implementation mapping.

---

## 1) High-Level Product Shape

Gemini CLI combines:
- Interactive terminal UX
- Tool-rich execution runtime
- Service-level orchestration (compression, loop handling, hooks)
- IDE-aware workflows

Design lesson for Zotigo:
- Keep core runtime services explicit and independently testable.

---

## 2) Command Experience

Observed pattern:
- Slash command driven interaction model
- Distinct command lifecycle and command registry behavior

Design lesson:
- Command framework should be first-class and wired directly into primary input flow.

---

## 3) Tooling Stack

Observed pattern:
- Strong shell/fs/search/edit capabilities
- Structured schemas for tool invocation
- Explicit control around risky operations

Design lesson:
- Keep tools contract-driven and policy-aware.
- Ensure fallback behavior when external binaries are unavailable.

---

## 4) Service Layer Patterns

Observed services:
- Loop detection
- Context compression
- Token accounting
- Output shaping and truncation

Design lesson:
- Treat runtime services as shared infrastructure, not UI helpers.

---

## 5) LSP and IDE Integration

Observed pattern:
- IDE-adjacent capabilities exposed as runtime functions
- Diagnostics and symbol intelligence treated as tooling primitives

Design lesson:
- LSP integration should be integrated into tool orchestration, not bolted on.

---

## 6) Hooks and Lifecycle

Observed pattern:
- Event-driven hook system around major runtime checkpoints

Design lesson:
- A hook bus enables extension without modifying core loop logic.

---

## 7) MCP and External Integrations

Observed pattern:
- MCP support for external tools/resources
- Structured connection management

Design lesson:
- Build integration boundaries early to avoid protocol rewrites later.

---

## 8) Agentic Expansion

Observed pattern:
- Sub-agent or delegated task execution capabilities

Design lesson:
- Keep delegated execution bounded (max turns, scoped tools, explicit summaries).

---

## 9) Robustness Practices

Observed pattern:
- Safety checks for execution paths
- Runtime observability and state tracing

Design lesson:
- Include debuggable checkpoints for tool decisions and compression outcomes.

---

## 10) Recommended Mapping to Zotigo

Already aligned in Zotigo:
- Core loop abstraction
- Tool registry and execution model
- Loop detector and context compressor foundations

Needs completion:
- Full command routing in primary TUI flow
- Gemini provider
- MCP and hooks
- Web transport layer

---

## 11) Practical Roadmap Guidance

P0:
- Close command integration gap.
- Stabilize tokenizer/test strategy for offline environments.

P1:
- Provider completeness (Gemini path).
- Better patch conflict handling and atomic edit semantics.

P2:
- MCP manager + hooks.
- Cloud/runtime environments (E2B, Docker).

P3:
- Extension ecosystem and IDE companion.

---

## 12) Conclusion

Gemini CLI demonstrates that long-term maintainability comes from:
1. clear boundaries between UI, runtime services, and tool orchestration,
2. explicit safety and policy control,
3. extension-ready integration surfaces.

These principles map well to Zotigo's current architecture and should guide subsequent phases.

