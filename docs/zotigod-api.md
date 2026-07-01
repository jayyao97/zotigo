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
- `POST /sessions/{id}/approvals/{approval_id}`

Internal worker endpoints under `/internal/sessions/...` are not public desktop
API and may change without compatibility guarantees.

Current internal worker endpoints include:

- `POST /internal/sessions/{id}/worker/attach`
- `POST /internal/sessions/{id}/worker/finish`
- `POST /internal/sessions/{id}/approvals`
- `GET /internal/sessions/{id}/approvals/{approval_id}`

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

Response:

```json
{
  "items": [
    {
      "id": "item_sess-1_1",
      "sequence": 1,
      "type": "user_message",
      "role": "user",
      "content": [{ "type": "text", "text": "hello" }],
      "created_at": "2026-01-02T03:04:05Z"
    },
    {
      "id": "item_sess-1_2",
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
      "id": "item_sess-1_3",
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

## Human approval flow

When a worker needs human approval, it creates an approval request through the
internal worker API. zotigod appends an `approval_request` display item followed
by `turn_paused` with `reason: "need_approval"`, and transitions the daemon
session state to `paused`.

The persisted approval source of truth is the display log. zotigod reconstructs
pending and resolved approval requests from `approval_request` and
`approval_decision` items, so public approval submission can continue after a
zotigod restart as long as the session display log is still present.

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

Response:

```json
{
  "id": "apr_8f0e12ab34cd56ef",
  "session_id": "sess-1",
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
After a valid decision, zotigod appends an `approval_decision` item and moves
the daemon session back to `running` when the session is still present in the
current daemon registry. If zotigod restarted and the session is only known from
the display log, the durable decision is still accepted and returned; there is no
in-memory daemon session state to transition.

Workers can poll the internal read endpoint until the request is resolved:

`GET /internal/sessions/{id}/approvals/{approval_id}`

Status codes:

- `201`: approval request created.
- `200`: approval request returned or decision accepted.
- `400`: invalid request body or decision set.
- `404`: session or approval request not found.
- `409`: invalid lifecycle state or already resolved approval request.
- `405`: method not allowed.
