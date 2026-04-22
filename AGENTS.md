# AGENTS.md — Guidelines for Agents (Zotigo)

This document guides any agent (AI coding agent, CLI assistant, etc.) working in this repository. If conflicts arise, follow system / developer / user instructions by priority. Deeper `AGENTS.md` files override this one for their subtree.

## Project Overview
Zotigo is a Go CLI agent in the spirit of Claude Code: streaming TUI, multi‑provider LLM backend (OpenAI / Anthropic / Gemini), tool‑calling loop with safety classification, ACP server for editor integration. The codebase keeps the tool surface and `Executor` interface deliberately narrow — additions need a real reason.

## Quick Commands
Run these often; they are the canonical preflight before any change is "done".

```bash
make check                # gofmt + go vet + golangci-lint + go test ./... — what CI also runs
make test                 # unit + integration tests
make test-e2e             # E2E tests (need zotigo.e2e.yaml — see E2E_TESTING.md)
go test ./core/agent/... -count=1 -run TestAgent_<Name>   # focused test loop
gofmt -w <file>           # pre-commit hook will reject otherwise
make build                # produces ./build/zotigo
go run ./cmd/zotigo       # run the TUI from source
go run ./cmd/acp          # run the ACP JSON-RPC server
ZOTIGO_DEBUG=true go run ./cmd/zotigo   # verbose agent + provider event logging
```

There is a **pre-commit hook** (`.git/hooks/pre-commit`) that runs `gofmt -l` on staged `.go` files and rejects commits that need formatting. Don't bypass with `--no-verify`; just run `gofmt -w`.

## Tech Stack
- **Go 1.25+** (`go.mod`).
- **TUI**: `charm.land/bubbletea/v2` + lipgloss.
- **Config**: viper / YAML (`~/.zotigo/config.yaml`).
- **Provider SDKs**: `openai/openai-go/v3` (Chat + Responses APIs), `anthropic-sdk-go`, `google/generative-ai-go`.
- **Editor protocol**: ACP (Agent Client Protocol) over JSON‑RPC.

## Project Structure
Top‑level:
- `cmd/zotigo/` — TUI entrypoint, `cmd/acp/` — ACP server entrypoint
- `cli/` — TUI (`tui/`) and slash commands (`commands/`)
- `internal/app/` — wiring (provider + tools + middleware + skills + transport)
- `core/` — protocol‑first runtime
- `tests/e2e/` — provider end‑to‑end tests (build tag `e2e`)
- `docs/` — architecture notes (e.g. `extension-points.md`)

Inside `core/`:
- `agent/` — conversation loop, tool dispatch, safety classifier, approval flow; `agent/prompt/` builds system prompts and reminders
- `providers/{openai,anthropic,gemini}/` — LLM adapters; openai has `chat_provider.go` (Chat Completions) and `response_provider.go` (Responses API for gpt‑5 / o‑series)
- `tools/` — `tools.Tool` interface + `builtin/` implementations
- `middleware/` — agent‑level tool‑call middleware (read tracker, etc.)
- `runner/` — Agent + Transport orchestration with turn‑level listeners
- `transport/` — IO abstraction (channel, ACP, stdio)
- `executor/` — filesystem + shell abstraction; `LocalExecutor` (and ACP `RemoteExecutor` lives in `core/acp/`)
- `services/` — loop detector, context compressor, tokenizer
- `skills/` — skill discovery and prompt injection
- `protocol/` — internal message + event types
- `session/` — on‑disk session persistence
- `lsp/` — language server integration
- `acp/` — ACP server bindings
- `config/` — schema, loader, defaults

## Tool Surface (mirrors Claude Code)
- **File**: `read_file`, `write_file`, `edit` (precise string replacement, supports `replace_all`).
- **Search**: `grep`, `glob`.
- **Shell**: `shell` with a read‑only command whitelist baked into `ShellTool` policy. Directory creation, deletion, listing — all go through Shell, not dedicated tools.
- **Web**: `web_search` (Tavily), `web_fetch`.
- **LSP**: `lsp` (definition / references / diagnostics).
- Registration site: `internal/app/app.go` via `ag.RegisterTool(...)`. There is no `CreateDefaultRegistry` — the agent gets exactly the tools its host wires in.
- The `Executor` interface is intentionally narrow: `ReadFile / WriteFile / Stat / Exec` plus `WorkDir / Platform / Init / Close`. New filesystem capabilities should come through Shell unless they need structured returns.

## Extension Points
Two distinct mechanisms (don't conflate them — see `docs/extension-points.md`):

**`agent.Middleware`** — HTTP‑handler‑style chain wrapping every tool call. Can rewrite arguments, short‑circuit, or replace results.

```go
// Block writes outside the working directory:
ag, _ := agent.New(profile, exec,
    agent.WithMiddleware(func(next agent.Next) agent.Next {
        return func(ctx context.Context, c *agent.ToolCall) (any, error) {
            if c.Name == "write_file" && !inWorkDir(c.Arguments) {
                return nil, fmt.Errorf("refusing write outside workdir")
            }
            return next(ctx, c)
        }
    }),
)
```

**`runner.Listeners`** — observational callbacks at turn‑level milestones. Return values ignored, panics caught and forwarded to `OnError`.

```go
r := runner.New(ag, tr, runner.WithListeners(runner.Listeners{
    AfterTurn: func(snap agent.Snapshot) { saveSession(snap) },
    OnError:   func(err error)          { logf("runner: %v", err) },
}))
```

Rule of thumb: **change what a tool does → Middleware. Record that something happened → Listener.**

## Safety and Approval
- Default approval policy is **Auto**. Each tool's `Classify(call)` returns a `SafetyDecision` with a level (`Safe / Low / Medium / High / Blocked`); calls at or above `safety.classifier.review_threshold` (default `medium`) get routed to the LLM safety classifier.
- The classifier runs on a separate (typically smaller) profile (`safety.classifier.profile`). Per‑attempt timeout 20s with one retry — see `core/agent/provider_classifier.go`.
- File path safety helpers (`IsInWorkDir`, `IsSensitivePath`) live in `core/tools/scope.go`; every mutator's `Classify` should use them.
- Toggle Auto vs Manual at runtime with `Shift+Tab` in the TUI.

## Provider Notes
- `core/providers/openai/factory.go` auto‑routes models whose IDs start with `gpt-5` / `o1` / `o3` / `o4` to the Responses API; everything else goes through Chat Completions.
- For stateless multi‑turn reasoning continuity on Responses API, the provider sets `Include: [reasoning.encrypted_content]` and echoes per‑turn `ReasoningID` + `EncryptedContent` back as a `ResponseReasoningItemParam` next turn.
- `reasoning_content` from llama.cpp / DeepSeek / OpenRouter is surfaced through the Chat Completions path so the agent sees thinking blocks regardless of provider.

## Loop Detection
- `core/services/loop_detector.go` flags identical **consecutive** tool calls. Defaults: warn at 5 in a row, window 15. Iterative workflows (`go test → edit → go test → edit → go test`) intentionally do NOT trigger because any intervening different call resets the streak.
- When triggered, the warning is **prefixed** into the next tool result text wrapped in `<system-reminder>`. It is NOT a separate `ToolResult` (OpenAI Chat Completions rejects duplicate `tool_call_id` in one tool‑role message).

## Code Style
- Format with `gofmt -w`; no goimports group reordering needed (the pre-commit hook only checks gofmt).
- Avoid single‑letter vars (except `i`, `r`, `w`, `t *testing.T` etc.).
- **Comments explain why, not what**: don't restate the type signature; do explain non‑obvious tradeoffs and references to other files.
- Don't add license headers unless asked.
- Errors: wrap with `fmt.Errorf("...: %w", err)` to preserve the chain. Return errors; only log inside long‑running goroutines.
- Strings shown to the LLM (system prompt, reminders, error messages from tools) are part of the agent contract — treat them like API surface, not casual text.

## Testing
- After touching `core/agent/...` or `core/runner/...`, run **both**: `go test ./core/agent/... ./core/runner/... -count=1`.
- After touching a provider, run that provider's package + `./tests/e2e/... -tags=e2e` if you have a config (see `E2E_TESTING.md`). E2E is opt‑in; don't add new tests that require live API keys to the default test path.
- After touching a tool or middleware, run `go test ./core/tools/... ./core/middleware/... -count=1`.
- Prefer a `LocalExecutor` against `t.TempDir()` over mocking the executor — the read‑tracker middleware needs real `Stat` results.
- The full preflight is `make check`.

## Commit and PR Conventions
- Commit subject: `topic: short imperative summary`. `topic` is lowercase, usually a package or short area (`agent:`, `loop-detector:`, `tools:`, `docs:`, `openai-response:`). Keep under ~70 chars.
- Body: explain **why**, not what (the diff shows what). Wrap at ~72 chars. Reference symptoms / incidents when applicable.
- One logical change per commit. Squash WIP commits before pushing.
- Branch names: `<kind>/<slug>` — `feat/`, `fix/`, `refactor/`, `docs/`, `chore/`. Examples: `feat/openai-reasoning-content`, `refactor/rename-hooks`.
- PRs go to `master`. Title mirrors the commit subject. Body: Summary, Test plan, optional Notes / Dependencies.
- Trailer: every commit ends with `Co-Authored-By: Claude ...` when an agent contributed.

## Boundaries

**Always do**
- Run `go build ./... && go test ./core/...` before claiming a change is done on Go code.
- `git mv` when relocating files (preserves history); never delete‑and‑add.
- `gofmt -w` modified Go files before committing.
- Use the dedicated tools (`read_file`, `edit`, `write_file`, `grep`, `glob`) over Bash for code edits.
- Add concrete tests for new behavior; reproduce a bug in a test before fixing.

**Ask first**
- Adding a new builtin tool (the surface is intentionally aligned with Claude Code).
- Adding methods to the `Executor` interface (kept narrow on purpose).
- Renaming exported types in `core/agent` or `core/runner` (downstream impact).
- Touching the safety classifier prompt or `record_decision` schema.
- Direct commits to `master` instead of feature branches + PRs.

**Never do**
- `git commit` without an explicit user request.
- `git push --force` (or `--force-with-lease`) to `master` / `main`.
- `--no-verify` to skip the gofmt pre‑commit hook — fix the formatting instead.
- Commit `.env`, API keys, anything from `~/.zotigo/`, or `e2e.config.json` with real keys.
- Run `git reset --hard` or `rm -rf` outside a `t.TempDir()`/`./build/` without explicit confirmation.
- Add features, refactors, or abstractions beyond what the task asked for; flag drive‑by ideas in the response, don't smuggle them into the diff.

## Common Task Guides
- **Add a tool**: implement under `core/tools/builtin/`, satisfy `tools.Tool` (`Name / Description / Schema / Classify / Execute`), register in `internal/app/app.go`. Provide a complete JSON Schema and a `Classify` that scopes the call.
- **Add a provider**: implement under `core/providers/<name>/`, satisfy `providers.Provider`, register the factory in `init()`. Map provider events into `protocol.Event` (text deltas → `ContentDelta`, tool calls → `ToolCallDelta` + `ToolCallEnd`, finish → `NewFinishEvent`).
- **Add a middleware**: write `func(next agent.Next) agent.Next`. Place under `core/middleware/`, register via `agent.WithMiddleware(...)`.
- **Add a runner listener**: extend `runner.Listeners` only if you genuinely need a new turn‑level milestone; otherwise just provide one of the existing four callbacks at construction.
- **Conversation logic**: change `core/agent/agent.go` carefully. Loop = provider stream → assemble assistant message → execute tool calls (with classifier + middleware) → append tool results → repeat until `FinishReason == stop` or `need_approval`.

## Quick Troubleshooting
- Provider not selected: check `~/.zotigo/config.yaml` `default_profile` and the profile's `provider / model / api_key`.
- Tools not firing: confirm registration in `internal/app/app.go`; `ZOTIGO_DEBUG=true` prints stream events showing whether the model emitted tool calls.
- Classifier silent‑failing: look for `provider classifier timeout` in debug logs. Default is 20s; lower `safety.classifier.timeout_ms` only after confirming the underlying model is fast enough.
- Empty output / loop: `ZOTIGO_DEBUG=true`; check for the loop‑guard `<system-reminder>` in the latest tool result. The runner ends a turn on `FinishReason == stop`.
- gofmt pre‑commit failure: run `gofmt -w` on the listed files, re‑stage, retry the commit.
