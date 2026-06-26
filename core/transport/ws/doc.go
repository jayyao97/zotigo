// Package ws implements transport.Transport over an established WebSocket
// connection.
//
// This package owns message framing and per-write deadlines only. The caller
// owns dialing, listening, authentication, session routing, and long-lived
// connection liveness policy such as read deadlines and ping/pong keepalive.
package ws
