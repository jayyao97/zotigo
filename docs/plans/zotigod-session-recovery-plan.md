# Zotigod Session Recovery Plan

## Background

Desktop can already show old conversation history through `GET /sessions/{id}/items`. That history comes from the durable display log on disk.

The missing piece is continuing an old session after `zotigod` has restarted. After a daemon restart, the in-memory session registry is empty and any old worker process is gone. The session files still exist on disk, but there is no live worker behind them.

We should not solve this by pretending stored sessions are `running`. That would make desktop think a session can accept pause, steering, or approval actions even though no worker is connected. The safer behavior is to show stored sessions as readable but offline, and only start a worker when the user performs an action that clearly means “continue this session”.

## Terms

- **Display log**: the durable transcript/read model used by CLI and desktop to replay what the user saw.
- **Session store**: the on-disk session data under `.zotigo/sessions`, including metadata, agent snapshot, and display log.
- **Daemon registry**: zotigod's in-memory list of sessions known to the current daemon process.
- **Worker**: the per-session process that owns the restored agent runtime and executes turns.
- **Live session**: a session that exists in the current daemon registry and has, or is starting, a worker.
- **Offline session**: a session that exists on disk but is not live in the current daemon process.
- **AgentSnapshot**: the runtime context used to continue model/tool execution. It is not the same thing as the display log.
- **Open turn**: a turn in the display log that started but has not been completed, failed, or interrupted.

## User-Visible Behavior

Reading session lists or history should never start background workers. Desktop may open a session list, switch between old sessions, and render history without causing new processes to appear.

Sending a new message is different. When the user sends a message to an offline session, that is an explicit request to continue the conversation. zotigod may load the stored session, start a worker, restore the agent snapshot, and then submit the message.

`POST /sessions/{id}/start` remains available for explicit runtime resume or pre-warming, but desktop's normal chat flow should not need it. In normal use, `POST /sessions/{id}/messages` can do the start/resume work internally before appending the message.

Pause, steering, and approval decisions are different from a new message. They refer to a currently running turn or a live pending approval. If the session is offline, zotigod should reject those actions clearly instead of trying to recover old tool execution.

## Target Semantics

### Read APIs Do Not Resume

These endpoints are read-only:

- `GET /sessions`
- `GET /sessions/{id}`
- `GET /sessions/{id}/items`

They may read from the daemon registry and the session store, but they must not launch a worker or mutate runtime state.

### Stored Sessions Are Offline

If a session exists in the store but not in the daemon registry, zotigod returns it as an offline session:

```json
{
  "id": "sess_123",
  "state": "offline",
  "live": false,
  "working_directory": "/Users/me/project"
}
```

Registry sessions return `live: true`. Sessions that exist only on disk return `live: false`.

Do not mark an offline session as `running`; that would imply a connected worker that does not exist.

### Message Auto-Resumes

`POST /sessions/{id}/messages` should behave as:

```text
ensure session is running
reload display log
check that a new message can be accepted
append user_message command
send command to worker
```

If the session is stored but not live, message submission may load the stored session into daemon memory, launch a worker, wait for it to connect, and then append the message command.

The worker already restores `AgentSnapshot` from the stored session. On startup it also interrupts any old open display-log turn with `reason: "worker_restarted"`. Because of that, the message handler must reload display items after the worker is running before it checks whether a new message can be accepted.

### Start Is Explicit Runtime Resume

`POST /sessions/{id}/start` remains useful, but desktop's normal chat flow does not need to call it.

`start` means:

- make this session live
- launch or reconnect a worker
- do not append a user message

`messages` means:

- ensure the session is live
- append and deliver a new user message

Implementation should share an internal helper instead of doing HTTP calls between handlers:

```go
func (h *handler) ensureSessionRunning(ctx context.Context, id string) (Session, error)
```

### No Auto-Resume For Turn-Scoped Commands

These operations require a currently live runtime state and should not auto-resume offline sessions:

- `POST /sessions/{id}/pause`
- `POST /sessions/{id}/steering`
- `POST /sessions/{id}/approvals/{approval_id}`

For offline sessions they should return a structured conflict, for example:

```json
{
  "code": "session_not_live",
  "message": "session is offline; send a message to resume it"
}
```

Do not try to recover an old pending tool execution after daemon or worker restart in this phase.

## Implementation Plan

### Step 1: Session Response Liveness

Add public response fields:

```go
State SessionState `json:"state"`
Live  bool         `json:"live"`
```

Add `SessionStateOffline = "offline"` for sessions that exist on disk but are not live.

Registry sessions should return `live: true`. Disk-only sessions should return `state: "offline", live: false`.

### Step 2: Read From Disk When Not In Memory

Update:

- `GET /sessions/{id}`
- `GET /sessions`

Behavior:

- registry wins when both registry and disk contain the same session id
- disk-only sessions are returned as offline
- missing from both returns `404`

`GET /sessions/{id}/items` already supports stored sessions and should remain read-only.

### Step 3: Load Stored Session Helper

Add a helper that loads stored metadata into the daemon registry without marking it running:

```go
func (h *handler) loadStoredSession(ctx context.Context, id string) (Session, bool, error)
```

It should:

- read `core/session.Session` from the session store
- preserve id, created time, and working directory
- insert into the registry as `created` or `starting` depending on caller need
- not launch a worker by itself

### Step 4: Shared Ensure-Running Path

Refactor `POST /sessions/{id}/start` to use:

```go
func (h *handler) ensureSessionRunning(ctx context.Context, id string) (Session, error)
```

Rules:

- `running` returns as-is
- `created` transitions to `starting`, launches worker, waits for `running`
- missing from registry but present on disk loads the stored session and starts it
- ended/failed returns conflict
- missing from both returns not found

The helper should use existing worker launch and wait behavior.

### Step 5: Message Auto-Resume

Update `POST /sessions/{id}/messages` to call `ensureSessionRunning` before checking whether the message can be accepted.

After `ensureSessionRunning` returns, reload display items before checking:

- last open turn
- pending message command
- pending approval, if applicable

This lets the restarted worker append `turn_interrupted(worker_restarted)` before the message acceptance checks run.

### Step 6: Offline Errors For Turn-Scoped Operations

Update pause, steering, and public approval decision paths so offline sessions fail clearly instead of looking missing or running.

Suggested error code:

```text
session_not_live
```

This may require extending `writeAPIError` to accept an explicit code for cases where HTTP status alone is too coarse.

### Step 7: Documentation

Update `docs/zotigod-api.md`:

- define `live`
- define `offline`
- explain read APIs never resume
- explain `messages` auto-resumes
- explain `start` is optional explicit runtime resume
- explain pause/steering/approval do not auto-resume

## Tests

Add handler tests for:

- `GET /sessions/{id}` returns disk-only session as `offline/live=false`
- `GET /sessions` merges registry and disk sessions without duplicates
- read endpoints do not call the worker launcher
- `POST /sessions/{id}/start` resumes a disk-only session
- `POST /sessions/{id}/messages` auto-resumes a disk-only session and sends the message command
- message acceptance checks reload display log after resume
- pause and steering reject offline sessions with `session_not_live`
- missing session still returns `not_found`

Run:

```bash
go test ./internal/zotigod ./core/session -count=1
go build ./...
make check
```

## Non-Goals

- Do not start workers from read APIs.
- Do not load every stored session into the live registry at daemon startup.
- Do not resume an in-flight turn after process restart.
- Do not recover pending tool execution after approval restart.
- Do not add full worker crash restart/backoff policy in this phase.
- Do not change display log storage format.
