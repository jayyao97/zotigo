# Prompt Cache Stability: Design, Status, and Validation

## Status

This document is the single source of truth for Zotigo prompt-cache stability.
It replaces the earlier persistent-user-context proposal, its review report,
and the separate zotigod session-cache-rate proposal.

Two pieces have different implementation status:

| Area | Status |
| --- | --- |
| Append-only dynamic user context | Implemented and locally verified |
| Durable zotigod session cache-rate API | Future work; not implemented |

## Problem

Zotigo previously rendered environment and project context on every provider
request and inserted it transiently before the latest real user message. The
message was not stored in `Agent.History`:

```text
turn 1: system, context-1, user-1
turn 2: system, user-1, assistant-1, context-2, user-2
```

Moving or replacing an already-sent prompt fragment changes the provider
prefix and prevents reuse of the conversation KV cache beyond the stable
system prompt.

The implemented model makes model-visible context append-only:

- persist initial context before the first real user message;
- append only changed context sections on later user boundaries;
- save the diff baseline in the session snapshot;
- keep internal context out of the display log; and
- replace accumulated context updates with one current full context after
  compaction.

## Three Message Views

Zotigo deliberately has three related but different histories:

| Data | Agent history | Session snapshot | Display log |
| --- | ---: | ---: | ---: |
| Real user message | Yes | Yes | Yes |
| Assistant response | Yes | Yes | Yes |
| Context full/delta | Yes | Yes | No |
| Compaction summary | Yes | Yes | No |
| Empty-response recovery nudge | Yes | Yes | No |
| Presentation-only tool event | Not necessarily | Not necessarily | Yes |

`Agent.History` is the provider context. The display log is the durable UI read
model used by CLI/TUI and zotigod. A contextual message must never be inferred
from XML text or rendered as if the user typed it.

## Message Model

Context is a normal provider-facing user message with an internal marker:

```go
type Message struct {
    // Existing fields...
    Contextual bool `json:"contextual,omitempty"`
}
```

Providers ignore the marker and continue mapping the role as `user`. Runtime
code uses `IsContextualUser()` to exclude it from user-facing behavior,
`LastPrompt`, and compression summaries.

Each dynamic section has a stable key and SHA-256 digest:

```text
environment
project_instructions:AGENTS.md
project_instructions:CLAUDE.md
```

The snapshot stores only `key -> digest` in `UserContextState`; rendered
content remains in history and is not duplicated in snapshot metadata.

## Turn Shapes

The examples use:

```text
S        stable system prompt
SD       optional dynamic system block
C(full)  complete contextual user message
C(delta) changed contextual sections
U1/A1    real user and assistant messages
T1       tool result message
CS       compaction summary (internal user-role message)
N        empty-response recovery nudge (internal user-role message)
```

Their concrete contents and lifetimes are:

| Symbol | Typical contents | Source | Persisted in `Agent.History` | Change behavior |
| --- | --- | --- | ---: | --- |
| `S` | Product behavior, tool-use rules, safety guidance, coding workflow, and other instructions from `system_prompt.md` | `SystemPromptBuilder` static prompt | No; rebuilt before each provider request | Expected to remain byte-stable across the session |
| `SD` | Currently the discovered `available_skills` index; future hosts may register other dynamic system sections | `SystemPromptBuilder` dynamic sections | No; rebuilt before each provider request | May change when skills are added, removed, or reloaded; a change can invalidate the prefix near the start |
| `C(full)` | All registered user-context sections: environment fields such as working directory, platform, transport, current date, and timezone, plus loaded `AGENTS.md`/`CLAUDE.md` project instructions | `UserContextBuilder` | Yes, with `Contextual=true` | Written once for a new/legacy session and rebuilt after compaction |
| `C(delta)` | Only added or changed keyed sections. In current wiring this is normally an environment/date change or a project-instruction change detected after resume; removed sections use `context_removed` | `UserContextBuilder.BuildUpdate` | Yes, with `Contextual=true` | Appended immediately before the next real user input; never rewrites an older context message |
| `U1` | User-authored text, images, or other structured content; queued steering messages are merged into the same shape | CLI/TUI, ACP, zotigod, or another transport | Yes | Appended at a new user boundary |
| `A1` | Assistant text, reasoning blocks, and tool calls; completed messages may also carry normalized provider usage metadata | Provider stream assembled by `Agent` | Yes | Appended after each provider generation |
| `T1` | Tool results, including text/JSON/content results, denial messages, errors, and tool-call IDs needed to pair results with calls. Loop warnings and some tool reminders may be prefixed into this result text | Tool execution loop | Yes | Appended after tool execution before the next provider continuation |
| `CS` | `[Previous conversation summary]` followed by the compressor's summary and optional transcript path | `Compressor` | Yes, as `RoleUser` | Replaces the compressed portion of history after successful compaction; it is internal and is not copied into the display log |
| `N` | A `<system-reminder>` asking the model to continue after it emitted neither visible text nor a tool call | Empty-response recovery | Yes, as `RoleUser` | Appended at most once for a consecutive empty response and retried immediately; it is internal and is not copied into the display log |

`S` and `SD` are provider-request scaffolding rather than conversation
history. `C`, `U`, `A`, `T`, `CS`, and `N` are resumable runtime history. Only
real `U` messages are user-authored; the others are runtime protocol records.
Tool definitions themselves are sent as a separate provider request parameter
rather than as a message; Zotigo sorts them by name so their serialization
remains deterministic.

### No user-context builder

```text
turn 1: S, U1
turn 2: S, U1, A1, U2
```

This is naturally append-only.

### Initial context

Internal history:

```text
C(full), U1, A1
```

Provider request before normalization:

```text
S, C(full), U1
```

Every provider calls `MergeConsecutiveUserMessages`, producing:

```text
S, User[C(full) + U1]
```

This notation is the normalized protocol shape immediately before each
provider adapter converts it to SDK parameters. It is not a literal wire shape
shared by all APIs:

| Adapter | Provider-specific mapping |
| --- | --- |
| OpenAI Chat Completions | Keeps system/user/assistant/tool message roles after consecutive user normalization |
| OpenAI Responses | Joins system blocks into top-level `instructions`; conversation becomes typed input items and tool results become `function_call_output` items |
| Anthropic | Moves system blocks to the system parameter; contextual and real user content are user blocks, while tool results are also represented inside user messages |
| Gemini | Joins system text into `SystemInstruction`; assistant messages become model content and tool results become user-role function responses |

Provider converter tests are the source of truth for the final SDK shape.

### Unchanged context

No contextual message is appended:

```text
S, User[C(full) + U1], A1, U2
```

The complete previous request remains an exact prefix.

### Changed context

Only changed or added sections are appended immediately before the next real
user message:

```text
S, User[C(full) + U1], A1, U2, A2, User[C(delta) + U3]
```

For example:

```xml
<user_context update="delta">
<environment>
current_date: 2026-08-02
</environment>
</user_context>
```

Removing a section appends an explicit `context_removed` marker. Existing
prefix content is never rewritten; the later section is authoritative.

### Steering

Queued user corrections use the same boundary:

```text
..., assistant/tool progress, C(delta), queued-user-message
```

Multiple queued inputs may be merged, but any context delta remains directly
before the merged real user input.

## Diff Rules

At a real user boundary:

| Previous/current state | Appended context |
| --- | --- |
| No baseline | Full current context |
| Same key and digest | Nothing |
| New key | Full new section in a delta |
| Changed digest | Full current section in a delta |
| Missing current key | Removal marker |

Context refresh happens for a new user turn and steering input. Tool-loop
continuations, approvals, retries, and empty continuations do not create a
context update.

Project instructions retain their existing load semantics: they are read when
the host constructs the builder. Resume can detect files that changed between
processes, but editing an instruction file inside one already-running process
does not hot-reload it.

## Resume and Legacy Sessions

A normal snapshot persists both history and `UserContextState`:

- unchanged after resume: append only the real user message;
- changed after resume: append a delta first;
- legacy snapshot without a baseline: append one full context before the next
  real user message.

The JSON additions are backward-compatible because absent fields decode to
their zero values.

One defensive improvement remains: if a damaged or externally produced
snapshot contains `UserContextState` but no contextual history message,
`Restore` should discard the baseline so the next input emits a full context.
Normal Zotigo save paths keep the two consistent.

## Compaction

Successful compaction necessarily rewrites the prompt and therefore starts a
new cache prefix. Context handling is centralized for threshold-triggered,
manual, and reactive compaction. The public `Agent.ForceCompress()` currently
uses the threshold-aware `Compress()` path despite its name, so a manual call
below the threshold is a no-op. Reactive context-length recovery uses the truly
unconditional compressor path. Whenever either path actually reports
`Compressed=true`, Zotigo:

1. exclude contextual messages from summarizer input;
2. compress conversational history;
3. remove old full and delta contextual messages;
4. render one authoritative current full context;
5. insert it after the conversation summary; and
6. replace the digest baseline.

The result is:

```text
summary, C(full-current), preserved recent messages...
```

Later turns are append-only again.

## Cache Measurement

Provider adapters normalize usage into:

```text
InputTokens              uncached input
CacheCreationInputTokens input written to cache
CacheReadInputTokens     input served from cache
```

Per-turn cache-read rate is:

```text
cache_read_rate = cache_read_input_tokens / total_input_tokens

total_input_tokens =
    input_tokens
  + cache_creation_input_tokens
  + cache_read_input_tokens
```

Two aggregates answer different questions:

- mean turn rate: arithmetic mean of each generation's rate;
- overall rate: sum of cache-read tokens divided by sum of total input tokens.

The overall rate is the preferred session metric because it is token-weighted.

## Validation

### Deterministic tests

The test suite covers:

- initial full context before the real user message;
- unchanged context preserving the provider-message prefix;
- changed and removed sections;
- snapshot round-trip and resume;
- legacy snapshot adoption;
- queued steering input;
- display-log isolation and real `LastPrompt` selection;
- contextual-message exclusion from summaries;
- threshold-satisfying manual and unconditional reactive compaction rebase; and
- deterministic consecutive-user normalization.

The protocol-level prefix test preserves 3/3 previous messages. After provider
normalization the same shape is 2/2 messages. This is a 100% structural
cacheability result, not a claim about service-side cache reads.

### Live DeepSeek E2E

A historical 10-turn probe ran against both configured DeepSeek profiles. It
emitted one initial full context and five deltas by changing date and a
synthetic Git-status section. Git status was probe-only: production wiring does
not currently inject Git status. Production dynamic context consists of the
environment section (working directory, platform, transport, current date, and
timezone); project instruction files are loaded when the builder is created,
not hot-reloaded during a process.

| Scope | Mean turn rate | Token-weighted rate |
| --- | ---: | ---: |
| Both models, including cold turn | 87.49% | 87.87% |
| Both models, excluding cold turn | 97.21% | 97.21% |
| Changed warm turns only | 96.71% | 96.72% |
| Unchanged warm turns only | 97.83% | 97.84% |

Per profile after excluding the cold turn:

| Profile | Mean turn rate | Token-weighted rate |
| --- | ---: | ---: |
| DeepSeek Flash | 97.35% | 97.36% |
| DeepSeek Pro | 97.06% | 97.07% |

Dynamic deltas cost roughly 1.1 percentage points in this historical sample
because the new delta is an uncached tail. They did not invalidate the existing
prefix; subsequent turns cached the extended history.

The reproducible DeepSeek-only dynamic test uses a deterministic synthetic
runtime-state section with the same 10-turn pattern: one full update, five
deltas, and four unchanged turns. It prints every turn's cache-read rate plus
the arithmetic mean and token-weighted overall rate, both including and
excluding the cold first turn:

```bash
go test -tags=e2e -v -count=1 \
  -run '^TestE2E_DeepSeekDynamicContextCaching$' ./tests/e2e/
```

The older three-turn static cache smoke test remains available separately:

```bash
go test -tags=e2e -v -count=1 \
  -run '^TestE2E_PromptCaching$/deepseek' ./tests/e2e/
```

Live percentages vary with provider-side cache state and model routing, so the
table above records one observed run rather than a fixed test threshold. The
dynamic test asserts the full/delta sequence structurally and requires cache
reads on at least one warm turn.

The first run of the formalized E2E on 2026-08-01 produced:

| Profile | All-turn mean | All-turn overall | Warm-turn mean | Warm-turn overall | Warm turns with cache |
| --- | ---: | ---: | ---: | ---: | ---: |
| DeepSeek Flash | 86.51% | 86.91% | 96.12% | 96.14% | 9/9 |
| DeepSeek Pro | 86.71% | 87.11% | 96.34% | 96.34% | 9/9 |

## Known Boundaries

- `available_skills` remains a dynamic system block. If it changes, it can
  invalidate the prefix independently of user-context handling.
- Exact provider-wire prefix integration assertions are still desirable for
  OpenAI Chat, OpenAI Responses, Anthropic, and Gemini converters.
- Automatic `NeedsCompression` should gain a dedicated contextual test plus a
  next-turn prefix assertion.
- Adding fields to exported Go structs is JSON-compatible but can break
  external positional composite literals; release notes should mention it.

## Future: Durable Zotigod Session Cache Rate

This section records future work and is **not implemented**.

`protocol.SessionUsage(snapshot.History)` is suitable for current retained
history, but not a lifetime session metric: compaction removes old assistant
messages and their usage metadata.

Desktop eventually needs a durable main-agent accumulator stored in
`agent.Snapshot`, updated in the same critical section that appends each
assistant generation. It should track:

```text
cumulative normalized usage
generation count
generations with provider-reported usage
whether the lifetime total is complete
```

Scope must include only main-agent `MessageMetadata.Usage`, excluding tool
child-agent usage, safety classifier calls, compressor calls, and optional
observability traffic.

Legacy sessions can backfill a lower-bound subtotal from retained assistant
messages, but must report `complete=false` because prior compaction cannot be
detected reliably.

The proposed read-only endpoint remains:

```text
GET /sessions/{id}/usage
```

It should return normalized token totals, the token-weighted cache-read rate,
generation counts, completeness, scope=`main_agent`, and the durable snapshot
timestamp. It should read session JSON directly without starting a worker or
loading the session into the daemon registry.

Acceptance criteria for that future work:

- totals survive compaction, restart, offline reads, and profile switches;
- zero input reports rate `null`, measured zero cache reads reports `0`;
- missing provider usage marks the accumulator incomplete;
- the endpoint does not scan the display log or update the SQLite index per
  generation; and
- dedicated protocol, snapshot, session, handler, restart, and race tests pass.
