package zotigod

import (
	"context"
	"net/http"
)

// newTestHandler builds a handler the way the daemon does, over the default
// stores under the test HOME that TestMain isolates.
func newTestHandler() http.Handler {
	return newDefaultHandler(handlerOptions{})
}

type workerLauncherFunc func(ctx context.Context, sessionID string, workingDirectory string) error

func (fn workerLauncherFunc) Start(ctx context.Context, sessionID string, workingDirectory string) error {
	return fn(ctx, sessionID, workingDirectory)
}
