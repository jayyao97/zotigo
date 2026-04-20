# Trust and Snapcommit Plan

## Background

The current approval model is too coarse for a trustworthy autonomous coding workflow.

- `manual` mode pauses for unsafe tool calls and auto-executes a limited set of read-only calls.
- `auto` mode executes every tool call, including mutating shell and git operations.
- The sandbox policy can classify commands as `normal`, `high`, or `blocked`, but `high` risk commands are not forced back into an approval step.
- `snap-commit` exists only as a user-invoked slash command and is not part of the safety pipeline.

This creates a trust gap: turning on auto-approve currently removes the last human checkpoint for destructive actions, while rollback protection depends on the user remembering to create snapshots manually.

## Goals

- Preserve a low-friction auto-approve path for genuinely safe operations.
- Ensure destructive or high-risk actions still require explicit user approval, even when auto-approve is enabled.
- Automatically create a rollback point before the first mutating or unsafe action in a turn, when the workspace is a git repository.
- Keep the initial implementation minimal and compatible with the current agent loop.

## Non-Goals

- Do not automatically initialize git repositories in non-git directories.
- Do not snapshot before every user message or every turn unconditionally.
- Do not build a full policy engine for all tools in the first iteration.
- Do not replace `snap-commit` slash commands; they remain useful as explicit user controls.

## Configuration Requirements

The classifier must be configurable. It should not be hardcoded as always-on with a fixed model or fixed policy threshold.

At minimum, configuration should support:

- enabled or disabled
- provider selection
- model selection
- timeout
- decision mode for borderline cases
- whether classifier `allow` can bypass user approval for specific risk classes

Configuration should preferably be available at the profile level, with sensible global defaults.

The same principle applies to audit data capture:

- default to compact structured records
- make raw context capture optional
- keep auditability without causing session bloat

## Proposed Behavior

### Approval Model

Replace the effective binary behavior of `manual` vs `auto` with a classification step for each tool call:

- `auto_execute`: safe to run without user interruption
- `require_approval`: must pause and ask the user
- `block`: cannot run

This classification should be applied regardless of whether the UI is currently in manual or auto mode. Auto mode should only skip approval for calls classified as `auto_execute`.

### Tool Classification Rules

#### Read-only tools

Auto-execute only when they are genuinely read-only and path-scoped to safe directories.

Examples:

- `read_file`
- `grep`
- `glob`
- `git_status`
- `git_diff`
- `lsp`
- `web_search`
- `web_fetch`

#### Mutating file tools

Require approval by default, and participate in pre-execution snapshot creation.

Examples:

- `write_file`
- `edit`
- `patch`
- `git_add`
- `git_commit`

#### Shell tool

Shell calls must be classified using sandbox risk analysis instead of the current blanket mutating flag.

- `blocked`: reject immediately
- `high`: require approval
- `normal` with clear side effects: require approval
- `normal` and effectively read-only: may auto-execute

This allows the system to keep low-risk shell reads fast while still stopping on destructive commands.

### Small Classifier for High-Risk Decisions

For high-risk or ambiguous actions, add a lightweight classifier step inspired by Claude Code style safety checks.

The classifier should not replace deterministic policy. It should sit behind it:

- hard-block obviously forbidden actions
- auto-execute clearly safe actions
- send only ambiguous or high-risk actions to the classifier

The classifier should return one of:

- `allow`
- `deny`
- `ask_user`

It should also return a short reason that can be surfaced either to the user or back to the main agent loop.

#### Why use a classifier

Rules are good at catching obvious patterns, but weak at contextual intent.

Examples where context matters:

- deleting generated files inside the repo because the user explicitly requested cleanup
- rewriting a config file after the user asked for a migration
- running a shell command that looks mutating, but is scoped and expected in context

The classifier can make a bounded judgment using limited context, without letting the main model freely rationalize unsafe actions.

#### Classifier scope

Run the classifier only for actions that are:

- `high` risk by sandbox classification
- mutating shell commands with unclear intent
- destructive git operations
- restore or rollback operations
- other future operations explicitly marked as `classifier_required`

Do not run the classifier for:

- blocked commands
- clearly safe read-only calls
- standard file edits that already require user approval through simple policy

Classifier usage itself should also be configurable. For example:

- `off`
- `high_risk_only`
- `high_risk_and_ambiguous`

#### Classifier input

Keep context intentionally small and structured:

- current user prompt
- current pending tool call name and arguments
- sandbox risk level
- whether the workspace is a git repository
- whether a snapshot already exists for the current turn
- recent relevant tool calls and tool results
- optionally a short excerpt of the assistant's recent plan text

Avoid passing the full conversation transcript.

#### Classifier output

The output should be structured JSON with a minimal schema, for example:

```json
{
  "decision": "ask_user",
  "reason": "This command deletes files and should be explicitly approved by the user.",
  "requires_snapshot": true
}
```

Suggested semantics:

- `allow`: execution may continue
- `deny`: execution is refused and the reason is fed back to the main model
- `ask_user`: pause and ask the user directly, showing the reason in the approval UI

When configuration disables classifier-authorized execution, an `allow` result should still fall back to user approval for the relevant risk class.

#### Interaction with the main agent

If the classifier returns:

- `allow`: continue toward snapshot creation and then execution
- `deny`: convert the denial into a tool result so the main model can revise its plan
- `ask_user`: enter approval flow and show the classifier reason to the user

This keeps the main model informed without letting it silently override safety decisions.

## Audit and Storage

Classifier and safety decisions should be stored for auditability.

They should be associated with the turn in which they occurred, but should not be mixed into normal conversation messages. Safety decisions are operational audit records, not chat content.

### Recommended storage shape

Store safety decisions under each turn, for example:

- `turn.safety_events[]`

This is preferred over embedding them into `messages[]`, because:

- safety decisions have different semantics than user or assistant messages
- audit queries are naturally turn-scoped
- the main conversation history stays clean
- later extraction into a dedicated audit store remains easy

### Safety event contents

Each safety event should include compact structured fields such as:

- timestamp
- turn id
- tool call id
- tool name
- tool arguments summary
- decision source
  - `hard_rule`
  - `classifier`
  - `user_approval`
- decision
  - `allow`
  - `deny`
  - `ask_user`
- reason
- risk level
- snapshot status
  - `not_needed`
  - `created`
  - `failed`
  - `missing_git_repo`
- snapshot id if available
- classifier provider and model if classifier was used

### Context retention policy

Do not store the full classifier context by default.

Instead, store only a compact context summary sufficient for audit:

- user prompt summary
- recent action summary
- trigger summary, such as why the action entered classifier evaluation

This keeps the audit trail useful without duplicating large parts of the session transcript.

Recommended default:

- store structured audit fields
- store short context summaries
- do not store raw classifier input or full recent message history

Optional future configuration may enable bounded raw context capture for debugging, but that should be off by default.

## Data Model Evolution

The current persisted session shape is minimal:

- `session.metadata`
- `session.agent_snapshot`

There is no explicit persisted turn model yet. Since safety audit should be turn-scoped, the implementation should introduce a turn-aware persistence shape instead of trying to infer turns later from raw message history alone.

### Recommended first-step model

Add a lightweight persisted turn record to the session model, for example:

- `session.turns[]`

Each turn record should be small and operationally focused. It does not need to duplicate the full agent message history.

Suggested turn fields:

- `id`
- `created_at`
- `updated_at`
- `user_prompt_summary`
- `safety_events[]`
- `snapshot_status`
- `snapshot_id`

This allows safety auditing to be attached to a clear execution unit without rewriting the entire chat persistence model up front.

### Relationship to agent snapshot

`agent_snapshot.history` should remain the source of truth for restoring the current conversation state.

`session.turns[]` should serve a different purpose:

- auditability
- structured per-turn status
- future UI views for approvals and safety history

This separation avoids polluting the conversation history with operational safety records while keeping implementation complexity bounded.

### Migration posture

The first implementation should preserve backward compatibility:

- old sessions without `turns[]` remain loadable
- new sessions can start with an empty `turns[]`
- safety-aware code should tolerate missing historical turn data

No destructive migration should be required for existing session files.

### Snapshot Trigger

If the current workspace is a git repository, create one `snap-commit` snapshot per turn:

- only when the turn is about to execute its first mutating or non-safe action
- only once per turn
- after approval is granted, but before the first protected action actually runs

If a turn contains only read-only actions, no snapshot should be created.

### Snapshot Failure Handling

If a protected action requires a snapshot and snapshot creation fails:

- do not silently continue for high-risk actions
- return to an approval state with a clear message that rollback protection could not be established
- let the user explicitly choose whether to continue without snapshot protection

### Non-Git Workspaces

If the workspace is not a git repository:

- do not auto-run `git init`
- do not auto-create `snap-commit` protection
- for high-risk actions, surface that execution will proceed without rollback protection

Future configuration may allow opt-in automatic git initialization, but it should not be default behavior.

## Implementation Plan

### 1. Introduce execution classification in the agent

Add a tool-call decision layer in the agent that determines whether each call should:

- auto-execute
- require approval
- be blocked

This should replace the current logic where:

- `manual` mode splits only by `isToolCallSafe()`
- `auto` mode executes everything

The new decision path should be shared by both modes.

### 2. Extend safety metadata and shell inspection

Keep the existing `ToolSafety` contract, but add agent-side special handling for `shell`:

- parse shell tool arguments
- call sandbox command classification
- map `RiskLevelBlocked` to `block`
- map `RiskLevelHigh` to `require_approval`
- detect obvious mutating shell commands and treat them as `require_approval`

This avoids treating all shell commands the same.

### 2a. Add a dedicated classifier interface

Introduce a small safety-classifier abstraction, for example under a new package such as:

- `core/safety`

The interface should accept a bounded request object and return a structured decision.

Suggested request fields:

- user prompt
- tool name
- tool arguments
- recent tool calls
- recent tool results
- sandbox risk level
- is git repo
- has snapshot this turn

Suggested result fields:

- decision
- reason
- requires snapshot

The first implementation can be a deterministic stub with the same interface. A model-backed classifier can be added later without changing the agent flow.

### 2b. Add classifier configuration

Add classifier settings to config, ideally under the active profile so different profiles can adopt different trust models.

Suggested fields:

- `enabled`
- `mode`
- `provider`
- `model`
- `timeout_ms`
- `max_recent_actions`
- `capture_raw_audit_context`
- `max_audit_context_chars`

Example intent, not final schema:

```yaml
profiles:
  default:
    safety:
      classifier:
        enabled: true
        mode: high_risk_and_ambiguous
        provider: openai
        model: gpt-5-mini
        timeout_ms: 3000
        max_recent_actions: 6
        capture_raw_audit_context: false
        max_audit_context_chars: 1200
```

Recommended default posture:

- classifier enabled
- only for high-risk and ambiguous actions
- classifier can return `deny` or `ask_user`
- classifier `allow` does not automatically bypass user approval for the highest-risk actions unless explicitly configured
- raw audit context capture disabled

### 2c. Introduce turn-aware audit persistence

Extend session persistence to support lightweight turn records.

At minimum:

- start a new turn record when a new user message enters the run loop
- append safety events to the active turn
- persist snapshot status on the active turn
- keep `agent_snapshot` unchanged for conversation restore

The first pass does not need full turn replay semantics. It only needs enough structure to support reliable audit logging.

### 3. Track per-turn snapshot state

Add turn-scoped state on the agent, for example:

- whether the current turn has already created a snapshot
- the snapshot id or summary returned by `snap-commit`
- whether snapshot creation was attempted and failed

Reset this state at the beginning of each new user turn.

### 4. Add classifier evaluation before protected execution

Before executing risky pending actions:

- run deterministic policy checks first
- call the classifier only for actions that need contextual safety judgment
- use the classifier result to decide whether to execute, deny, or request user approval

This decision step should happen before any snapshot is created, since denied actions should not generate rollback points.

### 5. Add a pre-execution snapshot hook

Before executing pending actions:

- scan the pending actions
- determine whether any action in the batch is protected
- if protected, and no snapshot has been created yet in this turn, and the workspace is a git repository:
  - run `snap-commit store`
  - record the result in turn state
  - continue only after success

This hook should live close to action execution so it applies consistently after approval and before the actual mutation.

### 6. Surface snapshot and classifier context to the user

When a snapshot is created automatically, include a short note in the tool result or assistant response:

- snapshot created
- snapshot id or label if available

When snapshot creation fails, show that explicitly in the approval flow.

When classifier evaluation returns `ask_user`, surface the classifier reason in the approval flow.

When classifier evaluation returns `deny`, include the reason in the tool result sent back to the main model.

### 6a. Persist safety events with turn state

Persist safety decisions with the turn they belong to.

The first implementation should:

- add a dedicated safety event structure
- attach safety events to turn state
- keep them out of normal message history

This gives good auditability now and preserves flexibility for future storage changes.

## Failure Modes and Expected Behavior

The implementation should define behavior for the main failure paths up front.

### Classifier unavailable

If the classifier is enabled but cannot be reached:

- do not silently downgrade to auto-execution
- fall back to `ask_user` for actions that depended on classifier evaluation
- record the failure in the safety event

### Snapshot creation failure

If snapshot creation fails before a protected action:

- do not silently continue for high-risk actions
- require explicit user confirmation to continue without rollback protection
- record the failure reason in the active turn

### Session persistence failure

If session save fails after a safety decision:

- execution should not be blocked solely by audit persistence failure in the first iteration
- the failure should be surfaced in logs or debug output
- this behavior should be documented as a temporary limitation

### Missing turn state

If the active turn record cannot be found:

- create a fallback turn record lazily
- do not drop safety events silently

## Implementation Phases

### Phase 1: Decision pipeline hardening

Scope:

- replace the current `auto executes everything` behavior
- classify tool calls into `auto_execute`, `require_approval`, or `block`
- enforce that high-risk actions still stop for approval

Deliverable:

- auto-approve no longer bypasses destructive approvals

### Phase 2: Classifier interface and configuration

Scope:

- add the classifier abstraction
- add config wiring and safe defaults
- ship a deterministic stub implementation first

Deliverable:

- the agent can call a classifier component without yet depending on a model-backed version

### Phase 3: Turn-aware audit persistence

Scope:

- add `session.turns[]`
- add `turn.safety_events[]`
- persist compact context summaries

Deliverable:

- safety decisions become queryable and reviewable per turn

### Phase 4: Turn-scoped auto snapshot

Scope:

- detect the first protected action in a turn
- create one snapshot in git repos before execution
- persist snapshot outcome

Deliverable:

- one protected snapshot per turn, no more and no less

### Phase 5: UX polish and operational feedback

Scope:

- surface classifier reasons in approval UI
- surface snapshot creation and failure details clearly
- align `/rewind` semantics with the new trust model

Deliverable:

- users can understand why the system paused, denied, or continued

## Acceptance Criteria

The implementation is acceptable when all of the following are true.

### Approval and execution

- enabling auto-approve does not cause high-risk or destructive actions to run without an approval checkpoint
- blocked actions are denied without execution
- clearly read-only actions continue to auto-execute without unnecessary friction
- classifier-driven `ask_user` decisions pause execution and surface a human-readable reason
- classifier-driven `deny` decisions are returned to the main agent loop as structured denial feedback

### Snapshot behavior

- in a git repository, the first protected action in a turn creates exactly one snapshot before execution
- later protected actions in the same turn do not create extra snapshots
- turns with only safe read-only activity create no snapshot
- snapshot failure prevents silent execution of high-risk actions

### Persistence and audit

- safety events are persisted under the correct turn
- persisted safety events do not pollute normal message history
- audit records store summaries by default, not full raw classifier context
- existing sessions created before this feature remain loadable

### Configuration

- classifier behavior can be disabled entirely by config
- classifier provider/model/timeout can be configured
- config can force classifier `allow` to still require approval for certain classes of action

### Operability

- the system behaves safely if the classifier is unavailable
- the system behaves safely if snapshot creation fails
- tests cover the critical happy paths and failure paths

## Out of Scope for the First Code Pass

These items are deliberately deferred so the first implementation stays bounded:

- model-backed classifier prompt tuning
- a dedicated audit database or separate audit store
- per-session TUI overrides for every classifier option
- retrospective rebuilding of historical turns from old sessions
- advanced analytics over audit records

### 7. Keep slash commands, but align semantics

Retain:

- `/snapshot`
- `/rewind`
- `/snapshots`

But align them with the trust model:

- `/rewind` should default to dry-run behavior unless the user explicitly confirms restore
- restore should remain a protected action

## Suggested Code Areas

- `/Users/yaotianjia/workspace/zotigo/core/agent/agent.go`
- `/Users/yaotianjia/workspace/zotigo/core/agent/types.go`
- `/Users/yaotianjia/workspace/zotigo/core/sandbox/guard.go`
- `/Users/yaotianjia/workspace/zotigo/core/sandbox/policy.go`
- `/Users/yaotianjia/workspace/zotigo/core/tools/builtin/shell.go`
- `/Users/yaotianjia/workspace/zotigo/cli/tui/model.go`
- `/Users/yaotianjia/workspace/zotigo/cli/commands/builtin/snapshot.go`

## Rollout Order

1. Change approval behavior so auto-approve no longer bypasses high-risk confirmation.
2. Add shell classification using sandbox risk levels.
3. Introduce the classifier interface with a deterministic stub.
4. Add classifier configuration and safe defaults.
5. Add turn-aware session persistence and turn-scoped safety events.
6. Add turn-scoped safety event persistence with compact context summaries.
7. Add turn-scoped auto-snapshot for the first protected action in git repositories.
8. Improve failure messaging and rewind confirmation UX.
9. Add tests for approval classification, classifier decisions, audit persistence, and snapshot triggering.

## Test Plan

Add or update tests for these scenarios:

- auto-approve still pauses on high-risk shell commands
- blocked shell commands are rejected without execution
- read-only tool calls in safe paths still auto-execute
- classifier `deny` returns a reason to the main model
- classifier `ask_user` pauses with a user-visible reason
- classifier `allow` continues execution
- classifier configuration can disable the feature cleanly
- classifier configuration can force `allow` to still require user approval
- safety events are persisted with the correct turn
- audit records contain summaries, not full raw context by default
- first mutating action in a git repo triggers exactly one snapshot per turn
- subsequent mutating actions in the same turn do not create additional snapshots
- turns with only read-only actions do not create snapshots
- snapshot failure pauses or denies protected execution with a clear reason
- non-git workspaces do not auto-initialize repositories
- sessions without the new turn structure still load correctly

## Open Questions

- Whether `normal` shell commands with side effects should be detected through heuristics, an explicit command allowlist, or both.
- Whether automatic snapshots should be configurable by profile.
- How much snapshot metadata should be persisted in session state for later rewind UX.
- Whether the classifier should use a separate small model, a separate provider profile, or a one-shot call through the current provider.
- Whether classifier config belongs only in profile config or also needs per-session overrides from the TUI.
- Whether bounded raw context capture should be available only in debug profiles or also in normal profiles.

## Next Coding Step

The next implementation step should start with data model and configuration scaffolding, because both the classifier pipeline and audit persistence depend on them.

Specifically:

1. extend session structs to support lightweight turn records and safety events
2. extend config structs for classifier settings and defaults
3. add agent-side types for execution decisions, classifier requests, classifier results, and snapshot turn state

Once those scaffolds exist, the approval pipeline can be refactored with much lower risk.
