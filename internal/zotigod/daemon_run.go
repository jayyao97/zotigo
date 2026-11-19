package zotigod

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bytedance/sonic"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/internal/buildinfo"
	"github.com/jayyao97/zotigo/internal/channels"
	channelsfeishu "github.com/jayyao97/zotigo/internal/channels/feishu"
	"github.com/jayyao97/zotigo/internal/codexapp"
	"github.com/jayyao97/zotigo/internal/diagnostics"
	"github.com/jayyao97/zotigo/internal/hooks"
	"github.com/jayyao97/zotigo/internal/update"
	"github.com/jayyao97/zotigo/internal/zotigod/codexworker"
	"github.com/jayyao97/zotigo/internal/zotigod/worker"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

// Run starts zotigod and returns a process exit code.
func Run(args []string) (exitCode int) {
	if len(args) > 0 && args[0] == "catalog-migrate" {
		return runCatalogMigrate(args[1:], os.Stdout, os.Stderr)
	}
	fs := flag.NewFlagSet("zotigod", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	versionFlag := fs.Bool("version", false, "Print program release version")

	addr := fs.String("addr", defaultAddr, "Address to listen on")
	authTokenFile := fs.String("auth-token-file", "", "File containing the bearer token required by public APIs")
	workerCallbackURL := fs.String("worker-daemon-url", "", "Daemon URL used by locally spawned workers")
	workerMode := fs.Bool("worker", false, "Run an internal zotigod worker")
	workerDaemonURL := fs.String("daemon-url", "", "zotigod daemon URL for internal worker mode")
	workerSessionID := fs.String("session-id", "", "zotigod session id for internal worker mode")
	workerActivation := fs.Uint64("worker-activation", 0, "zotigod worker activation for internal worker mode")
	sessionStoreRoot := fs.String("session-store-root", "", "Session store root for internal worker mode")
	codexWorkerMode := fs.Bool("codex-worker", false, "Run an internal Codex bridge worker")
	codexSocket := fs.String("codex-socket", "", "Codex app-server Unix socket")
	codexWorkingDirectory := fs.String("codex-working-directory", "", "Codex worker working directory")
	codexModel := fs.String("codex-model", "", "Codex model")
	codexReasoningEffort := fs.String("codex-reasoning-effort", "", "Codex reasoning effort")
	codexApprovalPolicy := fs.String("codex-approval-policy", "", "Codex approval policy")
	codexThreadID := fs.String("codex-thread-id", "", "Existing Codex thread id")
	if err := fs.Parse(args); err != nil {
		stopLogging := diagnostics.Start("daemon")
		log.Printf("argument_error: %v", err)
		stopLogging()
		return 2
	}
	if *versionFlag {
		fmt.Printf("zotigod %s\n", buildinfo.Version)
		return 0
	}
	stopLogging := diagnostics.Start("daemon")
	defer stopLogging()
	log.Printf("daemon_start version=%s pid=%d", buildinfo.Version, os.Getpid())
	defer func() {
		if value := recover(); value != nil {
			log.Printf("daemon_panic: %v", value)
			panic(value)
		}
		log.Printf("daemon_exit code=%d", exitCode)
	}()
	if *workerMode {
		daemonURL := *workerDaemonURL
		if daemonURL == "" {
			daemonURL = "http://" + defaultAddr
		}
		workerAuthToken := os.Getenv(workerAuthTokenEnv)
		_ = os.Unsetenv(workerAuthTokenEnv)
		if err := worker.Run(context.Background(), workerbase.Config{
			DaemonURL:  daemonURL,
			SessionID:  *workerSessionID,
			Activation: *workerActivation,
			AuthToken:  workerAuthToken,
		}); err != nil {
			log.Printf("zotigod worker failed: %v", err)
			return 1
		}
		return 0
	}
	if *codexWorkerMode {
		daemonURL := *workerDaemonURL
		if daemonURL == "" {
			daemonURL = "http://" + defaultAddr
		}
		workerAuthToken := os.Getenv(workerAuthTokenEnv)
		_ = os.Unsetenv(workerAuthTokenEnv)
		if err := codexworker.Run(context.Background(), codexworker.Config{
			Config:           workerbase.Config{DaemonURL: daemonURL, SessionID: *workerSessionID, Activation: *workerActivation, AuthToken: workerAuthToken},
			SocketPath:       *codexSocket,
			WorkingDirectory: *codexWorkingDirectory, Model: *codexModel,
			ReasoningEffort: *codexReasoningEffort, ApprovalPolicy: *codexApprovalPolicy, ThreadID: *codexThreadID,
			SessionStoreRoot: *sessionStoreRoot,
		}); err != nil {
			log.Printf("zotigod Codex worker failed: %v", err)
			return 1
		}
		return 0
	}

	logger := log.New(log.Writer(), "[zotigod] ", log.LstdFlags)
	var publicAuthToken string
	if strings.TrimSpace(*authTokenFile) != "" {
		var err error
		publicAuthToken, err = loadAuthToken(*authTokenFile)
		if err != nil {
			logger.Printf("Authentication configuration failed: %v", err)
			return 1
		}
	}
	if listenAddressNeedsAuth(*addr) && publicAuthToken == "" {
		logger.Printf("Authentication configuration failed: non-loopback listen address requires --auth-token-file")
		return 1
	}
	listener, listenErr := net.Listen("tcp", *addr)
	if listenErr != nil {
		if errors.Is(listenErr, syscall.EADDRINUSE) {
			compatible, healthErr := compatibleZotigodAt(context.Background(), *addr)
			if compatible {
				logger.Printf("Compatible zotigod already listening on http://%s; reusing it", *addr)
				return 0
			}
			logger.Printf("daemon_port_occupied: listen address %s: 该端口上的服务不是 zotigod (health check: %v)", *addr, healthErr)
			return 1
		}
	}
	workerAuthToken, err := generateAuthToken()
	if err != nil {
		logger.Printf("Worker authentication initialization failed: %v", err)
		return 1
	}
	workerCallback, err := resolveWorkerDaemonURL(*addr, *workerCallbackURL)
	if err != nil {
		logger.Printf("Worker daemon URL configuration failed: %v", err)
		return 1
	}
	configPath, created, err := config.NewManager().EnsureGlobalConfig()
	if err != nil {
		logger.Printf("Config initialization failed: %v", err)
		return 1
	}
	if created {
		logger.Printf("Created default config template at %s. Add a profile, set default_profile, and configure its API key before creating sessions.", configPath)
	}
	if listenErr != nil {
		logger.Printf("Listening on http://%s", *addr)
		logger.Printf("Server failed: %v", listenErr)
		return 1
	}
	defer func() { _ = listener.Close() }()
	hookDispatcher, hookErr := hooks.LoadDefault(logger)
	if hookErr != nil {
		logger.Printf("Hook configuration failed; hooks disabled: %v", hookErr)
	}
	if hookDispatcher != nil {
		defer workerbase.CloseHookDispatcher(hookDispatcher)
	}
	launcher, err := newProcessWorkerLauncher(workerCallback, workerAuthToken, logger)
	if err != nil {
		logger.Printf("Worker launcher disabled: %v", err)
	}
	if launcher != nil {
		defer launcher.Close()
	}
	runtimes := newRuntimeRegistry(nativeRuntimeAdapter{launcher: launcher})
	var codexHost codexapp.HostProvider
	binaryPath, _, codexErr := codexapp.Discover()
	if codexErr == nil && launcher == nil {
		codexErr = errors.New("worker launcher is unavailable")
	}
	if codexErr == nil {
		var cacheDir string
		cacheDir, codexErr = os.UserCacheDir()
		if codexErr == nil {
			var host *codexapp.Host
			host, codexErr = codexapp.NewHost(binaryPath, filepath.Join(cacheDir, "zotigod", "runtime"), log.Writer(), codexapp.HostOptions{StopWhenIdle: true})
			if codexErr == nil {
				codexHost = host
				defer func() { _ = host.Close() }()
				runtimes = newRuntimeRegistry(nativeRuntimeAdapter{launcher: launcher}, codexapp.NewAdapter(host, launcher.StartCodex))
			}
		}
	}
	if codexErr != nil {
		logger.Printf("Codex runtime unavailable: %v", codexErr)
	}
	server := &http.Server{
		Addr:              *addr,
		Handler:           nil,
		ReadHeaderTimeout: 5 * time.Second,
	}
	channelStore, channelStoreErr := channels.Open("")
	var channelService *channels.Service
	if channelStoreErr != nil {
		logger.Printf("Channels disabled: %v", channelStoreErr)
	} else if secretStore, secretErr := channels.NewSecretStore(""); secretErr != nil {
		_ = channelStore.Close()
		logger.Printf("Channels disabled: %v", secretErr)
	} else {
		channelService = channels.NewService(channelStore, secretStore, logger, map[string]channels.AdapterFactory{channels.ProviderFeishu: channelsfeishu.Factory{}})
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var updater *update.Manager
	if publicAuthToken != "" {
		if executable, err := os.Executable(); err == nil {
			updater, _ = update.ForExecutable(executable, "daemon")
		}
	}
	server.Handler = newDefaultHandler(handlerOptions{
		callbackBaseURL:  workerCallback,
		updater:          updater,
		serviceContext:   ctx,
		launcher:         launcher,
		runtimes:         runtimes,
		codexHost:        codexHost,
		codexUnavailable: codexErr,
		publicAuthToken:  publicAuthToken,
		workerAuthToken:  workerAuthToken,
		logger:           logger,
		hooks:            hookDispatcher,
		channels:         channelService,
	})

	var closeChannelsOnce sync.Once
	closeChannels := func() {
		closeChannelsOnce.Do(func() {
			if channelService == nil {
				return
			}
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := channelService.Close(shutdownCtx); err != nil {
				logger.Printf("Channels shutdown failed: %v", err)
			}
		})
	}
	defer closeChannels()
	if channelService != nil {
		if err := channelService.Start(ctx); err != nil {
			logger.Printf("Channels start failed: %v", err)
		}
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Printf("Listening on http://%s", *addr)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Printf("Shutdown failed: %v", err)
			return 1
		}
		closeChannels()
		if err := <-errCh; err != nil {
			logger.Printf("Server failed: %v", err)
			return 1
		}
		return 0
	case err := <-errCh:
		if err != nil {
			logger.Printf("Server failed: %v", err)
			return 1
		}
		return 0
	}
}

func compatibleZotigodAt(ctx context.Context, addr string) (bool, error) {
	baseURL, err := resolveWorkerDaemonURL(addr, "")
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil)
	if err != nil {
		return false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("health returned %s", resp.Status)
	}
	var envelope struct {
		Code string `json:"code"`
		Data struct {
			Status          string `json:"status"`
			ProtocolVersion string `json:"protocol_version"`
		} `json:"data"`
	}
	if err := sonic.ConfigDefault.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&envelope); err != nil {
		return false, fmt.Errorf("decode health response: %w", err)
	}
	if envelope.Code != "ok" || envelope.Data.Status != "ok" || envelope.Data.ProtocolVersion != apiProtocolVersion {
		return false, fmt.Errorf("incompatible health response")
	}
	return true, nil
}
