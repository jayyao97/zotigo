# Architecture

This document explains how Zotigo is put together and why each part exists.
It is the place to look before adding a package or moving code across one of
the boundaries below. Related documents:

- `docs/zotigod-api.md`: the daemon's public HTTP contract
- `docs/terminal-and-acp-clients.md`: how the TUI and ACP attach to the daemon
- `docs/extension-points.md`: `agent.Middleware` vs `runner.Listeners`
- `AGENTS.md`: working rules, commands and conventions

## One sentence

Zotigo is a session service (`zotigod`) that runs agent turns in per-session
worker processes, plus thin clients (TUI, ACP, Desktop, Web) that all speak
the same HTTP API. The agent itself is a library under `core/` that
knows nothing about the service.

## Processes

```
 zotigo (TUI)       zotigo-acp        Desktop / Web
      │                 │                  │
      └─────────────────┴──── HTTP + SSE ──┘
                               │  public API
                    ┌──────────▼───────────┐  session registry, catalog,
                    │ zotigod (daemon)     │  durable display log, input queue,
                    │  internal/zotigod    │  approvals, worker supervision,
                    └──────────┬───────────┘  IM channel connections
              launches one process per live session
           ┌───────────────────┴────────────────────┐
┌──────────▼───────────┐                 ┌──────────▼───────────┐
│ native worker        │                 │ Codex worker         │
│  core/agent loop     │                 │  bridge to codex     │
│  tools, provider     │                 │  app-server          │
└──────────────────────┘                 └──────────────────────┘
   each worker dials back to the daemon on /internal/* (WebSocket + HTTP)
```

Why this shape:

- **A turn must outlive its client.** Closing the TUI or an editor must not
  kill a running turn, and a second client must be able to attach. So turns run
  in the service, never in a client.
- **A worker crash must not take down other sessions.** Each live session gets
  its own process. The daemon holds only state it can rebuild from disk; the
  worker holds the agent and is disposable. Recovery replays durable commands
  to a fresh worker.
- **Clients must not drift apart.** All of them read the same durable display
  items and send the same commands, so there is one implementation of
  approvals, steering, queueing and history.

The embedded mode (`zotigo --no-daemon --state-dir <dir>`) runs the same
handler and worker launcher inside the client process on a private port. It is
a deployment variant, not a second implementation.

## Layers

```
cmd/            entrypoints only
cli/            TUI and slash commands               ── clients
internal/       the product: daemon, clients, wiring ── product
core/           embeddable agent library             ── library
```

`core/` never imports `internal/`, `cli/` or `cmd/`. Everything else in this
section is a finer cut of that one rule.

### core: the library

From the bottom up. A package may import packages above it in this list, never
below.

| Level | Packages | What it is | Why it is separate |
| --- | --- | --- | --- |
| Foundation | `protocol`, `config`, `executor`, `debug`, `skills`, `lsp`, `catalogschema`, `acp`, `agent/prompt`, `agent/loopguard` | Data types and self-contained mechanisms. Each imports no other Zotigo package. | They are the vocabulary. If `protocol` or `config` could reach upward, every package would transitively depend on the agent. |
| Capabilities | `tools`, `tools/builtin`, `providers/*`, `compaction`, `transport`, `observability/*`, `textassist`, `workspace` | Things an agent uses: the tool contract and leaf tools, model adapters, context compaction, the IO interface, tracing, one-shot model calls, the workspace catalog. | They are used by the agent and must not know about it. A tool that imports its caller cannot be tested or reused without the whole agent. |
| Kernel | `agent` | The turn loop: stream from the provider, classify and run tool calls, build and compact context, keep the audit trail. | The one place where conversation semantics live. It does not know how it is hosted, driven or persisted. |
| Around the kernel | `runner`, `subagent`, `middleware`, `session`, `acp/embed` | Things built from an agent: drive it over a transport, run a child agent as a tool, standard middleware, persist its snapshot, plug it into an ACP connection. | Each needs the agent's types, so each sits above it. Keeping them out of `agent` keeps the kernel free of transport, storage and recursion. |

Notes on specific packages:

- **`tools` vs `tools/builtin` vs `subagent`.** `tools` is the contract: the
  `Tool` interface, safety levels, and the per-call context values
  (`EventSink`, `CallInfo`) the agent attaches for a tool to read. `builtin`
  holds leaf tools. The spawn tool creates a child agent, so it cannot be a
  leaf; it lives in `subagent`.
- **`session` sits above `agent` on purpose.** A persisted session embeds
  `agent.Snapshot`. Persistence depending on the domain model is the normal
  direction; the reverse would be the problem, and the layering check forbids
  it. An earlier plan to hide this behind type aliases was dropped: it would
  have added indirection without removing a real dependency.
- **`executor` is the environment seam.** Every tool receives an
  `executor.Executor` (`ReadFile / WriteFile / Stat / Exec`). Running tools
  somewhere else (a container, a remote sandbox) means implementing that
  interface, not changing tools.
- **`acp` is protocol only.** JSON-RPC server and ACP types, no other Zotigo
  import. `acp/embed` holds the adapters that connect an ACP connection
  directly to an in-process agent.

### internal: the product

| Package | What it is | Why it is separate |
| --- | --- | --- |
| `zotigod` | The daemon: HTTP handlers, the session and worker registries, worker supervision, the input queue, approvals, channels dispatch, the Codex history syncer, and the entry point that dispatches a process to the daemon or to one of the worker packages. | See Processes. |
| `zotigod/wire` | The daemon↔worker protocol as data: the WebSocket frame and its payloads, the commands delivered to workers, and the bodies and headers of the worker's `/internal/*` calls. It imports only `core`. | The two sides are different processes and can be different builds during an update, so what they exchange should be a compile-time fact with a pinned format (`wire_test.go`). It is the first step of separating daemon and worker. |
| `zotigod/sessionlog` | The session display log as both processes use it: the item-source interfaces and their store-backed implementation (`ItemSource`, `StoredItemSource`), the eventing wrappers and the live `EventBroker` that carries previews next to the durable log, the durable log writer (`Log`: turns, blocks, tool calls, subagents, steering, approvals), approval requests, their validation and their conversion to the wire form (`ApprovalRequest`, `PublicApprovalRequest`), session-input acceptance and deduplication with its sentinel errors (`AppendAcceptedInput`, `FindExistingInput`, `ValidateCompletionSteering`, `ErrActiveTurn`, `ErrCommandIDConflict`), interaction requests and answer validation (`NewUserInputInteraction`, `ValidateInteractionAnswers`), the conversion of command items back into `wire.Command` values and user messages (`CommandFromItem`, `UserMessageFromCommand`), the item helpers the writer needs (`MessageItem`, `MessageImage`, `LastOpenTurn`), message-image blob storage and validation with its limits (`StoreMessageImageBlobs`, `CleanupMessageImageBlobs`, `ValidateMessageImageData`, `IsAllowedMessageImageMimeType`, `MessageImageRefs`, `PublicImageURLFromBlobPath`, `MaxMessageImageBytes`), and ID generation. It imports `core` and `zotigod/wire`, never `zotigod`. | The daemon and the worker processes both run this code. It sits below the daemon and the workers so that a worker package does not need the daemon. |
| `zotigod/workerbase` | What every worker process is built on: the daemon connection (`Config`, `ConnectURL`, `NewHTTPClient`, `AcquireSessionLock`, `EnsureSession`, `ReportReady`, `ReportFinish`, `IsExpectedClose` and the timing constants), the queued WebSocket writer and display barrier (`Writer`, `DisplayBarrierClient`), command fetch and cursor recovery (`CommandCursor`, `FetchCommands`, `LoadCommandCursor`, `RecoverCommandCursor`, `LatestCommandSequence`), turn hooks (`HookDispatcher`, `DispatchTurnStartHook`, `DispatchTurnEndHook`, `TurnUsageDelta`), the session tool specs and the runtime-tool client that carries tool calls to the daemon (`SessionToolSpecs`, `SessionRuntimeTool`, `RuntimeToolClient`, `ChannelReadMessagesTool`), approval-policy and profile persistence (`PersistSessionApprovalPolicy`, `PersistSessionProfile`, `LowersPermissions`), and the sentinels and reason strings both sides compare (`ErrRuntimeOccupied`, `ErrSessionNotFound`, `ProfileStateUncertainError`, `ControlChannelClosedReason`). It imports `core`, `internal/hooks`, `internal/channels`, `zotigod/wire` and `zotigod/sessionlog`, never `zotigod`. | The native worker and the Codex bridge worker both run this code, and the daemon shares a few pieces of it (timeouts, the cursor scan, policy persistence, the hook dispatcher type). It sits below both workers so that each is a package of its own. |
| `zotigod/worker` | The native worker process, one per session: it connects to the daemon through `workerbase`, builds the agent runtime, applies the commands the daemon delivers, runs turns and writes the session log. The daemon binary enters it through `worker.Run`; `Runtime` is the per-session runtime. Its blank imports register the model providers. It imports `core`, `internal/wiring`, `zotigod/wire`, `zotigod/sessionlog` and `zotigod/workerbase`, never `zotigod`. | The worker is a separate process and a client of the daemon. As a package it cannot reach daemon state, and its tests run without the daemon's fixtures. |
| `zotigod/codexworker` | The Codex bridge worker process, one per Codex-backed session: it connects to the daemon through `workerbase` and to a Codex app server, and translates Codex threads, turns and items into the session log. The daemon binary enters it through `codexworker.Run` with a `codexworker.Config`; `Runtime` is the per-session bridge. It also holds the Codex thread reading and conversion the daemon uses for history sync and fork (`ReadThreadHistory`, `ListThreadActivity`, `SyncCompletedTurns`, `ForkConversation`, `ErrHistoryAPIUnsupported` and the thread data types `ThreadSnapshot`, `Thread`, `Turn`, `ThreadItem`). It imports `core`, `internal/codexapp`, `internal/hooks`, `internal/channels`, `internal/wiring`, `zotigod/wire`, `zotigod/sessionlog` and `zotigod/workerbase`, never `zotigod` or `zotigod/worker`. | The bridge is a separate process and a client of the daemon, and it shares no code with the native worker beyond `workerbase`. As a package it cannot reach daemon state, and its tests run without the daemon's fixtures. |
| `zotigodclient` | Go client for the public API plus daemon discovery and start. | The TUI and ACP need exactly the same attach, resubmit and stop logic. |
| `wiring` | Builds an agent the way the product wants it: prompt builders, tool set, observer, classifier. | One place decides "what a Zotigo agent is" so the worker and any future host do not diverge. |
| `sessionadapter` | Maps `agent.Snapshot` to and from `session.Session`. | Keeps persistence mapping out of both the agent and the store. |
| `cliapp`, `acpserver` | The two client applications. Both are daemon clients. | Entry wiring for `cmd/zotigo` and `cmd/acp`. |
| `channels`, `channels/feishu` | IM channel connections that create and drive sessions. | Long-lived network connections owned by the daemon. |
| `codexapp`, `runtime` | Client for the Codex app-server and the runtime identity types shared with it. | The Codex worker is a bridge, not an agent loop. |
| `hooks` | User-configured command hooks as agent middleware. | Product policy on top of the generic middleware seam. |
| `environment` | `Executor` + `session.Store` chosen together. Reserved; see below. | Composition concept, so it lives beside `wiring`. |
| `update`, `diagnostics`, `buildinfo` | Self-update, crash and log capture, version constant. | Operational concerns of the binaries. |
| `testutil/*` | Test fixtures. Importable only from tests. | Enforced by the layering check. |

`internal/zotigod` is one large package. Files are named by area so the
package reads as groups:

- `daemon_run.go`, `app.go` (handler and route table), `api_response.go`, `auth.go`
- `session_*`: registry, handlers, activation, decoration, catalog, fork, tools
- `control_handlers.go`, `pause_steering_handlers.go`, `command_items.go`,
  `input_queue.go`, `approval*`, `interaction_handlers.go`, `codex_request_expiry.go`: the command surface
- `worker_attach_handlers.go`, `worker_registry.go`, `worker_process.go`,
  `worker_display_log.go`: the daemon's side of the worker link
- `codex_session_syncer.go`, `codex_input_images.go`,
  `codex_request_expiry.go`, `codex_settings_handler.go`: the daemon's own
  Codex code (history sync under the session registry and operation locks,
  and handlers)
- `worker_client_daemon_test.go`, `codex_worker_client_daemon_test.go`,
  `codex_session_syncer_daemon_test.go`: daemon tests that drive the native
  worker (`internal/zotigod/worker`) or the Codex bridge
  (`internal/zotigod/codexworker`) against the real handler
- `channel_*`, `catalog_handlers.go`, `file_*`, `*_handler.go`: feature APIs

### cli

`cli/tui` renders durable display items and sends commands through
`zotigodclient`. It never constructs an agent. `cli/commands` is the slash
command registry.

## Enforcement

`make check-layering` (also a CI step) runs `scripts/check_layering`, which
scans the imports of every production Go file against a small rule table:

1. `core` must not import `internal`, `cli` or `cmd`.
2. Foundation packages import no other Zotigo package.
3. Capability packages must not import `agent`, `session`, `runner`,
   `subagent` or `middleware`.
4. `agent` must not import `session`, `runner`, `subagent`, `middleware`,
   `transport`, `workspace`, `skills` or `acp`.
5. `cli` must not import `agent`, `internal/zotigod` or `internal/wiring`.
6. `internal/testutil` is importable only from tests.
7. `internal/zotigod/wire` must not import `internal`, `cli` or `cmd`.
8. `internal/zotigod/sessionlog` must not import `internal/zotigod`,
   `internal/zotigodclient`, `internal/cliapp`, `internal/acpserver`, `cli` or
   `cmd`; `internal/zotigod/wire` is the one allowed exception under
   `internal/zotigod`.
9. `internal/zotigod/workerbase` must not import `internal/zotigod`,
   `internal/zotigodclient`, `internal/cliapp`, `internal/acpserver`, `cli` or
   `cmd`; `internal/zotigod/wire` and `internal/zotigod/sessionlog` are the
   allowed exceptions under `internal/zotigod`.
10. `internal/zotigod/worker` must not import `internal/zotigod`,
    `internal/zotigodclient`, `internal/cliapp`, `internal/acpserver`, `cli` or
    `cmd`; `internal/zotigod/wire`, `internal/zotigod/sessionlog` and
    `internal/zotigod/workerbase` are the allowed exceptions under
    `internal/zotigod`. Rules 7 to 9 keep those three from importing
    `internal/zotigod/worker`.
11. `internal/zotigod/codexworker` must not import `internal/zotigod`,
    `internal/zotigodclient`, `internal/cliapp`, `internal/acpserver`, `cli` or
    `cmd`, with the same three exceptions as rule 10. Because
    `internal/zotigod/worker` and `internal/zotigod/codexworker` are both
    under `internal/zotigod` and neither is an exception, rules 10 and 11
    keep the two workers from importing each other, and rules 7 to 9 keep
    `wire`, `sessionlog` and `workerbase` from importing either.

Tests are exempt: a test may import upward to build a fixture. When adding a
package, decide its level and add it to the table.

## Context injection

Text the model sees beyond the conversation has one concept and two injection
points, both in `core/agent/prompt`:

- **Turn boundary: `UserContextBuilder`** (in use). Produces a separate
  contextual user message (environment, project instructions, request
  context). It is persisted, diffed between turns, and understood by
  compaction.
- **Mid-turn: `ReminderBuilder`** (reserved, no provider registered). Appends a
  `<system-reminder>` to the last tool result of a batch.

Four places still hand-roll a reminder instead of going through
`ReminderBuilder`: the loop warning (prefixed to a tool result), the
empty-response retry prompt, `request_context`, and the worker's tool-recovery
notice. They are listed as debt, not migrated, because moving them changes
where the text appears to the model.

## Reserved abstractions

Kept on purpose although the product does not use them today. Do not delete
them as dead code; do not design new features around their current shape
without reading the note.

| What | Why it is kept | Note |
| --- | --- | --- |
| `acp/embed` (`Transport`, `RemoteExecutor`) | Library users who run an agent in-process with an editor on the other end. | `zotigo-acp` does not use it; tools run in the session runtime. |
| `internal/environment` | Seam for non-local execution environments. | Its interface pairs `Executor()` with `Store()`. Sessions are now stored by the daemon, so a remote environment will likely need only the executor half. Decide when the first non-local environment is built. |
| `prompt.ReminderBuilder`, `agent.WithReminder` | The general mid-turn reminder entry. | See Context injection. |
| `transport.ChannelTransport`, `runner.Start/Stop` | In-process hosting of an agent behind a channel pair. | Used by tests and by library users; the worker uses `RunFullTurnStarted` with its own transport. |
| `middleware.ToolSpan` | Tool-call tracing spans. | Not registered by `wiring`, so production traces have no per-tool spans. |
| `providers.WithToolChoiceRequired` | Forcing a tool call. | No caller yet. |
| `config.Security`, `config.UI.Theme` | Config schema. | Parsed and preserved; not read. |

## Compatibility surfaces

Changing any of these needs a migration or a version check, not a refactor:

- On-disk state under the state directory: session files, display log,
  catalog SQLite schema (`core/catalogschema`, see `docs/catalog-migrations.md`),
  worker command cursors and WAL.
- Config schema (`~/.zotigo/config.yaml`, project `zotigo.yaml`).
- Public HTTP API and SSE events (`docs/zotigod-api.md`), and the `/internal/*`
  routes between daemon and workers.
- CLI flags and environment variables.
- Every string the model reads: system prompts, tool descriptions and schemas,
  reminders, tool result formats.

## Tests

- Unit tests sit next to the code. Agent and tool tests use `LocalExecutor` on
  `t.TempDir()` and a mock provider registered through `providers.Register`.
- `internal/zotigod` tests drive the real handler through `httptest` with a
  fake worker launcher, or run a real worker runtime against a mock provider.
  `testmain_test.go` isolates HOME and the state directory.
- `tests/e2e` (build tag `e2e`) calls real providers and needs
  `zotigo.e2e.yaml`.
- A test should assert behavior through the API production uses. Tests whose
  only subject is an accessor nothing calls, or that re-implement the logic
  they check, are removed rather than kept green.
- Fixtures that must be long should not be `strings.Repeat` of one rune or
  phrase: the tokenizer is quadratic on such input (see Known debt).

## Known debt

- `internal/zotigod` is the daemon only, but it is still one flat package of
  about 14,800 non-test lines in 63 files. Its largest files are
  `worker_registry.go` (923 lines), `catalog_handlers.go` (822),
  `session_tools.go` (714), `input_queue.go` (611) and `channel_dispatch.go`
  (610). Nothing inside it is enforced: any handler can reach any registry.
- Some names are exported only for tests in another package, because daemon
  tests in `internal/zotigod` drive a real worker runtime in-process against
  the real handler: `worker.NewRuntimeForTest`, `worker.ReadMessages`,
  `worker.ReplayCommands`; `workerbase.NewUnstartedWriter`,
  `(*Writer).Queue`, `(*Writer).Run`, `workerbase.DeltaBufferSize`,
  `workerbase.RecoverAppliedCommandSequence`;
  `sessionlog.(*EventBroker).SubscriberCount`, `(*Log).HasToolCallWaiter`,
  `(*Log).AppendItem`; `codexworker.NewRuntimeForTest` with
  `codexworker.RuntimeForTest`, `(*Runtime).ActiveDisplayItemCountForTest`,
  the type name `codexworker.Runtime`, and the Codex thread data types that
  only test fixtures name (`ThreadItem`, `ThreadItemEntry`, `ThreadItemList`,
  `ThreadList`, `TurnList`). None has a production caller outside its own
  package. Three `codexworker.Runtime` methods are used in production inside
  the package and are exported only because tests in another package call
  them: `HandleNotification`, `PersistSteering` and `Append`.
- `agent.RunMessage` in `turn_loop.go` is one function of about 600 lines.
- `Agent.ApproveAndExecutePendingActions` is called only by tests, and it
  writes a "user approved action" audit event that the production path
  (`ResolvePendingActions`) never writes. Either production should record that
  event or the tests should move to the production entry. Related test-only
  entries: `ExecuteBypassedPendingActions`, public `SubmitToolOutputs`,
  `GetContextStats`, `WithHistoryRecorder`, `Compressor.Compress`,
  `SystemPromptBuilder.SetStaticPrompt`, `catalogschema.NewDriver`,
  `WithCompressorSummarizer`, `SkillDefinition.HasAlias`.
- The BPE tokenizer is quadratic on long runs of a repeated rune or short
  phrase (30,000 identical CJK runes take about 7 seconds). This is reachable
  from real input, not only from tests.
- Four hand-rolled reminder injections (see Context injection).
- `session.Session.Turns` duplicates `AgentSnapshot.Turns`; it is written and
  persisted but not read.
- `/snapshot`, `/rewind` and `/snapshots` slash commands cannot work: they
  need an executor the TUI no longer has.
- The embedded service does not run repository maintenance.
- `/internal/sessions/{id}/turn/interrupted` has no caller in this repository.
- A spawned child agent does not inherit the parent's skill directories as
  safe read locations.
