package transport

import "errors"

// ErrTransportClosed is returned when attempting to use a closed transport.
var ErrTransportClosed = errors.New("transport is closed")
