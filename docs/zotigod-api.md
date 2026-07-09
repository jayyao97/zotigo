# zotigod HTTP API

`zotigod` exposes a small localhost HTTP API for desktop clients. Desktop may
cache responses locally, but zotigo remains the source of truth for session
state and display history.

## Public endpoints

- `GET /health`
- `POST /sessions`
- `GET /sessions`
- `GET /sessions/{id}`
- `POST /sessions/{id}/start`
- `GET /sessions/{id}/items`
- `POST /sessions/{id}/messages`
- `POST /sessions/{id}/pause`
- `POST /sessions/{id}/steering`
- `POST /sessions/{id}/approvals/{approval_id}`

Internal worker endpoints under `/internal/sessions/...` are not public desktop
API and may change without compatibility guarantees.

Current internal worker endpoints include:

- `GET /internal/workers/connect?session_id={id}`
- `POST /internal/sessions/{id}/worker/attach`
- `POST /internal/sessions/{id}/worker/finish`
- `GET /internal/sessions/{id}/commands`
- `POST /internal/sessions/{id}/turn/interrupted`
- `POST /internal/sessions/{id}/approvals`
- `GET /internal/sessions/{id}/approvals/{approval_id}`

## Response envelope

Public HTTP endpoints return a JSON envelope in addition to the HTTP status
code. Successful responses use `code: "ok"` and put the endpoint-specific DTO in
`data`:

```json
{
  "code": "ok",
  "message": "",
  "data": {
    "id": "sess_8f0e12ab34cd56ef"
  }
}
```

Errors keep the non-2xx HTTP status and return a stable error body:

```json
{
  "code": "invalid_request",
  "message": "message requires text or images"
}
```

Current error codes include `invalid_request`, `not_found`,
`method_not_allowed`, `conflict`, `request_too_large`,
`session_not_live`, `service_unavailable`, and `internal_error`.

Internal HTTP endpoints also use this envelope, except
`GET /internal/sessions/{id}/commands` successful responses. The commands
endpoint intentionally returns the raw command page so replayed HTTP commands
and live WebSocket command frames can share the same command DTO. Command
endpoint errors still use the structured `{ "code", "message" }` shape.

Unless a section explicitly says "raw response", response examples below show
the endpoint-specific `data` payload.

## Create sessions

`POST /sessions` creates a zotigod session for a project directory. Desktop
clients should pass the project root selected by the user:

```json
{
  "working_directory": "/Users/me/workspace/project"
}
```

`working_directory` must be an absolute path that resolves to an existing
directory. If it is omitted, zotigod uses its current working directory for
CLI/backward compatibility. The directory is persisted in the core session
store and returned in session responses as `working_directory`.

Workers launched for the session use this directory as their process working
directory and as the source for project config, skills, project instructions,
tools, shell execution, and LSP state. Legacy sessions without a stored working
directory fall back to the worker process current directory.

## Session liveness and recovery

Session history and session runtime are separate. The session store on disk can
contain old sessions and display logs even when the current `zotigod` process
has no worker running for them.

Read APIs do not start workers:

- `GET /sessions`
- `GET /sessions/{id}`
- `GET /sessions/{id}/items`

If a session exists only on disk, `GET /sessions` and `GET /sessions/{id}`
return it as offline:

```json
{
  "id": "sess_8f0e12ab34cd56ef",
  "state": "offline",
  "live": false,
  "working_directory": "/Users/me/workspace/project",
  "created_at": "2026-01-02T03:04:05Z"
}
```

`live: false` means desktop may render history but should not show turn-scoped
controls as usable. Sending a new message or explicitly starting the session can
make it live again. Stored-only sessions are never reported as `running`;
`running` means the current daemon has accepted a worker connection for that
session.

`POST /sessions/{id}/start` is an explicit runtime resume/pre-warm operation. It
loads a stored session into the daemon registry when needed, launches a worker,
and waits for that worker to connect. It does not append a user message.

`POST /sessions/{id}/messages` also resumes an offline session before accepting
the message. Desktop's normal chat flow can call `messages` directly instead of
calling `start` first.

Pause, steering, and approval decisions do not auto-resume offline sessions
because they refer to a currently running turn or a live pending approval. For a
stored-only session they return `409` with `code: "session_not_live"`.

## Read session display items

`GET /sessions/{id}/items` returns a paginated, read-only display log for a
session. This log is a persistent read model for CLI and desktop replay; it is
not `AgentSnapshot.History`, and desktop clients must not read `.zotigo/sessions`
directly.

The current file-store implementation reads the per-session append log before
applying pagination, so `limit` bounds the HTTP response size but is not yet a
tail-read optimization for very long sessions. A future store-level query can
optimize this without changing the public API.

Query parameters:

- `limit`: number of items to return. Defaults to the most recent `50`, maximum
  `200`.
- `after`: return items with `sequence` greater than this cursor.
- `before`: return items with `sequence` lower than this cursor.

`after` and `before` are mutually exclusive. Responses are always ordered by
`sequence` ascending, including the default recent page.

Response data:

```json
{
  "items": [
    {
      "id": "item_sess_8f0e12ab34cd56ef_1",
      "sequence": 1,
      "type": "user_message",
      "role": "user",
      "content": [
        { "type": "text", "text": "hello" },
        {
          "type": "image",
          "image": {
            "media_type": "image/png",
            "size_bytes": 1024,
            "width": 640,
            "height": 480,
            "url": "/sessions/sess_8f0e12ab34cd56ef/images/0123456789abcdef0123456789abcdef.png"
          }
        }
      ],
      "created_at": "2026-01-02T03:04:05Z"
    },
    {
      "id": "item_sess_8f0e12ab34cd56ef_2",
      "sequence": 2,
      "type": "assistant_message",
      "role": "assistant",
      "content": [
        {
          "type": "tool_call",
          "tool_call": {
            "id": "call_123",
            "name": "shell",
            "arguments": "{\"command\":\"git status\"}"
          }
        },
        {
          "type": "tool_result",
          "tool_result": {
            "tool_call_id": "call_123",
            "tool_name": "shell",
            "result_type": "execution-denied",
            "reason": "User denied",
            "is_error": true
          }
        }
      ],
      "created_at": "2026-01-02T03:04:06Z"
    },
    {
      "id": "item_sess_8f0e12ab34cd56ef_3",
      "sequence": 3,
      "type": "turn_completed",
      "turn": {
        "id": "turn_123",
        "status": "completed",
        "provider_finish_reason": "stop",
        "last_agent_message": "done",
        "duration_ms": 1200
      },
      "created_at": "2026-01-02T03:04:06Z"
    }
  ],
  "next_cursor": "",
  "prev_cursor": "",
  "has_more": false
}
```

Current item types include:

- `user_message`
- `steering_message`
- `session_command`
- `assistant_message`
- `error`
- `turn_started`
- `turn_paused`
- `turn_completed`
- `turn_failed`
- `turn_interrupted`
- `approval_request`
- `approval_decision`
- `context_compacted`

`turn_paused` with `reason: "need_approval"` is not a completed turn. Desktop
should use explicit turn lifecycle items instead of inferring turn completion
from runtime state.

`approval_request` and `approval_decision` are display-log items, not command
messages. Desktop clients render approval UI from these items, but submit the
user's decision through the public approval endpoint below.

`steering_message` is a user-visible correction sent while a turn is already
running. It is separate from `user_message` so workers can consume steering
commands without replaying ordinary history messages as new input.

`session_command` records durable control requests such as pause. It is a
command request, not proof that the worker already applied the command.
Lifecycle confirmation still comes from explicit turn items such as
`turn_interrupted`.

Message content parts are zotigod display DTOs, not runtime protocol structs.
Current part types include `text`, `reasoning`, `tool_call`, and `tool_result`.
For structured parts such as `tool_call` and `tool_result`, desktop clients
should use the structured `tool_call` and `tool_result` objects for rendering,
state, filtering, and details. `text` is reserved for actual text content parts.

Old sessions that do not have a per-session display log return an empty item
list. zotigo may later add an explicit best-effort migration path, but this
endpoint does not reconstruct display history from runtime
`AgentSnapshot.History`.

Status codes:

- `200`: items returned. A known session with no display log returns an empty
  `items` array.
- `400`: invalid pagination parameters.
- `404`: session not found.
- `405`: method not allowed.

## Submit, pause, and steering

Desktop can submit a new user message, request a running session to pause the
current turn, or add steering text for the worker to apply at the next provider
interruption point. zotigod tries to make sure a worker is online before
accepting these requests, records accepted requests as durable display-log
items, and then sends a best-effort command frame over the internal worker
WebSocket.

The display-log append and WebSocket write are not a single transaction. zotigod
tries to start a missing worker before appending; after a command has been
appended, the durable command log is the recovery source of truth. Public
message, pause, and steering requests are conditionally appended against the
current display-log state. If the target turn ends before the command is durably
recorded, zotigod rejects the command instead of accepting a stale no-op. A
successful public response means the command was accepted into the durable log,
not that the worker has already applied it.

Starting a session launches an internal worker process from the current
`zotigod` executable. The worker connects back over WebSocket; connecting a
`starting` session transitions it to `running`. If zotigod launched a worker but
it does not connect before the startup timeout, the session is marked `failed`
and the start or message request returns `503`.

Worker startup also acquires the same per-session file lock used by the CLI
session manager. If another CLI, daemon worker, or local process already owns
that session lock, the worker exits instead of reusing the session concurrently.
The worker stores a per-session command cursor under `.zotigo/sessions` and uses
it to avoid replaying old accepted commands after restart. Cursor writes are
atomic rename operations. If the cursor file is corrupt, the bundled worker only
recovers past commands whose application is visible in the display log; pending
accepted commands are replayed rather than skipped.

Workers attach a live control channel by dialing:

`GET /internal/workers/connect?session_id={id}`

This is a WebSocket endpoint. zotigod keeps one active worker connection per
session ID, so multiple sessions can run concurrently on independent worker
processes. Reconnecting the same session replaces the old connection.
Connecting a `starting` session transitions it to `running`; `running` and
`paused` sessions may reconnect. `created`, `ended`, and `failed` sessions are
rejected. A worker WebSocket disconnect only removes that live connection; it
does not by itself end the session.

zotigod sends WebSocket ping frames to workers and expects pong responses. A
worker connection that stops responding is closed and unregistered, so later
public commands can relaunch or reconnect a worker instead of writing to a stale
socket. When a worker reports session finish, zotigod closes and unregisters the
live worker connection immediately.

Workers also send WebSocket ping frames to zotigod and require pong responses.
If the worker cannot write a ping or does not receive a pong before its read
deadline, it closes the WebSocket, cancels any active turn, and exits. Workers do
not continue tool or model execution after the control channel is lost. If a
display-log turn is active, the worker appends `turn_interrupted` with reason
`control_channel_closed` before closing. This prevents a desktop user from
seeing a disconnected session while tools keep running in the background.

Worker command delivery is split into a WebSocket reader and an ordered command
consumer. The reader only reads frames, handles ping/pong control traffic,
decodes commands, and enqueues them into an in-process command buffer. The
consumer processes commands sequentially and saves the command cursor after each
applied command. The command buffer is intentionally bounded at 32 items; if it
fills, the worker treats itself as unhealthy and exits instead of staying
connected but not applying pause or steering commands.

If the daemon process restarts, old workers are not treated as still live.
Stored sessions are returned as `offline` until `POST /sessions/{id}/start` or
`POST /sessions/{id}/messages` starts a new worker. Worker crash recovery is
intentionally limited in this version. Final runtime states such as `ended` or
`failed` are not persisted across daemon restarts; after restart, stored-only
sessions are reported as `offline` and can be continued by starting a new
worker. Once a worker accepts a message command and starts a turn, the command
cursor may be advanced before that turn completes. If the worker process crashes
mid-turn, zotigod does not currently reconstruct and resume that in-flight turn.
When a new bundled worker starts and finds an old open display-log turn, it
appends `turn_interrupted` with reason `worker_restarted` before accepting new
control commands.

Server-to-worker command frame:

```json
{
  "type": "command",
  "command": {
    "id": "item_sess_8f0e12ab34cd56ef_4",
    "sequence": 4,
    "type": "pause",
    "pause": {
      "turn_id": "turn_123",
      "reason": "user_pause"
    },
    "created_at": "2026-01-02T03:04:07Z"
  }
}
```

Submit a user message:

`POST /sessions/{id}/messages`

Text-only payloads remain supported:

```json
{
  "text": "Build the desktop runtime."
}
```

Messages may also include image input:

```json
{
  "text": "What is shown in this image?",
  "images": [
    {
      "mime_type": "image/png",
      "data_base64": "..."
    }
  ]
}
```

`text` is optional when `images` is non-empty. Requests must include at least one
non-empty text value or one image. `images` is optional. The first image-input
version only accepts `image/png`,
`image/jpeg`, and `image/webp`. A request may include at most 5 images; each
decoded image is capped at 5 MiB, total decoded image bytes are capped at 20
MiB, and the JSON request body is capped at 28 MiB. Invalid base64, unsupported
MIME types, and images whose decoded bytes do not match their declared MIME type
return `400`. PNG and JPEG validation decodes image config; WebP validation is
limited to basic RIFF/WebP header sniffing in this first version. Oversized
request bodies return `413`.

Accepted messages append one durable `user_message` display item with a
`command` payload of `type: "message"`. Workers consume that same item as the
command source of truth; UI clients render it as the visible user message. This
keeps the visible transcript and executable command atomic.

For image messages, the live worker command includes the image payload so the
runtime receives real `image` content parts. The display log and public
`/sessions/{id}/items` response do not persist or return full image base64.
Image bytes are stored separately as per-session blobs for command replay, and
public responses only include metadata such as `mime_type`, `size_bytes`,
`width`, `height`, and an image read `url` when available.

Historical image bytes are read through a separate public endpoint:

`GET /sessions/{id}/images/{name}`

The `{name}` value is the random image name returned in `/items` image URLs.
This endpoint does not wrap the response in JSON; it returns the original image
bytes with `Content-Type` set to `image/png`, `image/jpeg`, or `image/webp`.
The image must be recorded in zotigod's session image index when the message is
accepted. Missing sessions, unknown image names, unreferenced blob files, and
deleted blobs return `404`. This keeps `/items` small and prevents base64 image
payloads from becoming part of the transcript API.

`POST /sessions/{id}/messages` starts or resumes the session when needed, then
requires no currently open turn and no pending message command that has not yet
started a turn. If a turn is active, desktop should use
`POST /sessions/{id}/steering` instead of submitting a new message.

Response data:

```json
{
  "id": "item_sess_8f0e12ab34cd56ef_4",
  "sequence": 4,
  "type": "message",
  "text": "Build the desktop runtime.",
  "images": [
    {
      "mime_type": "image/png",
      "size_bytes": 1024,
      "width": 640,
      "height": 480,
      "url": "/sessions/sess_8f0e12ab34cd56ef/images/0123456789abcdef0123456789abcdef.png"
    }
  ],
  "created_at": "2026-01-02T03:04:07Z"
}
```

Pause current turn:

`POST /sessions/{id}/pause`

Optional request body:

```json
{
  "turn_id": "turn_123"
}
```

If `turn_id` is omitted, zotigod uses the last open display-log turn. A pause
request without an open turn is rejected. When `turn_id` is present, it must
match the open turn. An accepted pause request appends `session_command` with
`type: "pause"` and `reason: "user_pause"`. It does not mark the session
`ended`; the bundled worker applies the command and confirms the lifecycle by
appending `turn_interrupted`.

Response data:

```json
{
  "id": "item_sess_8f0e12ab34cd56ef_4",
  "sequence": 4,
  "type": "pause",
  "turn_id": "turn_123",
  "reason": "user_pause",
  "created_at": "2026-01-02T03:04:07Z"
}
```

Submit steering text:

`POST /sessions/{id}/steering`

```json
{
  "text": "Use the smaller fix and avoid changing the parser.",
  "turn_id": "turn_123"
}
```

`turn_id` is optional. When present, it must match the currently open display-log
turn. When omitted, zotigod uses the currently open turn. Steering without an
open turn is rejected; desktop should use `POST /sessions/{id}/messages` for a
new normal turn. Steering also requires the session registry state to be
`running`; paused approval sessions reject steering until the approval is
resolved and the live worker resumes. Steering v1 only supports text; requests
with `images` are rejected with `400`.

Response data:

```json
{
  "id": "item_sess_8f0e12ab34cd56ef_5",
  "sequence": 5,
  "type": "steering",
  "turn_id": "turn_123",
  "text": "Use the smaller fix and avoid changing the parser.",
  "created_at": "2026-01-02T03:04:08Z"
}
```

Workers poll commands with a display-log cursor. `after` is a sequence cursor
kept for compatibility; workers should prefer the byte `offset` cursor because
it avoids re-reading the full display log on long sessions.

`GET /internal/sessions/{id}/commands?after=0&limit=200`

or:

`GET /internal/sessions/{id}/commands?offset=0&limit=200`

Raw response:

```json
{
  "commands": [
    {
      "id": "item_sess_8f0e12ab34cd56ef_4",
      "sequence": 4,
      "type": "message",
      "message": {
        "text": "Build the desktop runtime."
      },
      "created_at": "2026-01-02T03:04:07Z"
    },
    {
      "id": "item_sess_8f0e12ab34cd56ef_5",
      "sequence": 5,
      "type": "pause",
      "pause": {
        "turn_id": "turn_123",
        "reason": "user_pause"
      },
      "created_at": "2026-01-02T03:04:08Z"
    },
    {
      "id": "item_sess_8f0e12ab34cd56ef_6",
      "sequence": 6,
      "type": "steering",
      "steering": {
        "turn_id": "turn_123",
        "text": "First correction"
      },
      "created_at": "2026-01-02T03:04:09Z"
    },
    {
      "id": "item_sess_8f0e12ab34cd56ef_7",
      "sequence": 7,
      "type": "steering",
      "steering": {
        "turn_id": "turn_123",
        "text": "Second correction"
      },
      "created_at": "2026-01-02T03:04:10Z"
    }
  ],
  "next_cursor": "7",
  "next_offset": 8123
}
```

`next_offset` is the next display-log byte offset after the complete lines that
were scanned. Workers persist both `next_offset` and the highest command
sequence they have applied, so replay can skip already-applied commands while
still advancing through the append-only log. If the log ends with a partial
line, the offset cursor stops at the last complete line and the partial line is
ignored until it is completed or truncated by a later append.

zotigod returns each accepted `steering_message` as its own command. The worker
runtime owns semantic coalescing before injecting steering into the model
context. Multiple steering commands received before the next provider request
are merged into one normal `role=user` message, appended to runtime history, and
then sent in the next provider request for that same active turn. Stale steering
commands for a completed, paused, or different turn are ignored by the worker
and are not carried into a later turn.

After applying a pause command, the bundled worker writes `turn_interrupted`
directly to the display log. The internal endpoint below exists for worker
implementations that report lifecycle confirmation over HTTP. `turn_id` is
required and must match the current open display-log turn.

`POST /internal/sessions/{id}/turn/interrupted`

```json
{
  "turn_id": "turn_123",
  "reason": "user_pause",
  "duration_ms": 1200
}
```

Response data:

```json
{
  "id": "item_sess_8f0e12ab34cd56ef_7",
  "sequence": 7,
  "type": "turn_interrupted",
  "turn": {
    "id": "turn_123",
    "status": "interrupted",
    "reason": "user_pause",
    "duration_ms": 1200
  },
  "created_at": "2026-01-02T03:04:10Z"
}
```

Status codes:

- `202`: pause command accepted.
- `201`: message or steering command created, or worker lifecycle confirmation
  appended.
- `200`: internal command list returned.
- `400`: invalid request body, invalid image input, missing `turn_id`, empty
  steering text, or invalid command query.
- `413`: message or steering request body exceeds the public API size limit.
- `404`: session not found.
- `409`: command submitted to a non-running session, message submitted during an
  active turn, turn-scoped command submitted to an offline session, pause/steering
  submitted without an active turn, or a lifecycle, pause, or steering `turn_id`
  does not match the active turn. Offline turn-scoped commands use
  `code: "session_not_live"`.
- `503`: zotigod could not start or reconnect a worker before accepting the
  command.
- `405`: method not allowed.

## Human approval flow

When a worker needs human approval, it creates an approval request through the
internal worker API. zotigod appends an `approval_request` display item as the
durable approval record, best-effort appends `turn_paused` with
`reason: "need_approval"` for display replay, and transitions the daemon session
state to `paused` when it is still running.

The persisted approval read model is the display log. zotigod reconstructs
pending and resolved approval requests from `approval_request` and
`approval_decision` items for display and worker reads. Public approval
submission still requires the session to be live in the current daemon. If the
daemon has restarted and the session is only present on disk, desktop can still
display the pending approval from `/items`, but
`POST /sessions/{id}/approvals/{approval_id}` returns `409` with
`code: "session_not_live"`.

Desktop clients using this flow must support the `paused` session state and the
`approval_request` / `approval_decision` item payloads before enabling HITL UI.

Worker create request:

`POST /internal/sessions/{id}/approvals`

```json
{
  "turn_id": "turn_123",
  "pending": [
    {
      "tool_call_id": "call_123",
      "tool_name": "shell",
      "arguments": "{\"command\":\"git status\"}",
      "description": "Run shell command",
      "reason": "requires user approval",
      "risk_level": "medium",
      "source": "classifier",
      "requires_snapshot": true
    }
  ]
}
```

Response data:

```json
{
  "id": "apr_8f0e12ab34cd56ef",
  "session_id": "sess_8f0e12ab34cd56ef",
  "turn_id": "turn_123",
  "status": "pending",
  "pending": [
    {
      "tool_call_id": "call_123",
      "tool_name": "shell",
      "arguments": "{\"command\":\"git status\"}",
      "description": "Run shell command",
      "reason": "requires user approval",
      "risk_level": "medium",
      "source": "classifier",
      "requires_snapshot": true
    }
  ],
  "created_at": "2026-01-02T03:04:05Z"
}
```

Desktop submit decision:

`POST /sessions/{id}/approvals/{approval_id}`

```json
{
  "decisions": [
    {
      "tool_call_id": "call_123",
      "approved": true
    }
  ]
}
```

Denied decisions can include a reason:

```json
{
  "decisions": [
    {
      "tool_call_id": "call_123",
      "approved": false,
      "reason": "not now"
    }
  ]
}
```

The decision request must include exactly one decision for each pending tool
call. Unknown, duplicate, missing, or missing-`approved` decisions are rejected.
After a valid decision, zotigod appends an `approval_decision` item. If the
current daemon registry still has the session in `paused`, zotigod moves it back
to `running`.

Workers can poll the internal read endpoint until the request is resolved:

`GET /internal/sessions/{id}/approvals/{approval_id}`

Status codes:

- `201`: approval request created.
- `200`: approval request returned or decision accepted.
- `400`: invalid request body or decision set.
- `404`: session or approval request not found.
- `409`: approval creation on a non-running session, or an already resolved
  approval request. Public decisions for offline sessions use
  `code: "session_not_live"`.
- `405`: method not allowed.
