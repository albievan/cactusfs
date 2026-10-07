package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/health"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/ports"
)

const (
	readinessTimeout  = 5 * time.Second
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 15 * time.Second
	idleTimeout       = 60 * time.Second
	maxHeaderBytes    = 16 << 10
)

// runServer runs a node until ctx is cancelled, then shuts it down gracefully.
// Logs are written to logOutput as JSON.
func runServer(ctx context.Context, cfg serverConfig, logOutput io.Writer) error {
	logger := slog.New(slog.NewJSONHandler(logOutput, &slog.HandlerOptions{Level: cfg.LogLevel}))

	tlsConfig, err := loadTLSConfig(cfg)
	if err != nil {
		return err
	}

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.AdminAddr)
	if err != nil {
		return fmt.Errorf("server.Listen: %w", err)
	}

	return serve(ctx, listener, serveOptions{
		tlsConfig:       tlsConfig,
		drainDelay:      cfg.DrainDelay,
		shutdownTimeout: cfg.ShutdownTimeout,
	}, logger)
}

// serveOptions is what serve needs beyond the listener.
type serveOptions struct {
	// tlsConfig is nil for development plaintext mode.
	tlsConfig *tls.Config
	// readinessChecks are evaluated by /readyz after the shutdown gate. Later
	// milestones wire the drive, metadata and KMS checks here.
	readinessChecks []ports.ReadinessCheck
	// drainDelay is how long the node keeps serving, with /readyz answering
	// 503, before its listener closes.
	drainDelay time.Duration
	// shutdownTimeout bounds the wait for in-flight requests.
	shutdownTimeout time.Duration
}

// loadTLSConfig returns the TLS configuration of the listener, or nil when the
// node runs in development plaintext mode.
func loadTLSConfig(cfg serverConfig) (*tls.Config, error) {
	if cfg.DevPlaintext {
		return nil, nil
	}
	certificate, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("server.LoadTLS: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		CipherSuites: tls12CipherSuites(),
		Certificates: []tls.Certificate{certificate},
	}, nil
}

// tls12CipherSuites returns the suites offered to TLS 1.2 clients: forward
// secrecy and AEAD only. Go's default list also accepts CBC-SHA1 suites. TLS
// 1.3 suites are not configurable and are all AEAD.
func tls12CipherSuites() []uint16 {
	return []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
		tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
	}
}

// newHTTPServer returns the admin HTTP server with the limits that protect it
// from slow and oversized requests.
func newHTTPServer(handler http.Handler, tlsConfig *tls.Config, logger *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           handler,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

// serve wires the use cases to the inbound adapters and serves them on
// listener until ctx is cancelled. A nil TLS configuration serves plaintext
// HTTP; the caller is responsible for having restricted the listener to
// loopback.
//
// On cancellation the readiness gate closes first. The node then keeps serving
// for the drain delay, with /readyz answering 503, so that load balancers and
// orchestrators can stop routing to it. After that the listener closes and
// in-flight requests get the shutdown timeout to finish.
func serve(ctx context.Context, listener net.Listener, opts serveOptions, logger *slog.Logger) error {
	gate := health.NewGate()
	healthService := app.NewHealthService(append([]ports.ReadinessCheck{gate}, opts.readinessChecks...)...)

	mux := http.NewServeMux()
	health.NewHandler(healthService, logger, readinessTimeout).Register(mux)

	server := newHTTPServer(mux, opts.tlsConfig, logger)

	serveErr := make(chan error, 1)
	go func() {
		if opts.tlsConfig == nil {
			serveErr <- server.Serve(listener)
			return
		}
		serveErr <- server.ServeTLS(listener, "", "")
	}()

	if opts.tlsConfig == nil {
		logger.WarnContext(ctx, "plaintext HTTP enabled for development; never use this in production")
	}
	logger.InfoContext(ctx, "listening",
		slog.String("addr", listener.Addr().String()),
		slog.Bool("tls", opts.tlsConfig != nil),
		slog.String("version", version),
	)

	select {
	case err := <-serveErr:
		return fmt.Errorf("server.Serve: %w", err)
	case <-ctx.Done():
	}

	gate.Close()
	logger.InfoContext(ctx, "shutting down",
		slog.String("drain_delay", opts.drainDelay.String()),
		slog.String("timeout", opts.shutdownTimeout.String()),
	)

	if opts.drainDelay > 0 {
		drain := time.NewTimer(opts.drainDelay)
		defer drain.Stop()
		select {
		case err := <-serveErr:
			return fmt.Errorf("server.Serve: %w", err)
		case <-drain.C:
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opts.shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		// In-flight requests outlived the timeout: cut them off rather than
		// leave connections and the serving goroutine behind.
		_ = server.Close()
		return fmt.Errorf("server.Shutdown: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server.Serve: %w", err)
	}

	logger.InfoContext(ctx, "stopped")
	return nil
}
