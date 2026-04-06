# AGENTS.md — Guidelines for Agents (Zotigo)

This document guides any agent (AI coding agent, CLI assistant, etc.) working in this repository. If conflicts arise, follow system/developer/user instructions by priority.

## Scope
- Applies to the repo root and all subdirectories.
- Deeper AGENTS.md files override this one for their subtree.

## Goals and Principles
- Build a local CLI agent comparable to Claude Code: multi‑LLM, extensible tools, safe defaults, tool‑calling conversational loop.
- Keep changes minimal, focused, and reversible; touch only what the task requires.
- Fix root causes instead of adding superficial patches.
- Do not commit or create branches unless explicitly requested by the user.

## Run and Validate
- Build: `make build`
- Dev mode: `make dev`
- Tests (if available): `make test`, `make test-integration`, `make test-e2e`
- Run CLI: `go run ./cmd/zotigo` or use the built binary under `./build/zotigo`
- Debug: set `ZOTIGO_DEBUG=true` to enable verbose agent/provider logging.

## Configuration
- Runtime config lives in `~/.zotigo/config.yaml`; E2E tests use `e2e.config.json` (legacy `config.json` is still supported).
- Provider selection: explicit `provider` or auto-detect by available creds in order `claude` → `aws` → `openai`.
- Never hardcode secrets; follow `core/config/config.go` and provider loaders.

## Code Style and Structure
- Go 1.21+.
- Layout:
  - `cli/` presentation and interaction (Cobra/Viper)
  - `core/agent` conversation + tool-calling loop
  - `core/providers` LLM adapters (OpenAI/Claude/AWS, etc.)
  - `core/tools` tool registry and implementations
  - `core/prompts` prompt templates and engine
- Maintain public APIs/types; prefer backward compatible changes.
- Clear naming; avoid single-letter vars. Do not add license headers unless asked.

## Tools and Workflow (in this environment)
- Use `rg` for search; `rg --files` to list files; read files in chunks (≤250 lines).
- Modify files via the patch tool (`apply_patch`); avoid pasting large file blobs inline.
- Network is restricted by default; ask for approval for network/system‑wide writes/destructive actions.
- Use the plan tool for multi‑step or ambiguous work; skip for trivial tasks.
- Before non‑trivial commands, briefly state intent and effect.

## Security and Boundaries
- Restrict file operations to the workspace; respect safety checks in `core/tools` (e.g., `edit` confines to project paths).
- Do not generate/explain/modify content with malicious intent.
- Avoid destructive commands; if cleanup is needed, minimize scope and confirm first.

## Change Requirements
- Only modify files directly related to the task and keep style consistent.
- If behavior/contract changes, update related docs (README, usage, templates).
- If you notice unrelated issues, mention them in results but do not fix unless asked.

## Provider and Tool‑Calling Notes
- `core/agent/agent.go`: loop is “model tool_calls → execute → append to conversation”. `task_complete` signals finalization; `talk` carries user‑facing text.
- OpenAI‑compatible path: `core/providers/openai/openai_provider.go` injects tool schemas via `tools.CreateDefaultRegistry()`; `ToolChoice` is often `required` to encourage tool use.
- Prompt templates under `core/prompts/templates/`; keep changes concise and consistent with higher‑level system instructions.

## Common Task Guides
- Add a tool: implement under `core/tools`, register in `CreateDefaultRegistry`, provide complete JSON Schema, clear description, and project‑scoped safety.
- Add a provider: implement under `core/providers/<name>`, satisfy interfaces, handle timeouts/error categories/tool mapping/logging.
- Conversation logic: change `core/agent/agent.go` carefully; preserve `talk`/`task_complete` semantics and loop safeguards.

## Commit and Rollback
- Do not run `git commit` unless explicitly requested.
- Keep patches atomic and reversible; ensure deletions have no remaining references.

## Quick Troubleshooting
- Provider not selected: check `~/.zotigo/config.yaml`/env or set `provider` explicitly.
- Tools not firing: ensure registry registration, schema alignment with model output, and inspect debug logs.
- Empty output/looping: enable `ZOTIGO_DEBUG=true`, verify tool results are appended and `task_complete` is emitted.

## Testing
- **Make sure you can validate the feature every time you add it, and keep it simple.**

— This guide aims to keep agents consistent, safe, and effective in this repo.
