# TUI and ACP on the session service

`zotigo` (TUI) and `zotigo-acp` (Agent Client Protocol over stdio) are clients of the zotigod session service. Session creation, the agent loop, tools, approvals, persistence, recovery and the event stream all run in the service, so the TUI, ACP editors, Desktop and Channels share one implementation and can attach to the same sessions. Both entrypoints speak the public HTTP API in [zotigod-api.md](zotigod-api.md) through `internal/zotigodclient`.

`core/` stays independent of the service: library users can still build an `agent.Agent` (and `core/acp.RemoteExecutor`) directly or write their own loop on `providers`/`protocol`/`tools`. Only the product entrypoints go through zotigod.

## Deployment modes

| Mode | How | Who owns running turns |
| --- | --- | --- |
| Shared daemon (default) | Connect to `ZOTIGOD_URL` (default `http://127.0.0.1:8766`, token from `ZOTIGOD_AUTH_TOKEN_FILE`). If a loopback endpoint is not listening, start `zotigod` detached (`ZOTIGOD_BINARY`, else next to the executable, else `PATH`) and wait up to 10s for `/health`. | The daemon. Turns keep running after the TUI or editor exits and can be reattached from any client. |
| Embedded (`--no-daemon --state-dir <dir>`) | `zotigod.StartEmbedded` runs the same handler, store, catalog and worker launcher in-process on a private `127.0.0.1` port with per-process tokens, with all state under `<dir>`. Workers are launched from the host executable through the hidden `__zotigod-worker` entrypoint, exactly like daemon workers. | The host process. On exit it pauses an open turn, then stops the service and its workers. |

Semantics shared by both clients:

- **No silent fallback.** If the daemon cannot be reached or started, startup fails with a hint to use `--no-daemon`; it never switches execution to an embedded service on its own. A non-loopback `ZOTIGOD_URL` is never auto-started.
- **Separate state.** `~/.zotigo` holds the sessions and the single-writer Workspace catalog of zotigod, and only zotigod uses it. `--no-daemon` therefore requires `--state-dir <dir>` (sessions, catalog, Project/Workspace directories, scratch directories, logs) and rejects `~/.zotigo` itself, including through a symlink. The host exports it as `ZOTIGO_STATE_DIR` so its session workers use the same root; an auto-started daemon never inherits it. Configuration (`config.yaml`, hooks) and user skills are still read from `~/.zotigo` and `~/.agents`. Existing sessions are reached through the daemon; `--no-daemon` is for CI, development builds and isolated experiments. Two `--no-daemon` processes on the same `--state-dir` are rejected by the catalog lock.
- **Embedded services run sessions only.** IM Channels and the Codex runtime remain daemon responsibilities.
- **Disconnects.** The event stream reconnects from the last durable sequence for up to a minute, so items are neither lost nor duplicated; streaming deltas lost during the gap are replaced by their durable items. After that the client reports the stream as ended. With the daemon, work continues regardless of the client.

## Session ownership and cwd

A new session uses the caller's directory as `cwd` without copying anything or creating Project/Workspace structure. If that directory is inside a Workspace, the service binds the session to it and keeps the subdirectory as `cwd`; otherwise it is an ordinary directory session with the same history, resume, approval and event-stream behavior. `--workspace <id>` binds explicitly and requires the cwd to be inside that Workspace. See "Create sessions" in the API document for archived and ambiguous owners.

## TUI

```sh
zotigo                         # new session in this directory (daemon)
zotigo --no-daemon --state-dir /tmp/zt   # isolated in-process service
zotigo --workspace <id>        # bind the new session to a Workspace
zotigo --resume | -r           # resume a session whose cwd is this directory
zotigo --resume-all            # resume across directories and Workspaces
zotigo --dangerously-skip-permissions   # Full access
```

- Enter while a turn runs adds the input to that turn (steering). Esc interrupts the running turn; Esc or Ctrl+C while idle exits. "Running" includes an input of this TUI whose turn the event stream has not reported yet, or whose submission could not be confirmed, so neither key treats that state as idle. Ctrl+C while a daemon turn runs asks: stop the turn and exit (a pending approval is denied with `stop_turn`), keep it running in the background and exit (with a `zotigo --resume` hint), or cancel. With `--no-daemon` the turn always stops.
- Stopping (Esc, or "stop and exit") uses `zotigodclient.StopTurn`, shared with the ACP adapter. A client's view lags the service, so it targets the turn the client saw plus the turn its own just-submitted input started or joined. Submission outcomes are recorded per input (in flight, accepted or possibly accepted, rejected) and read before each history snapshot: an input still in flight is waited for, a rejected one is ignored, and an accepted one is located in the history (paging past the recent window if needed). An accepted input that cannot be located is never taken as "nothing is running"; the stop keeps looking until its timeout and then fails. Turns already located are stopped meanwhile.
- Submissions go through `zotigodclient.SubmitTracked`. Only a 4xx on the first attempt is a rejection. A transport error or 5xx leaves the outcome unknown, so the input is resubmitted with the same `client_message_id` (deduplicated by the service) for a few seconds; if it still cannot be confirmed, the TUI warns instead of reporting "not accepted" and ACP fails the prompt with a "may still be running" error. Either way the input stays a stop target: Ctrl+C still offers to stop it, and ACP `session/cancel` and disconnect still stop the turn it started. A pending approval is denied with `stop_turn`; a `409` from that denial only means another client resolved the approval (possibly approving it), so the session is re-read and the turn paused. "Stop and exit" exits only after the service accepted the stop or the turn is known to have ended; otherwise the TUI stays attached and reports the error.
- Approvals come from the session's durable `approval_request` items, including subagent (spawn) approvals. Accept/Deny/Feedback work per call. A plain deny stops the turn together with the denial (`stop_turn`); a deny with feedback lets the agent continue. A decision made by another client closes the prompt.
- Shift+Tab cycles the approval policy Manual → Auto → Full access while idle.
- `/clear` starts a new session in the same directory (the old one stays resumable); `/compress` sends `/compact` to the service; `/model` lists and switches profiles; `/cost` and `/stats` use completed-turn usage and the service's context estimate.
- The resume picker lists sessions from `/catalog/sessions`; `d` archives instead of deleting history. Codex-backed sessions are listed but must be opened in Desktop.

## ACP

```sh
zotigo-acp                     # daemon mode
zotigo-acp --no-daemon --state-dir <dir>   # embedded service
zotigo-acp --workspace <id>    # bind new sessions to a Workspace
```

| ACP | Service |
| --- | --- |
| `session/new {cwd}` | `POST /sessions` with the client cwd (must be absolute and exist; never replaced) and `approval_policy: manual`. ACP `sessionId` is the zotigod session ID. The response advertises modes. |
| Session modes | `manual` (Manual), `auto` (Auto), `bypass_permissions` (Full access) are the session approval policy. `session/set_mode` calls `PUT /approval-policy` (idle sessions only); `approval_policy_changed` items, including changes from other clients, are sent as `current_mode_update`. |
| `session/load {sessionId, cwd}` | Rejected unless cwd equals the session's cwd (and, with `--workspace`, the session belongs to it). Replays history as `user_message_chunk`/`agent_message_chunk`/`agent_thought_chunk`/`tool_call` updates, then follows the live stream. A live pending approval is asked again after the reply. |
| `session/prompt` | `POST /messages` (start-or-steer) and wait for the turn that consumes the input. `end_turn`, `max_tokens`, `refusal` or `cancelled` map from the turn's terminal item; a failed turn is reported as an error message. |
| `session/cancel` | `POST /pause` for the open turn; if the turn has not started yet it is paused when it starts. During a permission request the client answers `cancelled`, which is sent as a denial with `stop_turn`. |
| `session/request_permission` | One request per pending call with Allow/Reject. Errors from the client deny (fail closed). If another client decided first, the late decision is discarded by the service (`409`). |
| `session/list` | `/catalog/sessions`, filtered by cwd and the bound Workspace. |
| Inputs from other clients | Forwarded as `user_message_chunk`. |

Tools run in the session runtime on the service host with the session cwd. The adapter never relies on the client's `fs/*` or `terminal/*` methods, so clients that do not implement them work; the trade-off is that tools see files on disk, not unsaved editor buffers. Prompt `resource_link` blocks become Markdown links in the message; other unadvertised block types are rejected. Client-provided MCP servers are not supported yet: the first response in that session states which servers were not loaded.

ACP sessions start in Manual because editors confirm tool calls in their own UI; Desktop and the TUI default to Auto.

**Disconnects.** ACP has no background mode. When the editor disconnects, the adapter stops every turn one of its prompts was waiting on: Pause, or for a pending approval a denial with `stop_turn`. Turns started by other clients of the shared daemon keep running, and their pending approvals stay pending for those clients instead of being denied. With `--no-daemon` every open turn stops because the service exits.
