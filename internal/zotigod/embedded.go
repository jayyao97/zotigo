package zotigod

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
	"github.com/jayyao97/zotigo/internal/hooks"
)

// WorkerSubcommand is the hidden first argument that host binaries embedding
// the session service (zotigo and zotigo-acp with --no-daemon) must route to
// Run. The embedded service launches session workers from the host
// executable, so the host needs this entrypoint to reach worker mode.
const WorkerSubcommand = "__zotigod-worker"

// EmbeddedService is the zotigod session service owned by one host process.
// It is the same handler, store, catalog, worker launcher and recovery logic
// as the shared daemon; only the deployment differs:
//
//   - it listens on a private 127.0.0.1 port with a per-process bearer token,
//     because session workers are separate processes that dial back exactly
//     as they do with the shared daemon;
//   - it does not run IM channels or the Codex runtime, which are long-lived
//     daemon responsibilities;
//   - Close stops every worker it launched, so work does not outlive the host.
//
// Its state root comes from config.StateDir. Hosts set an isolated root
// (zotigo --no-daemon --state-dir) so the embedded service never shares
// sessions or the single-writer Workspace catalog with zotigod. A second
// embedded service on the same root fails instead of running degraded.
type EmbeddedService struct {
	BaseURL string
	Token   string

	server   *http.Server
	served   chan struct{}
	cancel   context.CancelFunc
	launcher *processWorkerLauncher
	hooks    *hooks.Dispatcher
	store    *zotigosession.FileStore
	catalog  *zotigoworkspace.Store
}

// StartEmbedded starts an in-process session service. logger receives
// service and worker output; it must not write to a terminal owned by a TUI.
func StartEmbedded(logger *log.Logger) (*EmbeddedService, error) {
	if logger == nil {
		logger = log.New(log.Writer(), "[zotigod-embedded] ", log.LstdFlags)
	}
	store, err := zotigosession.NewFileStore("")
	if err != nil {
		return nil, fmt.Errorf("open session store: %w", err)
	}
	catalog, err := zotigoworkspace.Open(store.RootDir())
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("open workspace catalog in %s (is another --no-daemon process using this --state-dir?): %w", store.RootDir(), err)
	}
	service := &EmbeddedService{store: store, catalog: catalog}
	if err := catalog.RecoverQueuedInputs(context.Background()); err != nil {
		service.closeStores()
		return nil, fmt.Errorf("recover queued inputs: %w", err)
	}
	publicToken, err := generateAuthToken()
	if err != nil {
		service.closeStores()
		return nil, err
	}
	workerToken, err := generateAuthToken()
	if err != nil {
		service.closeStores()
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		service.closeStores()
		return nil, fmt.Errorf("listen for embedded session service: %w", err)
	}
	service.BaseURL = "http://" + listener.Addr().String()
	service.Token = publicToken
	launcher, err := newProcessWorkerLauncher(service.BaseURL, workerToken, logger)
	if err != nil {
		_ = listener.Close()
		service.closeStores()
		return nil, err
	}
	launcher.argsPrefix = []string{WorkerSubcommand}
	service.launcher = launcher

	options := handlerOptions{
		launcher:        launcher,
		runtimes:        newRuntimeRegistry(nativeRuntimeAdapter{launcher: launcher}),
		store:           store,
		catalog:         catalog,
		publicAuthToken: publicToken,
		workerAuthToken: workerToken,
		logger:          logger,
	}
	dispatcher, hookErr := hooks.LoadDefault(logger)
	if hookErr != nil {
		logger.Printf("Hook configuration failed; hooks disabled: %v", hookErr)
	}
	if dispatcher != nil {
		service.hooks = dispatcher
		options.hooks = dispatcher
	}
	ctx, cancel := context.WithCancel(context.Background())
	service.cancel = cancel
	options.maintenanceContext = ctx
	go maintainConversationIndex(ctx, store, logger)

	service.server = &http.Server{
		Handler:           newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, options),
		ReadHeaderTimeout: 5 * time.Second,
		// Request contexts derive from ctx so Close also ends long-lived
		// event streams, which http.Server.Shutdown would otherwise wait on.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	service.served = make(chan struct{})
	go func() {
		defer close(service.served)
		if err := service.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("embedded session service stopped: %v", err)
		}
	}()
	return service, nil
}

// Close stops the service and every worker it launched. A turn still running
// in a worker is killed with it; the next start of that session records the
// open turn as interrupted through the normal worker recovery path.
func (s *EmbeddedService) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.cancel()
	err := s.server.Shutdown(ctx)
	if err != nil {
		_ = s.server.Close()
	}
	<-s.served
	s.launcher.Close()
	closeHookDispatcher(s.hooks)
	s.closeStores()
	return err
}

func (s *EmbeddedService) closeStores() {
	if s.catalog != nil {
		_ = s.catalog.Close()
	}
	if s.store != nil {
		_ = s.store.Close()
	}
}
