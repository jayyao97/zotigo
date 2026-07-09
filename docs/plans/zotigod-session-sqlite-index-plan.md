# Zotigod Session SQLite Index Plan

## Background

`zotigod` now supports reopening stored sessions after a daemon restart. Read APIs can show old sessions as `offline`, and `POST /sessions/{id}/messages` can start a worker when the user wants to continue.

The current session store still lists stored sessions through a file-based registry. This is acceptable for the first recovery version, but it is not the right long-term shape for a desktop session list. Desktop needs fast listing, filtering, sorting, and later search across many sessions and projects.

Codex uses a similar split: durable rollout files keep replay history, while SQLite provides queryable metadata. Zotigo should follow that boundary.

## Goal

Add a SQLite-backed session metadata index for `core/session.FileStore` so `GET /sessions` and store `List` can query metadata without relying on a full file-registry scan. The same database may also hold narrow blob-reference indexes, such as accepted message images, when a public read API needs fast lookup without moving the blob bytes into SQLite.

The SQLite index is not the source of truth for conversation history. It is an index over durable session files and display logs.

## Non-Goals

- Do not move display logs into SQLite.
- Do not change `/sessions/{id}/items` pagination or display-log storage.
- Do not migrate image blob bytes into SQLite.
- Do not change worker command replay.
- Do not add archive, pinning, project, or selection state to zotigod. Those are desktop UI concepts.
- Do not add desktop-side SQLite changes in this PR.

## Storage Boundary

Keep these responsibilities separate:

- **Session JSON**: durable agent snapshot and core session metadata.
- **Display log JSONL**: durable transcript, lifecycle events, and command log.
- **Image blob directory**: raw image bytes referenced by display-log commands.
- **zotigod SQLite session index**: queryable metadata owned by zotigo, such as session id, working directory, existing last prompt preview, timestamps, and narrow blob references.
- **zotigo-desktop SQLite**: desktop UI state, such as projects, conversations, pinned order, selected conversation, and the binding to a daemon session.

If SQLite is missing or damaged, zotigod should be able to rebuild the index from existing session JSON files. SQLite should improve query behavior, not become the only copy of session metadata.

The two SQLite databases intentionally overlap only at the binding boundary. zotigod owns daemon session identity and project path metadata. desktop owns local organization and UI state.

## Initial Schema

Use a small schema focused on session metadata needed by the public session list:

```sql
CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY,
  working_directory TEXT NOT NULL,
  last_prompt TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_updated_at ON sessions(updated_at);
CREATE INDEX IF NOT EXISTS idx_sessions_working_directory_updated_at
  ON sessions(working_directory, updated_at);
```

Message images use a separate reference table. It indexes only accepted image
blob metadata, not image bytes:

```sql
CREATE TABLE IF NOT EXISTS session_images (
  session_id TEXT NOT NULL,
  name TEXT NOT NULL,
  blob_path TEXT NOT NULL,
  mime_type TEXT NOT NULL,
  size_bytes INTEGER NOT NULL,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (session_id, name)
);
```

`last_prompt` preserves the existing `session.Metadata.LastPrompt` behavior used by CLI session lists. It is not a desktop conversation title.

`created_at` and `updated_at` are stored as Unix nanoseconds so SQLite ordering is chronological and does not depend on RFC3339 string formatting details.

Do not add title, archive state, pinning, project id, selected state, new preview fields, or last display sequence in the first version. Those fields either belong to desktop UI state or need separate producers and product semantics. In particular, desktop already owns `conversations.title`; zotigod should not introduce a competing title source until we define portable daemon session titles.

## File Layout

Store the database under the existing zotigo root:

```text
~/.zotigo/session_index.sqlite
```

The exact filename can change during implementation, but it should be clearly scoped to session metadata. Avoid names that imply the display log or full runtime state lives in SQLite.

## Write Path

Update the SQLite session metadata index whenever `FileStore` changes durable session metadata:

- `Put`: upsert `session.Metadata`
- `Delete`: delete the row

`AppendDisplayItem` should not update session metadata rows in this version. Display-log append frequency is higher, and we are not indexing last-message preview or last sequence yet. Message image refs are written explicitly by zotigod when image blobs are accepted, so image reads do not need to scan the display log.

The write order should preserve correctness:

- For `Put`, write the session JSON first, then upsert SQLite. If SQLite update fails, return an error so callers know the session list index may be stale.
- For `Delete`, remove durable files first, then remove SQLite. If SQLite delete fails, return an error so the index can be repaired instead of silently showing stale sessions forever.

## Read Path

`FileStore.List` should query SQLite first.

Supported filters should match the current `ListFilter` behavior:

- optional `WorkingDirectory`
- `Limit`
- `OrderByUpdatedDesc`
- `OrderByUpdatedAsc`
- `OrderByCreatedDesc`
- `OrderByCreatedAsc`

`FileStore.Get` should continue reading the session JSON by id. `GET /sessions/{id}` needs the full stored session metadata and later may need the agent snapshot, so SQLite should not replace `Get`.

`GET /sessions/{id}/images/{name}` should query the image reference table by
`(session_id, name)`, then read the referenced blob file. It should not scan the
full display log per image request.

## Index Initialization

On `NewFileStore`:

1. create the sessions directory
2. open/create SQLite database
3. apply migrations
4. if there is no completed bootstrap marker, bootstrap it from existing session JSON files

The first version should avoid scanning all session files on every `NewFileStore`.
Workers also open the store, so startup must stay cheap. The initial bootstrap
only runs when the SQLite metadata table has no completed bootstrap marker:

- scan `sessions/*.json`
- read each session metadata from valid session JSON files
- upsert rows into SQLite
- write the bootstrap marker after all rows are indexed

This keeps old installations working and makes the SQLite index rebuildable
without moving directory scans into the normal worker startup path.
Corrupt or unrelated `sessions/*.json` files are skipped during bootstrap so a
single bad file does not make the whole store unavailable.

Stale rows should be prevented through the normal `Delete` path. A full explicit
repair command can be added later if we need operator recovery for manual file
edits or external corruption.

For rollback compatibility, `NewFileStore` also checks the legacy `registry.json`
mtime. If the registry was updated after the last SQLite sync, the store upserts
registry metadata into SQLite and removes rows that are no longer present in the
registry. This catches sessions created or deleted by an older registry-only
version without scanning every session file on every startup.

## Error Handling

SQLite initialization failure should make `NewFileStore` fail. `zotigod` already treats store initialization failure as a real startup/runtime limitation, and session persistence should not silently degrade to registry-only behavior.

For a corrupted SQLite database, prefer a clear error first. A manual or automatic backup-and-rebuild flow can be a follow-up if we see it in practice. Rebuilding the index is possible because session JSON remains the source of truth, but doing it safely needs a separate policy for backup names and operator visibility.

## Tests

Add `core/session` tests for:

- `Put` creates or updates a SQLite index row.
- `List` returns sessions from SQLite in updated-desc order.
- `List` supports working-directory filter.
- `List` supports limit.
- `List` orders zero-nanosecond and non-zero-nanosecond timestamps chronologically.
- `Delete` removes the SQLite row.
- `NewFileStore` indexes existing session JSON files from an older file-store installation.
- `NewFileStore` skips corrupt session JSON files during bootstrap.
- `NewFileStore` syncs sessions written by an older registry-only version after the bootstrap marker exists.

Add `internal/zotigod` tests only if handler behavior changes. Ideally the existing `GET /sessions` tests keep passing through the store interface.

Run:

```bash
go test ./core/session ./internal/zotigod -count=1
go test ./core/... -count=1
go build ./...
make check
```

## Open Questions

- Which SQLite driver should Zotigo use? Prefer a driver that does not require CGO unless the project already accepts CGO for local builds.
- Should `registry.json` remain as a compatibility file for one release, or can it stop being used once SQLite repair from session JSON exists?
- Should future preview text and last activity metadata be produced by display-log observers or by explicit APIs?
