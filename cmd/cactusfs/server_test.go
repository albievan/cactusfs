package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/health"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/ports"
)

// running is a server started by startServer.
type running struct {
	listener net.Listener
	addr     string
	// cancel begins the shutdown, as SIGTERM does.
	cancel context.CancelFunc
	// done is closed when serve has returned; err then holds its result.
	done chan struct{}
	err  *error
}

// wait returns what serve returned, failing the test if it does not return.
func (r running) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-r.done:
		return *r.err
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not return")
		return nil
	}
}

// startServer runs serve with the probe endpoints on an ephemeral loopback
// port. checks are evaluated by /readyz after the shutdown gate.
func startServer(t *testing.T, opts serveOptions, checks ...ports.ReadinessCheck) running {
	t.Helper()

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if opts.shutdownTimeout == 0 {
		opts.shutdownTimeout = 5 * time.Second
	}
	logger := slog.New(slog.DiscardHandler)
	gate := health.NewGate()
	mux := http.NewServeMux()
	health.NewHandler(app.NewHealthService(append([]ports.ReadinessCheck{gate}, checks...)...), logger, readinessTimeout).Register(mux)
	endpoints := []endpoint{{name: "admin", listener: listener, handler: mux, ioTimeout: adminIOTimeout}}

	ctx, cancel := context.WithCancel(t.Context())
	srv := running{
		listener: listener,
		addr:     listener.Addr().String(),
		cancel:   cancel,
		done:     make(chan struct{}),
		err:      new(error),
	}
	go func() {
		defer close(srv.done)
		*srv.err = serve(ctx, endpoints, opts, gate, logger)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-srv.done:
		case <-time.After(15 * time.Second):
		}
	})
	return srv
}

// blockingCheck is a readiness check that holds /readyz open, which keeps a
// request in flight for as long as a test needs.
type blockingCheck struct {
	entered chan struct{}
	release chan struct{}
}

func newBlockingCheck() *blockingCheck {
	return &blockingCheck{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (b *blockingCheck) Name() string { return "blocking" }

func (b *blockingCheck) Check(ctx context.Context) error {
	b.entered <- struct{}{}
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// getAsync issues GET url in the background and delivers the status code, or
// 0 when the request failed.
func getAsync(t *testing.T, url string) <-chan int {
	t.Helper()

	status := make(chan int, 1)
	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			status <- 0
			return
		}
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			status <- 0
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		status <- resp.StatusCode
	}()
	return status
}

// response is what the tests need from an HTTP exchange, captured so that the
// body is always closed.
type response struct {
	status int
	body   string
	tls    *tls.ConnectionState
}

func get(t *testing.T, client *http.Client, url string) response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return response{status: resp.StatusCode, body: string(body), tls: resp.TLS}
}

func TestServePlaintext(t *testing.T) {
	t.Parallel()

	srv := startServer(t, serveOptions{})
	client := &http.Client{Timeout: 5 * time.Second}

	for path, wantStatus := range map[string]int{
		"/healthz": http.StatusOK,
		"/readyz":  http.StatusOK,
		"/":        http.StatusNotFound,
		"/metrics": http.StatusNotFound,
	} {
		resp := get(t, client, "http://"+srv.addr+path)
		if resp.status != wantStatus {
			t.Errorf("GET %s: status = %d, want %d", path, resp.status, wantStatus)
		}
		if wantStatus == http.StatusOK && resp.body != `{"status":"ok"}`+"\n" {
			t.Errorf("GET %s: body = %q", path, resp.body)
		}
	}

	srv.cancel()
	if err := srv.wait(t); err != nil {
		t.Fatalf("serve returned %v, want nil after a graceful shutdown", err)
	}
	if conn, err := srv.listener.Accept(); err == nil {
		_ = conn.Close()
		t.Error("listener is still open after shutdown")
	}
}

func TestServeAnswersNotReadyDuringDrainDelay(t *testing.T) {
	t.Parallel()

	const drainDelay = 3 * time.Second
	srv := startServer(t, serveOptions{drainDelay: drainDelay})
	client := &http.Client{Timeout: 5 * time.Second}

	if resp := get(t, client, "http://"+srv.addr+"/readyz"); resp.status != http.StatusOK {
		t.Fatalf("before shutdown: /readyz = %d, want 200", resp.status)
	}

	began := time.Now()
	srv.cancel()

	// The gate closes as soon as serve observes the cancellation.
	deadline := began.Add(drainDelay - 500*time.Millisecond)
	for {
		resp := get(t, client, "http://"+srv.addr+"/readyz")
		if resp.status == http.StatusServiceUnavailable {
			if resp.body != `{"status":"unavailable"}`+"\n" {
				t.Errorf("draining: /readyz body = %q", resp.body)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("draining: /readyz = %d, want 503", resp.status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if resp := get(t, client, "http://"+srv.addr+"/healthz"); resp.status != http.StatusOK {
		t.Errorf("draining: /healthz = %d, want 200: a draining node is still alive", resp.status)
	}

	if err := srv.wait(t); err != nil {
		t.Fatalf("serve returned %v, want nil", err)
	}
	if elapsed := time.Since(began); elapsed < drainDelay {
		t.Errorf("serve returned after %s, before the %s drain delay had passed", elapsed, drainDelay)
	}
}

func TestServeLetsInFlightRequestsFinish(t *testing.T) {
	t.Parallel()

	check := newBlockingCheck()
	srv := startServer(t, serveOptions{}, check)

	status := getAsync(t, "http://"+srv.addr+"/readyz")
	<-check.entered

	srv.cancel()
	select {
	case <-srv.done:
		t.Fatalf("serve returned (%v) while a request was in flight", *srv.err)
	case <-time.After(300 * time.Millisecond):
	}

	close(check.release)
	if got := <-status; got != http.StatusOK {
		t.Errorf("in-flight request: status = %d, want 200", got)
	}
	if err := srv.wait(t); err != nil {
		t.Fatalf("serve returned %v, want nil", err)
	}
}

func TestServeCutsOffRequestsThatOutliveTheShutdownTimeout(t *testing.T) {
	t.Parallel()

	check := newBlockingCheck()
	srv := startServer(t, serveOptions{shutdownTimeout: 200 * time.Millisecond}, check)

	status := getAsync(t, "http://"+srv.addr+"/readyz")
	<-check.entered

	srv.cancel()
	err := srv.wait(t)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "server.Shutdown") {
		t.Fatalf("serve returned %v, want a wrapped shutdown deadline error", err)
	}

	// The stuck request was cut off, not left behind: its connection is
	// closed, so the client fails long before the check would have returned.
	select {
	case got := <-status:
		if got != 0 {
			t.Errorf("stuck request: status = %d, want the connection to be closed", got)
		}
	case <-time.After(3 * time.Second):
		t.Error("stuck request is still open after the shutdown timeout")
	}
}

// slowExitCheck is a readiness check that notices cancellation and then takes
// a while to return, like a handler that is finishing a commit.
type slowExitCheck struct {
	entered  chan struct{}
	returned atomic.Bool
}

func (c *slowExitCheck) Name() string { return "slow-exit" }

func (c *slowExitCheck) Check(ctx context.Context) error {
	c.entered <- struct{}{}
	<-ctx.Done()
	time.Sleep(400 * time.Millisecond)
	c.returned.Store(true)
	return ctx.Err()
}

// TestServeWaitsForHandlersBeforeReturning: when the shutdown timeout passes,
// connections are closed, but a handler may still be running. The caller
// closes the drives and the metadata store as soon as serve returns, so serve
// must not return while a handler can still be using them.
func TestServeWaitsForHandlersBeforeReturning(t *testing.T) {
	t.Parallel()

	check := &slowExitCheck{entered: make(chan struct{}, 1)}
	srv := startServer(t, serveOptions{shutdownTimeout: 100 * time.Millisecond}, check)
	getAsync(t, "http://"+srv.addr+"/readyz")
	<-check.entered

	srv.cancel()
	err := srv.wait(t)
	if !check.returned.Load() {
		t.Fatal("serve returned while a request handler was still running")
	}
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errHandlersRunning) {
		t.Errorf("serve returned %v, want the shutdown deadline error and no running handlers", err)
	}
}

func TestWaitForGivesUp(t *testing.T) {
	t.Parallel()

	var running sync.WaitGroup
	running.Add(1)
	if waitFor(&running, 20*time.Millisecond) {
		t.Error("waitFor reported completion while a handler was running")
	}
	running.Done()
	if !waitFor(&running, time.Second) {
		t.Error("waitFor did not notice that the handlers returned")
	}
}

func TestNewHTTPServerLimits(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)

	admin := newHTTPServer(endpoint{name: "admin", handler: http.NewServeMux(), ioTimeout: adminIOTimeout}, nil, logger)
	data := newHTTPServer(endpoint{name: "s3", handler: http.NewServeMux()}, nil, logger)

	for _, server := range []*http.Server{admin, data} {
		// Every listener bounds what a client can do before it has sent a
		// complete request head, and how long an idle connection lives.
		for name, got := range map[string]time.Duration{
			"ReadHeaderTimeout": server.ReadHeaderTimeout,
			"IdleTimeout":       server.IdleTimeout,
		} {
			if got <= 0 || got > 2*time.Minute {
				t.Errorf("%s = %s, want a positive bound of at most 2m", name, got)
			}
		}
		if server.MaxHeaderBytes <= 0 || server.MaxHeaderBytes > 64<<10 {
			t.Errorf("MaxHeaderBytes = %d, want a positive bound of at most 64 KiB", server.MaxHeaderBytes)
		}
		if server.ErrorLog == nil {
			t.Error("ErrorLog is nil: server errors would bypass the JSON log")
		}
	}
	// The admin listener carries small messages and bounds them whole.
	if admin.ReadTimeout <= 0 || admin.WriteTimeout <= 0 || admin.ReadTimeout > 2*time.Minute || admin.WriteTimeout > 2*time.Minute {
		t.Errorf("admin ReadTimeout = %s, WriteTimeout = %s; want positive bounds", admin.ReadTimeout, admin.WriteTimeout)
	}
	// An object transfer may take hours: the S3 listener must not cut it.
	if data.ReadTimeout != 0 || data.WriteTimeout != 0 {
		t.Errorf("S3 ReadTimeout = %s, WriteTimeout = %s; want none", data.ReadTimeout, data.WriteTimeout)
	}
}

func TestServeTLS(t *testing.T) {
	t.Parallel()

	certFile, keyFile, pool := writeTestCertificate(t)
	tlsConfig, err := loadTLSConfig(serverConfig{TLSCert: certFile, TLSKey: keyFile})
	if err != nil {
		t.Fatalf("loadTLSConfig: %v", err)
	}

	srv := startServer(t, serveOptions{tlsConfig: tlsConfig})

	t.Run("TLS 1.3 is negotiated", func(t *testing.T) {
		client := &http.Client{
			Timeout:   5 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		}
		resp := get(t, client, "https://"+srv.addr+"/readyz")
		if resp.status != http.StatusOK || resp.body != `{"status":"ok"}`+"\n" {
			t.Errorf("status = %d, body = %q", resp.status, resp.body)
		}
		if resp.tls == nil || resp.tls.Version != tls.VersionTLS13 {
			t.Errorf("negotiated TLS state = %+v, want TLS 1.3", resp.tls)
		}
	})

	t.Run("plaintext is refused", func(t *testing.T) {
		resp := get(t, &http.Client{Timeout: 5 * time.Second}, "http://"+srv.addr+"/healthz")
		if resp.status != http.StatusBadRequest {
			t.Errorf("plaintext request to the TLS listener: status = %d, want 400", resp.status)
		}
	})

	srv.cancel()
	if err := srv.wait(t); err != nil {
		t.Fatalf("serve returned %v, want nil after a graceful shutdown", err)
	}
}

func isAEADSuite(name string) bool {
	return strings.Contains(name, "_GCM_") || strings.Contains(name, "CHACHA20_POLY1305")
}

func TestTLSConfigIsStrict(t *testing.T) {
	t.Parallel()

	certFile, keyFile, _ := writeTestCertificate(t)
	cfg, err := loadTLSConfig(serverConfig{TLSCert: certFile, TLSKey: keyFile})
	if err != nil {
		t.Fatalf("loadTLSConfig: %v", err)
	}

	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want TLS 1.2 (%#x)", cfg.MinVersion, tls.VersionTLS12)
	}
	if cfg.MaxVersion != 0 {
		t.Errorf("MaxVersion = %#x, want unset so that TLS 1.3 is negotiated", cfg.MaxVersion)
	}
	if len(cfg.CipherSuites) == 0 {
		t.Fatal("CipherSuites is empty: Go's default list accepts CBC-SHA1 suites")
	}

	secure := map[uint16]bool{}
	for _, suite := range tls.CipherSuites() {
		secure[suite.ID] = true
	}
	for _, id := range cfg.CipherSuites {
		name := tls.CipherSuiteName(id)
		if !secure[id] {
			t.Errorf("%s is not in Go's list of secure suites", name)
		}
		if !strings.HasPrefix(name, "TLS_ECDHE_") {
			t.Errorf("%s has no forward secrecy", name)
		}
		if !isAEADSuite(name) {
			t.Errorf("%s is not an AEAD suite", name)
		}
	}
}

func TestLoadTLSConfig(t *testing.T) {
	t.Parallel()

	cfg, err := loadTLSConfig(serverConfig{DevPlaintext: true})
	if err != nil || cfg != nil {
		t.Errorf("plaintext: got (%v, %v), want (nil, nil)", cfg, err)
	}

	if _, err := loadTLSConfig(serverConfig{TLSCert: "missing.pem", TLSKey: "missing.key"}); err == nil {
		t.Error("missing certificate files: got nil error")
	}
}

// writeTestCertificate creates a throw-away self-signed certificate for
// 127.0.0.1, writes it and its key to a temporary directory and returns the
// file names together with a pool that trusts it.
func writeTestCertificate(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cactusfs-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	pool = x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("certificate not added to pool")
	}
	return certFile, keyFile, pool
}
