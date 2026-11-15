// Package worker is the native worker process. The daemon starts one per
// session. It connects to the daemon through workerbase, builds the agent
// runtime, applies the commands the daemon delivers and writes the session
// log. It must not import the daemon.
package worker
