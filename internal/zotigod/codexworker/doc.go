// Package codexworker is the Codex bridge worker process. The daemon starts one
// per Codex-backed session. It connects to the daemon through workerbase and to
// a Codex app server, and translates Codex threads, turns and items into the
// session log. It also holds the Codex thread reading and conversion the daemon
// uses for history sync and fork. It must not import the daemon.
package codexworker
