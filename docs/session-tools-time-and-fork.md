# Session Time Filters and Forking

## Usage

Session tools retain the daemon's workspace/channel authorization rules. Desktop callers can read authorized workspaces. Channel connection owners can read other non-archived sessions in the bound workspace; other Channel callers can read only their currently bound session. Channel owners may also fork readable sessions in the same workspace, subject to the existing runtime, approval and prompt constraints. Every call resolves current ownership from the stored connection and trusted input actor, so revocation applies to subsequent calls.

`zotigo.list_sessions` adds optional `activity_since` and `activity_until` parameters. `zotigo.read_session` adds optional `since` and `until` parameters. All accept RFC3339 timestamps with a timezone. Intervals are half-open, `[since, until)`, with an omitted endpoint leaving that side unbounded. Listing matches any user, assistant, or steering message within the window, not just the session's last update time. Reading combines time filters with exclusive sequence cursors.

```json
{"activity_since":"2026-09-13T00:00:00+08:00","activity_until":"2026-09-20T00:00:00+08:00","limit":20}
```

Time-filtered list results include `last_matched_message_at`. Read items include `timestamp` and, when available, `turn_id`. For compatibility, listing remains ordered by session ID ascending and paginated with `after_id`, not by descending activity time. Reading retains sequence pagination and the existing 8 KiB text budget. A time window does not automatically expand to include complete turns.

`zotigo.create_session` adds an optional `fork_from` parameter:

```json
{
  "workspace_id":"workspace_123",
  "title":"Alternative approach",
  "fork_from":{"session_id":"sess_abc","through_turn_id":"turn_456"},
  "initial_message":"Use the previous context to try an alternative implementation."
}
```

The caller must have permission to read the source history. The source runtime, approval policy, and prompt configuration must match the caller's. Responses retain `session_id/command_id/status` and add `forked_from`. `accepted` means only that the initial message was accepted, not that its task completed. Uncertain external delivery still returns `outcome_unknown`; do not blindly retry with a new call ID.

## Public API for the UI

`POST /sessions/{id}/fork` uses the same authentication as the other public APIs. It creates an independent, idle branch without automatically sending a new message.

```json
{"request_id":"client-generated-unique-id","through_turn_id":"turn_456","title":"Alternative approach"}
```

`request_id` is required and limited to 200 bytes. Clients retain it when retrying the same operation after a failure. `through_turn_id` is optional; omitting it selects the latest completed turn. `title` is optional and limited to 200 bytes. Success returns HTTP 201 with the new session in the standard envelope's `data`, including `forked_from: {session_id, through_turn_id}`. Invalid parameters return 400. An unavailable exact fork, unavailable source, request conflict, or uncertain outcome returns 409. An uncertain outcome requires checking the target; clients must not automatically retry with a new request ID.

The latest turn's action bar stays visible. Earlier turns show it on hover or focus, and touch devices show it persistently. Forking is disabled for an in-progress turn. The sidebar session context menu forks the latest completed turn. On success, the UI opens the branch and focuses its input field.

## Context and Storage Boundaries

- Forking copies model context through the selected completed turn, inclusive. It does not modify the source session or copy or roll back workspace files.
- The **Codex runtime uses Codex app-server's native `thread/fork` RPC**, with `threadId` identifying the source and `lastTurnId` selecting the boundary. Zotigo does not reconstruct Codex context from its own JSONL files. The implementation in [codex_fork.go](../internal/zotigod/codex_fork.go) sets `excludeTurns: true` and `deferGoalContinuation: true`, then verifies the child thread's final turn through `thread/turns/list`. Inherited goals must not run automatically. A Codex version supporting these interfaces is required. Dynamic-tool capabilities are inherited from the source; forking does not install new tools into an old thread. See the [official Codex App Server documentation](https://learn.chatgpt.com/docs/app-server) for the native fork API.
- The **native Zotigo runtime** stores a history length and digest for each newly completed turn, without copying a complete snapshot per turn. Forking first verifies the current history prefix. If it has been compacted, recovery follows compressor summary references to existing `~/.zotigo/sessions/compacted/transcript_*.jsonl` archives. It checks archived and expanded prefixes at each step and creates a branch only when the target turn's digest matches. It does not scan the entire archive directory, overwrite the source session, or re-execute historical tools.
- Archive recovery is limited to 64 files, 64 MiB and 100,000 messages in total, with a 16 MiB limit per JSONL record. Only regular files under the compacted directory are allowed. Symlinks, out-of-scope references, cycles, and corrupt records are rejected, and request cancellation is respected. The directory follows the native worker's HOME configuration, not a custom session-store root.
- Archives are not complete versioned snapshots: prompt projection may already shorten tool output before compaction; archives contain only the compacted prefix; and the retained suffix may be rewritten or receive newly inserted context. Exact forking is therefore still rejected when digests do not match, archives are missing, or legacy turns have no checkpoint. There is no approximate fallback based only on answer text or timestamps. Archive recovery requires no new storage format or migration; reverting that change only removes the recovery capability.
- Pending commands, approvals, channel bindings, and worker ownership are not inherited. The initial input after tool-driven creation retains the original caller identity and delegation restrictions.
- The fork journal is persisted before remote creation. If remote creation succeeds but local completion is unconfirmed, an unbound remote thread or incomplete local branch may remain. Creation is not automatically repeated. These outcomes require manual verification; potentially executed resources are not automatically deleted.

## SQLite Indexes and Performance Boundaries

History JSONL remains the source of truth. The existing session SQLite database gains `display_items` (session, sequence, message timestamp, conversation marker, file offset, and length) and `display_index_files` (index progress). The index does not duplicate message bodies.

The daemon performs incremental repair at startup and every 30 seconds. Historical backfill uses batches of 256 records and releases the lock between batches. Appending messages updates the index synchronously. If historical logs are not fully indexed or a write was interrupted, reads explicitly return `conversation index is rebuilding; retry shortly`. They do not return partial results as if complete or scan the entire log at query time. Rebuilding corrupt indexes and invalidating indexes before log replacement must go through the storage layer.

Reading uses indexed offsets to fetch only the current page's bodies. Listing queries the time index for authorized candidate sessions without loading their conversation bodies. It still enumerates session organization records and queries the index per candidate, so cost grows with the candidate count. Authorization still reads the caller's log to recover trusted turn identity. Storage pagination benchmarks therefore do not measure complete tool-call latency or establish constant overhead for the full path.

## Compatibility, Rollout, and Rollback

Deploy the daemon before the UI; older daemons do not support the fork route. Indexes are rebuildable derived tables: original logs are neither migrated nor deleted. Older versions can still read existing sessions without the new indexes. Logs produced during a downgrade require backfill after upgrading again. Older workers may drop the new native JSON fork checkpoints when saving sessions, reducing the set of forkable turns without preventing existing conversations from continuing.

Because these changes affect authorization, persistence, and retry semantics, production rollout still requires human review. Validation uses isolated temporary environments, not production services. Pending delegated commands retain the existing session-tools downgrade restrictions: they must not be replayed by an older daemon that cannot restore and verify authorization.
