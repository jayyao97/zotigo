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

Internal worker endpoints under `/internal/sessions/...` are not public desktop
API and may change without compatibility guarantees.

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
          "summary": "Shell(git status)",
          "tool_call": {
            "id": "call_123",
            "name": "shell",
            "arguments": "{\"command\":\"git status\"}"
          }
        },
        {
          "type": "tool_result",
          "summary": "Denied: User denied",
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
- `context_compacted`

`turn_paused` with `reason: "need_approval"` is not a completed turn. Desktop
should use explicit turn lifecycle items instead of inferring turn completion
from runtime state.

Message content parts are zotigod display DTOs, not runtime protocol structs.
Current part types include `text`, `reasoning`, `tool_call`, and `tool_result`.
For structured parts such as `tool_call` and `tool_result`, `summary` is a lossy
display projection only; desktop clients should use the structured `tool_call`
and `tool_result` objects for state, filtering, and detailed rendering. `text`
is reserved for actual text content parts.

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
