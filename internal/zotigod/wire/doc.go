// Package wire defines the protocol between the zotigod daemon and the worker
// processes it launches: the WebSocket frame and its payloads, the commands
// the daemon delivers, and the bodies and headers of the worker's HTTP calls
// to the /internal/* routes.
//
// It holds data only. Handlers, I/O and anything that depends on daemon or
// worker state stay in the packages that use these types.
//
// Compatibility rule: JSON tags, field order, constant values and header
// names here are a contract between two processes. During an update the
// daemon and a running worker can be different builds, so none of them may
// change without a migration or a version check. wire_test.go pins the
// format.
package wire
