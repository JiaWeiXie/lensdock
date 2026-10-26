package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/capture"
	"github.com/JiaWeiXie/lensdock/internal/cdp"
	"github.com/JiaWeiXie/lensdock/internal/clipboard"
	"github.com/JiaWeiXie/lensdock/internal/lease"
	"github.com/JiaWeiXie/lensdock/internal/runtimeapi"
	"github.com/JiaWeiXie/lensdock/internal/supervisor"
	"github.com/JiaWeiXie/lensdock/internal/transfer"
	"github.com/JiaWeiXie/lensdock/internal/vnc"
)

const (
	chromiumDevToolsURL  = "http://127.0.0.1:9223"
	chromiumDevToolsAddr = "127.0.0.1:9223"
	downloadsDir         = "/data/downloads"
	serverShutdownGrace  = 5 * time.Second
)

// superviseCommand is the container entrypoint. It starts as root, prepares
// the runtime directories and loads the TLS credentials, drops privileges,
// serves the API (so readiness probes answer while the browser stack starts)
// and runs the supervisor until a signal or a fatal unit failure.
func superviseCommand(modeFlag string) error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := supervisor.LoadConfig(os.Getenv)
	if err != nil {
		return err
	}
	// The flag overrides the environment; both are only the initial mode,
	// used when no policy has been saved yet.
	if modeFlag != "" {
		mode, err := apiwire.ParseAgentMode(modeFlag)
		if err != nil {
			return fmt.Errorf("--mode: %w", err)
		}
		cfg.AgentMode = mode
	}
	// Credentials are read while still root; the unprivileged API process
	// never needs access to the key file.
	tlsConf, err := buildTLS(cfg.TLS)
	if err != nil {
		return err
	}
	if err := supervisor.PrepareRoot(); err != nil {
		return fmt.Errorf("prepare runtime root: %w", err)
	}
	// The policy directory is a supervisor-private volume; it is prepared
	// while still root and never falls back to a browser-accessible path.
	if err := supervisor.PreparePolicyDir(cfg.PolicyDir); err != nil {
		return err
	}
	if err := supervisor.DropPrivileges(); err != nil {
		return fmt.Errorf("drop privileges: %w", err)
	}
	// Tini forwards Docker's stop signal with kill(2). After this drop the
	// runtime is unprivileged, so that needs CAP_KILL in the runtime
	// container's cap_add; without it tini fails with EPERM and the container
	// is killed instead of shut down, which leaves Chromium's profile marked
	// as crashed. Report the misconfiguration at startup, loudly.
	if err := supervisor.CheckStopSignalPath(); err != nil {
		log.Warn("graceful container stop unavailable", slog.String("error", err.Error()))
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate runtime binary: %w", err)
	}

	leases, err := lease.NewManager(lease.Config{
		WorkspaceID: cfg.WorkspaceID,
		AgentMode:   cfg.AgentMode,
		PolicyStore: lease.FilePolicyStore{Dir: cfg.PolicyDir},
	})
	if err != nil {
		return err
	}
	capturer, err := capture.New(capture.Config{Endpoint: chromiumDevToolsAddr, MaxImageBytes: cfg.CaptureMaxImageBytes, MaxImagePixels: cfg.CaptureMaxImagePixels})
	if err != nil {
		return err
	}
	sup := supervisor.New(supervisor.Hooks{
		Ready: func() {
			leases.SetLocked(false, "")
		},
		NotReady:         func(code string) { leases.SetLocked(true, code) },
		BrowserRestarted: func() { leases.StartGeneration() },
		StopBrowser:      capturer.CloseBrowser,
	}, log, cfg.ChromiumSandbox)

	cdpPrefix := "/api/v1/workspaces/" + cfg.WorkspaceID + "/cdp"
	proxy, err := cdp.NewProxy(cdp.ProxyConfig{
		Upstream:     chromiumDevToolsURL,
		Auth:         leases,
		Logger:       log,
		PublicPrefix: func(*http.Request) string { return cdpPrefix },
	})
	if err != nil {
		return err
	}
	limits := transfer.Limits{QuotaBytes: cfg.FileQuotaBytes, MaxFileBytes: cfg.FileMaxBytes, MaxFiles: cfg.FileMaxFiles}
	handler := runtimeapi.NewHandler(runtimeapi.Deps{
		WorkspaceID:          cfg.WorkspaceID,
		Operator:             cfg.OperatorDigest,
		Leases:               leases,
		CDP:                  proxy,
		Clipboard:            clipboard.New(sup, cfg.ClipboardTTL, cfg.ClipboardMaxBytes),
		Health:               sup,
		Logger:               log,
		VNCView:              vnc.NewProxy(vnc.ProxyConfig{Instance: vnc.View, Logger: log}),
		VNCControl:           vnc.NewProxy(vnc.ProxyConfig{Instance: vnc.Control, MaxConns: 1, Logger: log}),
		Resizer:              vnc.NewResizer(sup),
		Capture:              capturer,
		Files:                transfer.NewHelperStore(sup, self, limits),
		FileLimits:           limits,
		ClipboardMaxBytes:    cfg.ClipboardMaxBytes,
		UploadReadTimeout:    cfg.UploadReadTimeout,
		ClipboardReadTimeout: cfg.ClipboardReadTimeout,
	})
	srv := runtimeapi.NewServer(cfg.Listen, handler)
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if tlsConf != nil {
		ln = tls.NewListener(ln, tlsConf)
	}

	servers := []*http.Server{srv}
	serveErr := make(chan error, 2)
	go func() { serveErr <- srv.Serve(ln) }()
	if tlsConf != nil {
		// The container healthcheck cannot present a client certificate: a
		// loopback-only plain listener answers /healthz and /readyz and
		// nothing else.
		hln, err := net.Listen("tcp", cfg.HealthListen)
		if err != nil {
			_ = ln.Close()
			return fmt.Errorf("health listen: %w", err)
		}
		hsrv := runtimeapi.NewServer(cfg.HealthListen, runtimeapi.NewHealthHandler(sup, log))
		servers = append(servers, hsrv)
		go func() { serveErr <- hsrv.Serve(hln) }()
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	supCtx, stopSupervisor := context.WithCancel(context.Background())
	defer stopSupervisor()
	supErr := make(chan error, 1)
	go func() { supErr <- sup.Run(supCtx) }()

	ctl := leases.AgentControl()
	log.Info("runtime started",
		slog.String("workspace", cfg.WorkspaceID),
		slog.String("listen", ln.Addr().String()),
		slog.Bool("mtls", tlsConf != nil),
		slog.String("agentMode", ctl.Mode),
		slog.Bool("agentPaused", ctl.Paused),
		slog.Uint64("agentPolicyRevision", ctl.Revision),
	)

	var result error
	supervisorDone := false
	select {
	case <-sigCtx.Done():
		log.Info("shutdown requested")
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			result = fmt.Errorf("api server: %w", err)
		}
	case err := <-supErr:
		supervisorDone = true
		if err != nil {
			result = fmt.Errorf("supervisor: %w", err)
		}
	}

	// Ordered shutdown: end the lease (closes control connections with
	// 4001), stop the API, then stop the browser stack.
	leases.Shutdown()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), serverShutdownGrace)
	defer cancel()
	for _, s := range servers {
		if err := s.Shutdown(shutdownCtx); err != nil {
			log.Warn("api server shutdown", slog.String("error", err.Error()))
			_ = s.Close()
		}
	}
	stopSupervisor()
	if !supervisorDone {
		if err := <-supErr; err != nil && result == nil {
			result = fmt.Errorf("supervisor: %w", err)
		}
	}
	log.Info("runtime stopped")
	return result
}
