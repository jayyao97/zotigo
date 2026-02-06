package transport

import "errors"

var (
	// ErrTransportClosed is returned when attempting to use a closed transport
	ErrTransportClosed = errors.New("transport is closed")

	// ErrApprovalTimeout is returned when approval request times out
	ErrApprovalTimeout = errors.New("approval request timed out")

	// ErrApprovalDenied is returned when all tool calls are denied
	ErrApprovalDenied = errors.New("all tool calls denied")
)
