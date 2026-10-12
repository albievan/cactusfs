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
	"sync"
	"sync/atomic"
	"time"

	"github.com/albievan/cactusfs/api/openapi"
	"github.com/albievan/cactusfs/internal/adapters/adminapi"
	"github.com/albievan/cactusfs/internal/adapters/adminapi/swaggerui"
	"github.com/albievan/cactusfs/internal/adapters/console"
	"github.com/albievan/cactusfs/internal/adapters/health"
	"github.com/albievan/cactusfs/internal/adapters/nodetls"
	"github.com/albievan/cactusfs/internal/adapters/s3api"
	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/buildinfo"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/web"
)

const (
	readinessTimeout  = 5 * time.Second
	readHeaderTimeout = 5 * time.Second
	idleTimeout       = 60 * time.Second
	maxHeaderBytes    = 16 << 10
	// adminIOTimeout bounds a whole request or response on the admin
	// listener, whose messages are small.
	adminIOTimeout = 15 * time.Second
	// handlerExitWait bounds the wait for request handlers to return after
	// their connections have been closed.
	handlerExitWait = 60 * time.Second
)

// errHandlersRunning reports that request handlers were still running when
// the node gave up waiting for them. The stores must then not be closed
// underneath them; the process exits instead.
var errHandlersRunning = errors.New("request handlers are still running")

// endpoint is one listener and what it serves.
type endpoint struct {
	name     string
	listener net.Listener
	handler  http.Handler
	// ioTimeout bounds the reading of a whole request and the writing of a
	// whole response. Zero means no bound, for the S3 listener, where an
	// object transfer may legitimately take hours.
	ioTimeout time.Duration
}

// serveOptions is what serve needs beyond the endpoints.
type serveOptions struct {
	// tlsConfig is nil for development plaintext mode.
	tlsConfig *tls.Config
	// drainDelay is how long the node keeps serving, with /readyz answering
	// 503, before its listeners close.
	drainDelay time.Duration
	// shutdownTimeout bounds the wait for in-flight requests.
	shutdownTimeout time.Duration
	// ready, when not nil, is closed when the node has started. Until
	// then the listeners are open and serve refusals; "listening" is logged
	// when they serve the node. Nil means that they do from the start.
	ready <-chan struct{}
	// build is which build this is; the listeners say it when they open.
	build buildinfo.Info
}

// runServer runs a node until ctx is cancelled, then shuts it down
// gracefully. Logs are written to logOutput as JSON; notice is the operator's
// terminal, used once for generated credentials.
func runServer(ctx context.Context, cfg serverConfig, logOutput, notice io.Writer) (err error) {
	// The log and, when exported, the audit records share one stream; each
	// line is written whole.
	logOutput = &lineWriter{out: logOutput}
	logger := slog.New(slog.NewJSONHandler(logOutput, &slog.HandlerOptions{Level: cfg.LogLevel}))
	if cfg.AuditExport {
		cfg.auditOut = logOutput
	}

	// Which build this is: what the log, the metrics and - to a caller who
	// has signed in - the management API say.
	build := buildinfo.Get()

	tlsConfig, certificate, err := loadTLSConfig(ctx, cfg, logger)
	if err != nil {
		return err
	}
	if certificate != nil {
		// The certificate is replaced without a restart when its files
		// change (ADR-0020), until the node is told to stop.
		reloadCtx, stopReload := context.WithCancel(ctx)
		reloaded := make(chan struct{})
		go func() {
			defer close(reloaded)
			certificate.Run(reloadCtx)
		}()
		defer func() {
			stopReload()
			awaitReloader(context.WithoutCancel(ctx), logger, "server", reloaded, reloadStopTimeout)
		}()
	}
	// An endpoint that is refused is named in the error without what a
	// credential travels in, and what the exporter cannot send later is said
	// in the node's log, without the words of the error (telemetry
	// .ExportErrors).
	tel, err := telemetry.New(ctx, telemetry.Config{Version: build.Tag(), OTLPEndpoint: cfg.OTLPEndpoint, Logger: logger})
	if err != nil {
		return fmt.Errorf("server.Open: %w", err)
	}
	defer func() {
		if stopErr := tel.Shutdown(ctx); stopErr != nil {
			err = errors.Join(err, fmt.Errorf("server.Close: %w", stopErr))
		}
	}()
	if !tel.Exporting() {
		logger.InfoContext(ctx, "traces are not exported: no OTLP endpoint is configured")
	}

	// The listeners open before the node is part of its deployment: a
	// member of a cluster may wait a long time for the others, and whatever
	// watches it - a kubelet, a load balancer - must find it alive and not
	// ready, not absent. Until then the probes answer and everything else
	// is refused; see starting.
	gate := health.NewGate()
	metadata := &lateCheck{name: "metadata"}
	readiness := app.NewHealthService(gate, metadata)
	probes := health.NewHandler(readiness, logger, readinessTimeout)
	// The gauge says what /readyz says, from the first scrape on.
	tel.ObserveReadiness(readyGauge{health: readiness})
	if certificate != nil {
		// And which certificate clients are shown, whichever it is by then.
		tel.ObserveTLS(telemetry.TLSUseS3, tlsGauges{reloader: certificate})
	}
	adminHandler, s3Handler := &handlerSwitch{}, &handlerSwitch{}
	adminHandler.set(startingAdmin(probes))
	s3Handler.set(s3api.Starting())
	// Every node answers probes and serves metrics; the S3 API is the
	// gateway's.
	serving, addrs := []endpoint{{name: "admin", handler: adminHandler, ioTimeout: adminIOTimeout}}, []string{cfg.AdminAddr}
	if cfg.Roles.Has(cluster.RoleGateway) {
		serving, addrs = append(serving, endpoint{name: "s3", handler: s3Handler}), append(addrs, cfg.S3Addr)
	}
	endpoints, err := listen(ctx, serving, addrs)
	if err != nil {
		return err
	}
	ready := make(chan struct{})
	serveCtx, stopServing := context.WithCancel(ctx)
	defer stopServing()
	// A listener that stops on its own takes the node down, while it
	// starts as well as after: opening is given up when serving has ended.
	openCtx, stopOpening := context.WithCancel(ctx)
	defer stopOpening()
	served := make(chan error, 1)
	go func() {
		defer stopOpening()
		served <- serve(serveCtx, endpoints, serveOptions{
			tlsConfig:       tlsConfig,
			build:           build,
			drainDelay:      cfg.DrainDelay,
			shutdownTimeout: cfg.ShutdownTimeout,
			ready:           ready,
		}, gate, logger)
	}()

	n, err := openNode(openCtx, cfg, tel, logger, notice)
	if err != nil {
		// Nothing was served but refusals; they end with the node.
		stopServing()
		servedErr := <-served
		if ctx.Err() != nil && servedErr == nil {
			// Told to stop while it was starting - waiting for the other
			// members, as a rule: a stop like any other, and not a
			// failure.
			logger.InfoContext(ctx, "stopped before the node had started")
			return nil
		}
		return errors.Join(fmt.Errorf("server.Open: %w", err), servedErr)
	}
	if certificate != nil {
		// The certificate clients are shown is part of what the node says of
		// its settings.
		n.local.serve(cluster.CertificateS3, certificate)
	}
	n.startBackground(ctx, cfg)

	admin := http.NewServeMux()
	probes.Register(admin)
	admin.Handle("GET /metrics", tel.MetricsHandler(n.identities, n.authorizer, logger))
	// The management API and its documentation are the gateway's; the
	// console is served by nodes with the console role. All of it shares
	// the admin listener, and so one origin.
	if cfg.Roles.Has(cluster.RoleGateway) {
		admin.Handle(adminapi.BasePath+"/", tel.TraceAdmin(adminapi.NewHandler(adminapi.Config{
			Buckets: n.buckets, Objects: n.objects, Search: n.search, IAM: n.iam, Sessions: n.sessions, Cluster: n.cluster,
			Settings:    n.settings,
			Idempotency: n.idempotency, Identities: n.identities, Clock: n.clock, Logger: logger,
			SpecYAML: openapi.AdminYAML(), SpecJSON: openapi.AdminJSON(), Observe: tel.ObserveAdmin,
			Build: build,
		})))
		swaggerui.Register(admin)
	}
	if cfg.Roles.Has(cluster.RoleConsole) {
		admin.Handle("/", console.NewHandler(web.Dist()))
	}
	s3Handler.set(tel.TraceS3(s3api.NewHandler(s3api.Config{
		Buckets: n.buckets, Objects: n.objects, Identities: n.identities,
		Clock: n.clock, Logger: logger, Region: cfg.Region, Domain: cfg.S3Domain,
		Observe: tel.ObserveS3,
	})))
	adminHandler.set(admin)
	metadata.set(n.meta)
	logger.InfoContext(ctx, "node started", slog.String("drives", n.describe()), slog.String("metadata_node", n.meta.NodeID()),
		slog.String("roles", cfg.Roles.String()), slog.Bool("cluster", cfg.clustered()))
	close(ready)

	err = <-served
	// The stores are closed only once no handler can still be using them:
	// a write that is being committed must not find its drive or the
	// metadata store torn down.
	if errors.Is(err, errHandlersRunning) {
		return err
	}
	if closeErr := n.Close(); closeErr != nil {
		err = errors.Join(err, fmt.Errorf("server.Close: %w", closeErr))
	}
	return err
}

// handlerSwitch serves with whatever handler it was last given: what a
// listener serves changes, once, when the node has started.
type handlerSwitch struct {
	current atomic.Pointer[http.Handler]
}

func (s *handlerSwitch) set(h http.Handler) { s.current.Store(&h) }

// ServeHTTP implements http.Handler.
func (s *handlerSwitch) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	(*s.current.Load()).ServeHTTP(w, r)
}

// startingAdmin is what the admin listener serves until the node has
// started: the probes, and a refusal of everything else.
func startingAdmin(probes *health.Handler) http.Handler {
	mux := http.NewServeMux()
	probes.Register(mux)
	mux.Handle("/", adminapi.Starting())
	return mux
}

// lateCheck is a readiness check that fails until it is given the check it
// stands for: the node is not ready before it has a metadata store to ask.
type lateCheck struct {
	name  string
	check atomic.Pointer[ports.ReadinessCheck]
}

func (c *lateCheck) set(check ports.ReadinessCheck) { c.check.Store(&check) }

// Name implements ports.ReadinessCheck.
func (c *lateCheck) Name() string { return c.name }

// Check implements ports.ReadinessCheck.
func (c *lateCheck) Check(ctx context.Context) error {
	check := c.check.Load()
	if check == nil {
		return fmt.Errorf("%w: the node is starting", domain.ErrUnavailable)
	}
	return (*check).Check(ctx)
}

// lineWriter serialises writes to a stream that several writers share.
type lineWriter struct {
	mu  sync.Mutex
	out io.Writer
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.Write(p)
}

// listen opens a listener for every endpoint. On failure the ones already
// opened are closed.
func listen(ctx context.Context, endpoints []endpoint, addrs []string) ([]endpoint, error) {
	for i := range endpoints {
		listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addrs[i])
		if err != nil {
			for _, opened := range endpoints[:i] {
				_ = opened.listener.Close()
			}
			return nil, fmt.Errorf("server.Listen: %s: %w", endpoints[i].name, err)
		}
		endpoints[i].listener = listener
	}
	return endpoints, nil
}

// reloadStopTimeout is how long a node that stops waits for a loop that
// reads certificate files again to end.
const reloadStopTimeout = 2 * time.Second

// awaitReloader waits until the loop that reads the certificate files of the
// given kind again has stopped, and no longer than limit. The loop ends with
// its context whatever it is doing; should it not, the node stops all the
// same and says so. Its goroutine - one that is held in a read of a file on
// a mount that hangs, say - is left behind: the process is about to end, and
// nothing it could still do changes what was stored.
func awaitReloader(ctx context.Context, logger *slog.Logger, kind string, stopped <-chan struct{}, limit time.Duration) {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-stopped:
	case <-timer.C:
		logger.WarnContext(ctx, "the loop that reads the certificate files again did not stop; shutdown goes on without it",
			slog.String("certificate", kind), slog.Duration("waited", limit))
	}
}

// loadTLSConfig returns the TLS configuration of the listeners and what keeps
// their certificate current, or nil for both when the node runs in
// development plaintext mode. Every handshake is shown the certificate in use
// when it begins; the caller runs the reloader that replaces it.
func loadTLSConfig(ctx context.Context, cfg serverConfig, logger *slog.Logger) (*tls.Config, *nodetls.Reloader, error) {
	if cfg.DevPlaintext {
		return nil, nil, nil
	}
	certificate, err := nodetls.LoadServer(ctx, nodetls.ReloadConfig{
		CertFile: cfg.TLSCert, KeyFile: cfg.TLSKey, Interval: cfg.TLSReloadInterval,
		Clock: systemClock{}, Logger: logger, Tick: cfg.tlsReloadTick,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("server.LoadTLS: %w", err)
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		CipherSuites:   tls12CipherSuites(),
		GetCertificate: certificate.GetCertificate,
	}, certificate, nil
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

// newHTTPServer returns the HTTP server of an endpoint with the limits that
// protect it from slow and oversized requests.
func newHTTPServer(e endpoint, tlsConfig *tls.Config, logger *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           e.handler,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       e.ioTimeout,
		WriteTimeout:      e.ioTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(logger.With(slog.String("listener", e.name)).Handler(), slog.LevelWarn),
	}
}

// serve serves every endpoint until ctx is cancelled. A nil TLS configuration
// serves plaintext HTTP; the caller is responsible for having restricted the
// listeners to loopback.
//
// On cancellation the readiness gate closes first. The node then keeps serving
// for the drain delay, with /readyz answering 503, so that load balancers and
// orchestrators can stop routing to it. After that the listeners close and
// in-flight requests get the shutdown timeout to finish.
func serve(ctx context.Context, endpoints []endpoint, opts serveOptions, gate *health.Gate, logger *slog.Logger) error {
	servers := make([]*http.Server, len(endpoints))
	serveErr := make(chan error, len(endpoints))
	var handlers sync.WaitGroup
	for i, e := range endpoints {
		e.handler = trackHandlers(&handlers, e.handler)
		server := newHTTPServer(e, opts.tlsConfig, logger)
		servers[i] = server
		go func() {
			if opts.tlsConfig == nil {
				serveErr <- server.Serve(e.listener)
				return
			}
			serveErr <- server.ServeTLS(e.listener, "", "")
		}()
	}
	// "listening" says that the node is served there; a listener that is
	// open while the node starts says only that.
	announce := func(msg string) {
		for _, e := range endpoints {
			logger.InfoContext(ctx, msg,
				slog.String("listener", e.name),
				slog.String("addr", e.listener.Addr().String()),
				slog.Bool("tls", opts.tlsConfig != nil),
				slog.String("version", opts.build.Version),
				slog.String("build", opts.build.Build),
				slog.String("commit", opts.build.Commit),
			)
		}
	}
	ready := opts.ready
	if ready == nil {
		announce("listening")
	} else {
		announce("listener open; the node is starting")
	}
	if opts.tlsConfig == nil {
		logger.WarnContext(ctx, "plaintext HTTP enabled for development; never use this in production")
	}

	// A listener that stops on its own takes the node down: half a node
	// must not keep running.
	var failure error
	for waiting := true; waiting; {
		select {
		case err := <-serveErr:
			failure, waiting = fmt.Errorf("server.Serve: %w", err), false
		case <-ready:
			announce("listening")
			ready = nil
		case <-ctx.Done():
			waiting = false
		}
	}
	// A node that never started has nobody to drain. (One that started at
	// the very moment it was told to stop did, and has.)
	if ready != nil {
		select {
		case <-ready:
			announce("listening")
		default:
			opts.drainDelay = 0
		}
	}

	gate.Close()
	logger.InfoContext(ctx, "shutting down",
		slog.String("drain_delay", opts.drainDelay.String()),
		slog.String("timeout", opts.shutdownTimeout.String()),
	)
	if failure == nil && opts.drainDelay > 0 {
		drain := time.NewTimer(opts.drainDelay)
		defer drain.Stop()
		select {
		case err := <-serveErr:
			failure = fmt.Errorf("server.Serve: %w", err)
		case <-drain.C:
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opts.shutdownTimeout)
	defer cancel()
	shutdownErr := shutdownAll(shutdownCtx, servers)
	// Shutdown waits for handlers; a forced Close only closes their
	// connections. Either way the caller is about to close the stores, so
	// every handler must have returned first.
	if !waitFor(&handlers, handlerExitWait) {
		return errors.Join(failure, shutdownErr, fmt.Errorf("server.Shutdown: %w", errHandlersRunning))
	}
	if shutdownErr != nil {
		return errors.Join(failure, fmt.Errorf("server.Shutdown: %w", shutdownErr))
	}
	if failure != nil {
		return failure
	}
	logger.InfoContext(ctx, "stopped")
	return nil
}

// shutdownAll shuts every server down gracefully, at the same time. A server
// whose in-flight requests outlive ctx is closed, so that no connection or
// goroutine is left behind.
func shutdownAll(ctx context.Context, servers []*http.Server) error {
	errs := make([]error, len(servers))
	var wg sync.WaitGroup
	for i, server := range servers {
		wg.Go(func() {
			if err := server.Shutdown(ctx); err != nil {
				_ = server.Close()
				errs[i] = err
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// trackHandlers counts the requests a handler is serving.
func trackHandlers(running *sync.WaitGroup, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		running.Add(1)
		defer running.Done()
		next.ServeHTTP(w, r)
	})
}

// waitFor waits until running is done, for at most limit, and reports whether
// it was.
func waitFor(running *sync.WaitGroup, limit time.Duration) bool {
	done := make(chan struct{})
	go func() {
		running.Wait()
		close(done)
	}()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
