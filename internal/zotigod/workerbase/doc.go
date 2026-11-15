// Package workerbase is what every worker process is built on: the daemon
// connection and its writer, command fetch/cursor recovery, readiness/finish
// reporting, lifecycle hooks, session tool specs, and policy/profile
// persistence.
//
// Both worker kinds (the native worker and the Codex bridge worker) use it,
// and the daemon uses the few pieces it shares with them. It imports
// zotigod/wire and zotigod/sessionlog, never zotigod.
package workerbase
