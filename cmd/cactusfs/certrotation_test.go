package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/nodetls"
)

// reloadSteps drives the loop that reads a node's certificate files again:
// step lets it read them once and returns when it has.
type reloadSteps struct {
	ticks   chan time.Time
	waiting chan struct{}
}

func newReloadSteps() *reloadSteps {
	return &reloadSteps{ticks: make(chan time.Time), waiting: make(chan struct{}, 64)}
}

// tick is the loop's timer. That it is asked for says that the reading
// before it is over.
func (s *reloadSteps) tick(time.Duration) <-chan time.Time {
	s.waiting <- struct{}{}
	return s.ticks
}

func (s *reloadSteps) await(t *testing.T) {
	t.Helper()
	select {
	case <-s.waiting:
	case <-time.After(30 * time.Second):
		t.Fatal("the certificate files are not being watched")
	}
}

func (s *reloadSteps) step(t *testing.T) {
	t.Helper()
	select {
	case s.ticks <- time.Now():
	case <-time.After(30 * time.Second):
		t.Fatal("the loop that reads the certificate files did not take its tick")
	}
	s.await(t)
}

func serialOfPEM(t *testing.T, certPEM []byte) string {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("no PEM block in the certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return certificate.SerialNumber.String()
}

// shownSerial completes a handshake and returns the serial number of the
// certificate the other end showed.
func shownSerial(t *testing.T, config *tls.Config, address string) string {
	t.Helper()
	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: config}
	conn, err := dialer.DialContext(t.Context(), "tcp", address)
	if err != nil {
		t.Fatalf("handshake with %s: %v", address, err)
	}
	defer func() { _ = conn.Close() }()
	secured, ok := conn.(*tls.Conn)
	if !ok {
		t.Fatal("not a TLS connection")
	}
	return secured.ConnectionState().PeerCertificates[0].SerialNumber.String()
}

// TestServedCertificateRotatesWithoutRestart: the certificate of the S3 and
// admin listeners is replaced on disk. A request on a connection from before
// is served as before, a new connection is shown the new certificate, and
// the protocol versions and suites are what they were.
func TestServedCertificateRotatesWithoutRestart(t *testing.T) {
	t.Parallel()
	ca, err := nodetls.NewNamedAuthority(rand.Reader, time.Now(), time.Hour, "test S3 CA")
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.CertPEM)
	issue := func() (certPEM []byte, keyPEM string) {
		certPEM, key, err := ca.IssueServer(nil, []net.IP{net.ParseIP("127.0.0.1")}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return certPEM, string(key)
	}
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	first, firstKey := issue()
	writeTestFile(t, certFile, string(first))
	writeTestFile(t, keyFile, firstKey)

	steps := newReloadSteps()
	tlsConfig, certificate, err := loadTLSConfig(t.Context(), serverConfig{
		TLSCert: certFile, TLSKey: keyFile, TLSReloadInterval: nodetls.MinReloadInterval, tlsReloadTick: steps.tick,
	}, nil)
	if err != nil {
		t.Fatalf("loadTLSConfig: %v", err)
	}
	reloadCtx, stop := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		certificate.Run(reloadCtx)
	}()
	steps.await(t)
	srv := startServer(t, serveOptions{tlsConfig: tlsConfig})
	url := "https://" + srv.addr + "/readyz"
	client := func() *http.Client {
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
		t.Cleanup(transport.CloseIdleConnections)
		return &http.Client{Timeout: 10 * time.Second, Transport: transport}
	}
	served := func(c *http.Client) string {
		resp := get(t, c, url)
		if resp.status != http.StatusOK || resp.tls == nil || resp.tls.Version != tls.VersionTLS13 {
			t.Fatalf("GET /readyz: status %d, TLS %+v", resp.status, resp.tls)
		}
		return resp.tls.PeerCertificates[0].SerialNumber.String()
	}

	// One client keeps its connection; it is the one from before.
	early := client()
	if got := served(early); got != serialOfPEM(t, first) {
		t.Fatalf("served %s, want %s", got, serialOfPEM(t, first))
	}

	second, secondKey := issue()
	writeTestFile(t, keyFile, secondKey)
	writeTestFile(t, certFile, string(second))
	steps.step(t)
	if got := served(client()); got != serialOfPEM(t, second) {
		t.Errorf("a new connection was shown %s, want the new certificate, %s", got, serialOfPEM(t, second))
	}
	if got := served(early); got != serialOfPEM(t, first) {
		t.Errorf("the connection from before now has certificate %s: it was opened again", got)
	}
	if certificate.Replaced() != 1 {
		t.Errorf("Replaced = %d, want 1", certificate.Replaced())
	}
	if tlsConfig.MinVersion != tls.VersionTLS12 || !slices.Equal(tlsConfig.CipherSuites, tls12CipherSuites()) || len(tlsConfig.Certificates) != 0 {
		t.Error("the TLS configuration is not what it was, with the certificate asked for at every handshake")
	}

	// A certificate without its key: refused, and the one in use is served.
	third, _ := issue()
	writeTestFile(t, certFile, string(third))
	steps.step(t)
	if got := served(client()); got != serialOfPEM(t, second) || certificate.Refused(nodetls.RefusedPair) != 1 {
		t.Errorf("with half a pair on disk a new connection was shown %s; refused %d", got, certificate.Refused(nodetls.RefusedPair))
	}

	stop()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the loop did not end with its context")
	}
	srv.cancel()
	if err := srv.wait(t); err != nil {
		t.Fatalf("serve returned %v", err)
	}
}

// TestClusterRotatesNodeCertificatesAndKeepsItsLeader: four nodes over the
// real transport. The certificate of every node is replaced, one node after
// the other. Each time, new handshakes on the node's Raft and node RPC ports
// are shown the new certificate, the metadata group has the leader and the
// term it had, and an object is written through the node and read through
// another. A certificate that names another node, and one of another CA,
// change nothing.
func TestClusterRotatesNodeCertificatesAndKeepsItsLeader(t *testing.T) {
	t.Parallel()
	c := newTestCluster(t, fourNodes())
	steps := map[string]*reloadSteps{}
	for id, cfg := range c.configs {
		steps[id] = newReloadSteps()
		cfg.TLSReloadInterval, cfg.tlsReloadTick = nodetls.MinReloadInterval, steps[id].tick
		c.configs[id] = cfg
	}
	c.start(c.all()...)
	ids := slices.Sorted(maps.Keys(c.configs))
	for _, id := range ids {
		steps[id].await(t)
	}
	if err := c.node("n1").buckets.Create(t.Context(), root(), "bkt"); err != nil {
		t.Fatalf("create the bucket: %v", err)
	}

	leader := func() (string, uint64) {
		t.Helper()
		for _, id := range ids {
			if c.node(id).meta.IsLeader() {
				term, err := c.node(id).meta.Term()
				if err != nil {
					t.Fatalf("term of %s: %v", id, err)
				}
				return id, term
			}
		}
		t.Fatal("the metadata group has no leader")
		return "", 0
	}
	leaderID, term := leader()

	// A voter asks: the Raft port admits nobody else.
	asker := func(id string) *node {
		if id == "n1" {
			return c.node("n2")
		}
		return c.node("n1")
	}
	shownBy := func(id string) (raft, rpc string) {
		t.Helper()
		config := asker(id).identity.Client(id)
		raft = shownSerial(t, config, c.configs[id].AdvertiseRaft)
		config = asker(id).identity.Client(id)
		config.NextProtos = []string{"h2"}
		return raft, shownSerial(t, config, c.configs[id].AdvertiseRPC)
	}
	replace := func(id string, authority *nodetls.Authority, named string) string {
		t.Helper()
		certPEM, keyPEM, err := authority.Issue(named, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, c.configs[id].NodeTLSKey, string(keyPEM))
		writeTestFile(t, c.configs[id].NodeTLSCert, string(certPEM))
		steps[id].step(t)
		return serialOfPEM(t, certPEM)
	}
	unchanged := func(when string) {
		t.Helper()
		if nowLeader, nowTerm := leader(); nowLeader != leaderID || nowTerm != term {
			t.Fatalf("%s: the leader is %s in term %d; it was %s in term %d", when, nowLeader, nowTerm, leaderID, term)
		}
	}

	current := map[string]string{}
	for i, id := range ids {
		before, _ := shownBy(id)
		serial := replace(id, c.authority, id)
		raft, rpc := shownBy(id)
		if raft != serial || rpc != serial || before == serial {
			t.Errorf("%s: after the replacement its Raft port shows %s and its RPC port %s, want %s (before: %s)",
				id, raft, rpc, serial, before)
		}
		if got := c.node(id).identity.NotAfter(); !got.Equal(c.authority.NotAfter()) || c.node(id).identity.NodeID() != id {
			t.Errorf("%s: its identity is %s until %s", id, c.node(id).identity.NodeID(), got)
		}
		current[id] = serial
		unchanged("after the certificate of " + id + " was replaced")
		// A proposal through the node, read through the next one.
		key, data := "object-"+id, randomData(t, 64<<10)
		if err := c.put(id, key, data); err != nil {
			t.Fatalf("put through %s after its certificate was replaced: %v", id, err)
		}
		if got, err := c.get(ids[(i+1)%len(ids)], key); err != nil || string(got) != string(data) {
			t.Fatalf("get through %s: %d bytes, %v", ids[(i+1)%len(ids)], len(got), err)
		}
		unchanged("after writing through " + id)
	}

	// What must not be loaded is not, and the node goes on as it was.
	foreign, err := nodetls.NewAuthority(rand.Reader, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for what, bad := range map[string]func() string{
		"a certificate that names another node": func() string { return replace("n1", c.authority, "n2") },
		"a certificate of another CA":           func() string { return replace("n1", foreign, "n1") },
	} {
		refused := bad()
		if raft, rpc := shownBy("n1"); raft != current["n1"] || rpc != current["n1"] || raft == refused {
			t.Errorf("%s: n1 shows %s on its Raft port and %s on its RPC port, want the one in use, %s", what, raft, rpc, current["n1"])
		}
		unchanged("after " + what + " was put on disk")
	}
	if err := c.put("n1", "object-last", randomData(t, 1<<10)); err != nil {
		t.Fatalf("put through n1 with a refused certificate on disk: %v", err)
	}
	if _, err := c.get("n3", "object-last"); err != nil {
		t.Fatalf("get through n3: %v", err)
	}
	unchanged("at the end")

	// What n1 says of it when it is scraped: the certificate in use ends
	// when the CA does, what is on disk is not in use, and each of the two
	// that were refused is counted once, by its reason. It has no gauge of a
	// certificate for clients: none was given to this node.
	got := samples(t, c.telemetry["n1"])
	for name, want := range map[string]float64{
		"cactusfs_node_certificate_not_after_seconds{}":                 float64(c.authority.NotAfter().Unix()),
		"cactusfs_tls_certificate_not_after_seconds{use=node}":          float64(c.authority.NotAfter().Unix()),
		"cactusfs_tls_reload_pending{use=node}":                         1,
		"cactusfs_tls_reload_refused_total{reason=identity,use=node}":   1,
		"cactusfs_tls_reload_refused_total{reason=untrusted,use=node}":  1,
		"cactusfs_tls_reload_refused_total{reason=pair,use=node}":       0,
		"cactusfs_tls_reload_refused_total{reason=unreadable,use=node}": 0,
	} {
		if value, ok := got[name]; !ok || value != want {
			t.Errorf("n1: %s = %v (exposed: %v), want %v", name, value, ok, want)
		}
	}
	if got := samples(t, c.telemetry["n2"])["cactusfs_tls_reload_pending{use=node}"]; got != 0 {
		t.Errorf("n2: cactusfs_tls_reload_pending{use=node} = %v with its new certificate in use", got)
	}
	for name := range got {
		if strings.Contains(name, "use=s3") {
			t.Errorf("n1: %s is exposed by a node that was given no certificate for clients", name)
		}
	}

	// Handshakes n1 refuses on its Raft port are counted, each once: a node
	// that does not vote, a caller with a certificate of another CA, one
	// with no certificate, one that speaks no TLS.
	raftPort := c.configs["n1"].AdvertiseRaft
	refusedOn := func(reason string) float64 {
		return samples(t, c.telemetry["n1"])["cactusfs_node_handshakes_refused_total{reason="+reason+"}"]
	}
	turnedAway := func(config *tls.Config) {
		t.Helper()
		dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: config}
		conn, err := dialer.DialContext(t.Context(), "tcp", raftPort)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		if n, err := conn.Read(make([]byte, 1)); err == nil || n > 0 {
			t.Error("the Raft port of n1 served a caller it must turn away")
		}
	}
	strangerPEM, strangerKey, err := foreign.Issue("n2", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := tls.X509KeyPair(strangerPEM, strangerKey)
	if err != nil {
		t.Fatal(err)
	}
	// Both trust n1, as a node of its cluster would.
	withoutCertificate, ofAnotherCA := c.node("n2").identity.Client("n1"), c.node("n2").identity.Client("n1")
	withoutCertificate.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		return &tls.Certificate{}, nil
	}
	ofAnotherCA.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &stranger, nil }
	for _, tc := range []struct {
		name, reason string
		grows        float64
		call         func()
	}{
		{name: "a node that does not vote", reason: "not_admitted", grows: 1, call: func() { turnedAway(c.node("n4").identity.Client("n1")) }},
		{name: "a certificate of another CA, and none", reason: "certificate", grows: 2, call: func() {
			turnedAway(ofAnotherCA)
			turnedAway(withoutCertificate)
		}},
		{name: "no TLS", reason: "other", grows: 1, call: func() {
			raw, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(t.Context(), "tcp", raftPort)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = raw.Close() }()
			_, _ = raw.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
			_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
			_, _ = raw.Read(make([]byte, 1))
		}},
	} {
		before := refusedOn(tc.reason)
		tc.call()
		// The listener counts when its side of the handshake has ended.
		for deadline := time.Now().Add(10 * time.Second); refusedOn(tc.reason) < before+tc.grows && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
		// Scraped twice more: what was counted is not added again.
		_ = refusedOn(tc.reason)
		if got := refusedOn(tc.reason); got != before+tc.grows {
			t.Errorf("%s: n1 counts %v handshakes refused as %s, want %v more than the %v before", tc.name, got, tc.reason, tc.grows, before)
		}
	}
	unchanged("after callers were turned away from the Raft port")
}

// TestReloaderThatDoesNotStopDoesNotHoldUpShutdown: a node that stops waits
// for the loop that reads its certificate files, and not for long. A loop
// that has ended is not remarked on; one that has not is, as a warning, and
// the shutdown goes on.
func TestReloaderThatDoesNotStopDoesNotHoldUpShutdown(t *testing.T) {
	t.Parallel()
	if reloadStopTimeout <= 0 || reloadStopTimeout > 5*time.Second {
		t.Errorf("a stopping node waits %s for the loop", reloadStopTimeout)
	}
	tests := []struct {
		name    string
		stopped bool
		warns   bool
	}{
		{name: "the loop has ended", stopped: true},
		{name: "the loop is held in a read", warns: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			stopped := make(chan struct{})
			if tc.stopped {
				close(stopped)
			}
			returned := make(chan struct{})
			go func() {
				defer close(returned)
				awaitReloader(t.Context(), slog.New(slog.NewJSONHandler(&logs, nil)), "node", stopped, 50*time.Millisecond)
			}()
			select {
			case <-returned:
			case <-time.After(30 * time.Second):
				t.Fatal("the shutdown waited for a loop that does not stop")
			}
			warned := strings.Contains(logs.String(), `"level":"WARN"`) && strings.Contains(logs.String(), "did not stop") &&
				strings.Contains(logs.String(), `"certificate":"node"`)
			if warned != tc.warns || (!tc.warns && logs.Len() != 0) {
				t.Errorf("warned = %t, want %t; the log:\n%s", warned, tc.warns, logs.String())
			}
		})
	}
}
