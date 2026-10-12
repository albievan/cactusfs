package nodetls_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/nodetls"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// pair is a certificate and its key, in PEM.
type pair struct {
	cert, key []byte
}

func nodePair(t *testing.T, a *nodetls.Authority, nodeID string) pair {
	t.Helper()
	certPEM, keyPEM, err := a.Issue(nodeID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return pair{certPEM, keyPEM}
}

func serverPair(t *testing.T, a *nodetls.Authority, names ...string) pair {
	t.Helper()
	certPEM, keyPEM, err := a.IssueServer(names, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return pair{certPEM, keyPEM}
}

// selfSigned makes a certificate nobody issued, with the given uses, valid
// for an hour around now.
func selfSigned(t *testing.T, usages ...x509.ExtKeyUsage) pair {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "self-signed"}, DNSNames: []string{"localhost"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usages,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pair{
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}
}

func serialOf(t *testing.T, certPEM []byte) string {
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

// files are the two files a Reloader reads.
type files struct {
	cert, key string
}

func newFiles(t *testing.T, p pair) files {
	t.Helper()
	dir := t.TempDir()
	f := files{filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")}
	f.write(t, p)
	return f
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f files) write(t *testing.T, p pair) {
	t.Helper()
	writeFile(t, f.cert, p.cert)
	writeFile(t, f.key, p.key)
}

// logSink collects what a Reloader logs.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *logSink) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(l, nil))
}

// lines returns the logged lines that contain every one of the given parts.
func (l *logSink) lines(parts ...string) []string {
	var found []string
	for line := range strings.SplitSeq(l.String(), "\n") {
		matches := line != ""
		for _, part := range parts {
			matches = matches && strings.Contains(line, part)
		}
		if matches {
			found = append(found, line)
		}
	}
	return found
}

// assertNoKeyMaterial fails when a log holds any part of a private key: the
// PEM header of one, or a line of the body of any of the given keys.
func assertNoKeyMaterial(t *testing.T, log string, keys ...[]byte) {
	t.Helper()
	if strings.Contains(log, "PRIVATE KEY-----") {
		t.Error("the log holds the PEM header of a private key")
	}
	for _, key := range keys {
		for line := range strings.SplitSeq(string(key), "\n") {
			if len(line) >= 16 && !strings.HasPrefix(line, "-----") && strings.Contains(log, line) {
				t.Error("the log holds part of a private key")
			}
		}
		if block, _ := pem.Decode(key); block != nil && bytes.Contains([]byte(log), block.Bytes) {
			t.Error("the log holds the bytes of a private key")
		}
	}
}

// stepper drives the loop of a Reloader by hand: step lets it look at the
// files once and returns when it has.
type stepper struct {
	ticks   chan time.Time
	waiting chan struct{}
}

func newStepper() *stepper {
	return &stepper{ticks: make(chan time.Time), waiting: make(chan struct{}, 64)}
}

// tick is the Reloader's timer. That it is asked for says that the look
// before it is over.
func (s *stepper) tick(time.Duration) <-chan time.Time {
	s.waiting <- struct{}{}
	return s.ticks
}

func (s *stepper) await(t *testing.T) {
	t.Helper()
	select {
	case <-s.waiting:
	case <-time.After(30 * time.Second):
		t.Fatal("the reloader is not waiting for its next look at the files")
	}
}

func (s *stepper) step(t *testing.T) {
	t.Helper()
	select {
	case s.ticks <- time.Now():
	case <-time.After(30 * time.Second):
		t.Fatal("the reloader did not take its tick")
	}
	s.await(t)
}

// serveServer runs a TLS listener, as the S3 and admin listeners are, that
// shows the Reloader's certificate and echoes what it is sent.
func serveServer(t *testing.T, r *nodetls.Reloader) string {
	t.Helper()
	inner, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := tls.NewListener(inner, &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: r.GetCertificate})
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String()
}

// connect opens a connection as a client of the S3 API would, checking the
// certificate against the CA, and returns it with the serial number of the
// certificate it was shown.
func connect(t *testing.T, address string, ca *nodetls.Authority) (*tls.Conn, string, error) {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.CertPEM)
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 10 * time.Second},
		Config:    &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12},
	}
	raw, err := dialer.DialContext(t.Context(), "tcp", address)
	if err != nil {
		return nil, "", err
	}
	conn, ok := raw.(*tls.Conn)
	if !ok {
		_ = raw.Close()
		return nil, "", errors.New("not a TLS connection")
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, conn.ConnectionState().PeerCertificates[0].SerialNumber.String(), nil
}

// shown returns the serial number a new handshake is shown.
func shown(t *testing.T, address string, ca *nodetls.Authority) string {
	t.Helper()
	conn, serial, err := connect(t, address, ca)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	_ = conn.Close()
	return serial
}

// roundTrip sends a line over a connection and expects it back.
func roundTrip(t *testing.T, conn net.Conn, reader *bufio.Reader, line string) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, line+"\n"); err != nil {
		t.Fatalf("write on a connection opened before the replacement: %v", err)
	}
	got, err := reader.ReadString('\n')
	if err != nil || got != line+"\n" {
		t.Fatalf("read on a connection opened before the replacement: %q, %v", got, err)
	}
}

func serverReloader(t *testing.T, f files, cfg nodetls.ReloadConfig) *nodetls.Reloader {
	t.Helper()
	cfg.CertFile, cfg.KeyFile = f.cert, f.key
	if cfg.Clock == nil {
		cfg.Clock = porttest.NewClock(time.Now())
	}
	r, err := nodetls.LoadServer(t.Context(), cfg)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	return r
}

// TestReloadPresentsTheNewCertificateToNewHandshakes: files that have not
// changed change nothing; a new pair is shown to every handshake that begins
// after it was found, while a connection opened before goes on; a pair that
// is half written is left alone and taken when it is whole; and the loop
// ends with its context.
func TestReloadPresentsTheNewCertificateToNewHandshakes(t *testing.T) {
	t.Parallel()
	ca := authority(t)
	first, second, third := serverPair(t, ca, "localhost"), serverPair(t, ca, "localhost"), serverPair(t, ca, "localhost")
	f := newFiles(t, first)
	log, steps := &logSink{}, newStepper()
	r := serverReloader(t, f, nodetls.ReloadConfig{Interval: nodetls.MinReloadInterval, Logger: log.logger(), Tick: steps.tick})
	address := serveServer(t, r)

	loopCtx, stop := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(loopCtx)
	}()
	steps.await(t)

	early, serial, err := connect(t, address, ca)
	if err != nil || serial != serialOf(t, first.cert) {
		t.Fatalf("first handshake: serial %s, %v", serial, err)
	}
	earlyReader := bufio.NewReader(early)
	roundTrip(t, early, earlyReader, "before")

	// Nothing changed: nothing is replaced, however often it is looked at.
	steps.step(t)
	steps.step(t)
	if r.Replaced() != 0 || shown(t, address, ca) != serialOf(t, first.cert) {
		t.Errorf("unchanged files: replaced %d times", r.Replaced())
	}

	// A whole new pair.
	f.write(t, second)
	steps.step(t)
	if got := shown(t, address, ca); got != serialOf(t, second.cert) {
		t.Errorf("after the replacement a handshake was shown %s, want %s", got, serialOf(t, second.cert))
	}
	if r.Replaced() != 1 || r.Pending() != "" {
		t.Errorf("Replaced = %d, Pending = %q", r.Replaced(), r.Pending())
	}
	if !r.NotAfter().Equal(ca.NotAfter()) {
		t.Errorf("NotAfter = %s, want %s", r.NotAfter(), ca.NotAfter())
	}
	// The connection from before is the connection it was.
	roundTrip(t, early, earlyReader, "after")
	if got := early.ConnectionState().PeerCertificates[0].SerialNumber.String(); got != serialOf(t, first.cert) {
		t.Errorf("the open connection changed its certificate to %s", got)
	}

	// Half of the next pair: the certificate is written, the key is not yet.
	writeFile(t, f.cert, third.cert)
	steps.step(t)
	steps.step(t)
	steps.step(t)
	if got := shown(t, address, ca); got != serialOf(t, second.cert) {
		t.Errorf("with half a pair on disk a handshake was shown %s, want the one in use", got)
	}
	if r.Refused(nodetls.RefusedPair) != 1 || r.Pending() != nodetls.RefusedPair {
		t.Errorf("half a pair: refused %d times, pending %q; want once, however often it is looked at",
			r.Refused(nodetls.RefusedPair), r.Pending())
	}
	if warned := log.lines(`"level":"WARN"`, `"reason":"pair"`); len(warned) != 1 {
		t.Errorf("half a pair was warned of %d times, want once:\n%s", len(warned), log)
	}
	if failed := log.lines(`"level":"ERROR"`); len(failed) != 0 {
		t.Errorf("half a pair is no error:\n%s", strings.Join(failed, "\n"))
	}
	// The other half.
	writeFile(t, f.key, third.key)
	steps.step(t)
	if got := shown(t, address, ca); got != serialOf(t, third.cert) {
		t.Errorf("once the pair was whole a handshake was shown %s, want %s", got, serialOf(t, third.cert))
	}
	if r.Replaced() != 2 || r.Pending() != "" {
		t.Errorf("Replaced = %d, Pending = %q", r.Replaced(), r.Pending())
	}
	if said := log.lines(`"level":"INFO"`, "certificate replaced", serialOf(t, third.cert), serialOf(t, second.cert)); len(said) != 1 {
		t.Errorf("the replacement was not logged with both serial numbers:\n%s", log)
	}
	assertNoKeyMaterial(t, log.String(), first.key, second.key, third.key)

	stop()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return when its context was cancelled")
	}
}

// TestReloadOffNeverReadsTheFilesAgain: with an interval of zero the loop
// returns at once, and what is put on disk afterwards is not used.
func TestReloadOffNeverReadsTheFilesAgain(t *testing.T) {
	t.Parallel()
	ca := authority(t)
	first, second := serverPair(t, ca, "localhost"), serverPair(t, ca, "localhost")
	f := newFiles(t, first)
	asked := make(chan struct{}, 1)
	r := serverReloader(t, f, nodetls.ReloadConfig{Interval: 0, Tick: func(time.Duration) <-chan time.Time {
		asked <- struct{}{}
		return nil
	}})
	address := serveServer(t, r)

	f.write(t, second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(t.Context())
	}()
	select {
	case <-done:
	case <-asked:
		t.Fatal("a reloader that is off waits for a next look")
	case <-time.After(30 * time.Second):
		t.Fatal("Run with an interval of zero did not return")
	}
	if got := shown(t, address, ca); got != serialOf(t, first.cert) || r.Replaced() != 0 {
		t.Errorf("a reloader that is off replaced the certificate: shown %s", got)
	}

	// An interval that is too short, or no clock, is a mistake.
	for name, cfg := range map[string]nodetls.ReloadConfig{
		"too often":   {CertFile: f.cert, KeyFile: f.key, Interval: time.Second, Clock: porttest.NewClock(time.Now())},
		"negative":    {CertFile: f.cert, KeyFile: f.key, Interval: -time.Minute, Clock: porttest.NewClock(time.Now())},
		"no clock":    {CertFile: f.cert, KeyFile: f.key, Interval: time.Minute},
		"no key file": {CertFile: f.cert, Interval: time.Minute, Clock: porttest.NewClock(time.Now())},
	} {
		if _, err := nodetls.LoadServer(t.Context(), cfg); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("LoadServer, %s: %v, want %v", name, err, domain.ErrInvalidArgument)
		}
		id := identity(t, ca, "node-1")
		if _, err := id.Reloader(cfg); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("Reloader, %s: %v, want %v", name, err, domain.ErrInvalidArgument)
		}
	}
	// At start there must be something to serve.
	writeFile(t, f.key, first.key)
	if _, err := nodetls.LoadServer(t.Context(), nodetls.ReloadConfig{CertFile: f.cert, KeyFile: f.key, Clock: porttest.NewClock(time.Now())}); err == nil {
		t.Error("LoadServer accepted a key that is not the certificate's")
	}
	if _, err := nodetls.LoadServer(t.Context(), nodetls.ReloadConfig{
		CertFile: f.cert, KeyFile: filepath.Join(t.TempDir(), "absent.key"), Clock: porttest.NewClock(time.Now()),
	}); err == nil {
		t.Error("LoadServer accepted a key file that does not exist")
	}
}

// TestReloadRefusesACandidateAndKeepsTheOneInUse: every way in which what is
// on disk can be wrong. The certificate in use stays, the refusal is counted
// and logged once with its reason - a pair that may yet become whole as a
// warning, anything else as an error - and never with any part of a key.
// When the files are put right, they are taken.
func TestReloadRefusesACandidateAndKeepsTheOneInUse(t *testing.T) {
	t.Parallel()
	const self = "node-1"
	ca, foreign := authority(t), authority(t)
	inUse := map[string]pair{"node": nodePair(t, ca, self), "server": serverPair(t, ca, "localhost")}
	good := map[string]pair{"node": nodePair(t, ca, self), "server": serverPair(t, ca, "localhost")}
	garbage := []byte("-----BEGIN CERTIFICATE-----\nbm90IGEgY2VydGlmaWNhdGU=\n-----END CERTIFICATE-----\n")

	tests := []struct {
		name string
		kind string
		// candidate is what is put on disk; a nil part leaves that file out.
		candidate pair
		// clock is where the node's clock stands, from now.
		clock  time.Duration
		reason nodetls.ReloadRefusal
		level  string
		// says is part of what the log must say.
		says string
	}{
		{
			name: "node: the key of another certificate", kind: "node",
			candidate: pair{nodePair(t, ca, self).cert, nodePair(t, ca, self).key}, reason: nodetls.RefusedPair, level: "WARN",
		},
		{name: "node: a certificate that does not parse", kind: "node", candidate: pair{garbage, good["node"].key}, reason: nodetls.RefusedPair, level: "WARN"},
		{name: "node: an empty certificate file", kind: "node", candidate: pair{[]byte{}, good["node"].key}, reason: nodetls.RefusedPair, level: "WARN"},
		{name: "node: no key file", kind: "node", candidate: pair{good["node"].cert, nil}, reason: nodetls.RefusedUnreadable, level: "WARN"},
		{
			name: "node: a file too large to be one", kind: "node",
			candidate: pair{bytes.Repeat([]byte("x"), 2<<20), good["node"].key}, reason: nodetls.RefusedUnreadable, level: "WARN",
		},
		{
			name: "node: a certificate that names another node", kind: "node",
			candidate: nodePair(t, ca, "node-2"), reason: nodetls.RefusedIdentity, level: "ERROR", says: `names node \"node-2\", this node is \"node-1\"`,
		},
		{
			name: "node: a certificate with two names", kind: "node",
			candidate: serverPair(t, ca, self, "node-2"), reason: nodetls.RefusedIdentity, level: "ERROR",
		},
		{
			name: "node: a certificate of another CA", kind: "node",
			candidate: nodePair(t, foreign, self), reason: nodetls.RefusedUntrusted, level: "ERROR", says: "not valid under the node CA",
		},
		{
			name: "node: a certificate that is good for serving only", kind: "node",
			candidate: serverPair(t, ca, self), reason: nodetls.RefusedUntrusted, level: "ERROR",
		},
		{name: "node: an expired certificate", kind: "node", candidate: good["node"], clock: 2 * time.Hour, reason: nodetls.RefusedExpired, level: "ERROR"},
		{
			name: "node: a certificate that is not valid yet", kind: "node",
			candidate: good["node"], clock: -time.Hour, reason: nodetls.RefusedNotYetValid, level: "ERROR",
		},
		{
			name: "server: the key of another certificate", kind: "server",
			candidate: pair{good["server"].cert, inUse["server"].key}, reason: nodetls.RefusedPair, level: "WARN",
		},
		{name: "server: no certificate file", kind: "server", candidate: pair{nil, good["server"].key}, reason: nodetls.RefusedUnreadable, level: "WARN"},
		{name: "server: an expired certificate", kind: "server", candidate: good["server"], clock: 2 * time.Hour, reason: nodetls.RefusedExpired, level: "ERROR"},
		{
			name: "server: a certificate that is not valid yet", kind: "server",
			candidate: good["server"], clock: -time.Hour, reason: nodetls.RefusedNotYetValid, level: "ERROR",
		},
		{
			name: "server: a certificate for clients only", kind: "server",
			candidate: selfSigned(t, x509.ExtKeyUsageClientAuth), reason: nodetls.RefusedUsage, level: "ERROR", says: "not for server authentication",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFiles(t, inUse[tc.kind])
			log, clock := &logSink{}, porttest.NewClock(time.Now())
			cfg := nodetls.ReloadConfig{CertFile: f.cert, KeyFile: f.key, Interval: time.Minute, Clock: clock, Logger: log.logger()}
			var r *nodetls.Reloader
			var id *nodetls.Identity
			var err error
			if tc.kind == "node" {
				if id, err = nodetls.Load(f.cert, f.key, writeCA(t, ca)); err != nil {
					t.Fatalf("Load: %v", err)
				}
				r, err = id.Reloader(cfg)
			} else {
				r, err = nodetls.LoadServer(t.Context(), cfg)
			}
			if err != nil {
				t.Fatal(err)
			}
			current := func() string {
				certificate, err := r.GetCertificate(nil)
				if err != nil || certificate == nil {
					t.Fatalf("GetCertificate = %v, %v: there is always a certificate", certificate, err)
				}
				if id != nil {
					// What a peer is shown is what the reloader holds.
					if shown, err := id.Server().GetCertificate(nil); err != nil || shown != certificate {
						t.Fatalf("the node's listener shows %v, %v", shown, err)
					}
					if shown, err := id.Client(self).GetClientCertificate(nil); err != nil || shown != certificate {
						t.Fatalf("the node's dialler shows %v, %v", shown, err)
					}
				}
				return certificate.Leaf.SerialNumber.String()
			}
			want := serialOf(t, inUse[tc.kind].cert)

			clock.Advance(tc.clock)
			for path, data := range map[string][]byte{f.cert: tc.candidate.cert, f.key: tc.candidate.key} {
				if data == nil {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					continue
				}
				writeFile(t, path, data)
			}
			// Looked at three times: refused each time, said once.
			for range 3 {
				if r.Poll(t.Context()) {
					t.Fatal("the candidate was put to use")
				}
			}
			if got := current(); got != want {
				t.Errorf("the certificate in use is %s, want the one from before, %s", got, want)
			}
			if r.Pending() != tc.reason || r.Refused(tc.reason) != 1 || r.Replaced() != 0 {
				t.Errorf("Pending = %q, Refused(%s) = %d, Replaced = %d", r.Pending(), tc.reason, r.Refused(tc.reason), r.Replaced())
			}
			for _, reason := range nodetls.ReloadRefusals() {
				if reason != tc.reason && r.Refused(reason) != 0 {
					t.Errorf("Refused(%s) = %d, want 0", reason, r.Refused(reason))
				}
			}
			said := log.lines(`"level":"`+tc.level+`"`, `"reason":"`+string(tc.reason)+`"`, `"certificate":"`+tc.kind+`"`, tc.says, `"in_use_serial":"`+want+`"`)
			if len(said) != 1 || len(log.lines(`"reason"`)) != 1 {
				t.Errorf("want one %s line with reason %s and %q; the log:\n%s", tc.level, tc.reason, tc.says, log)
			}
			assertNoKeyMaterial(t, log.String(), tc.candidate.key, inUse[tc.kind].key, good[tc.kind].key)

			// Put right, and taken.
			clock.Advance(-tc.clock)
			f.write(t, good[tc.kind])
			if !r.Poll(t.Context()) {
				t.Fatalf("a good pair was not put to use:\n%s", log)
			}
			if got := current(); got != serialOf(t, good[tc.kind].cert) || r.Pending() != "" || r.Replaced() != 1 {
				t.Errorf("after a good pair: in use %s, Pending = %q, Replaced = %d", got, r.Pending(), r.Replaced())
			}
			if r.Poll(t.Context()) {
				t.Error("the pair in use was put to use again")
			}
			assertNoKeyMaterial(t, log.String(), tc.candidate.key, inUse[tc.kind].key, good[tc.kind].key)
		})
	}

	if r := nodetls.ReloadRefusals(); len(r) != 7 {
		t.Errorf("ReloadRefusals = %v", r)
	}
}

func writeCA(t *testing.T, ca *nodetls.Authority) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	writeFile(t, path, ca.CertPEM)
	return path
}

// TestReloadAcceptsACertificateThatExpiresSooner: an operator may shorten a
// lifetime on purpose. It is done, and it is said.
func TestReloadAcceptsACertificateThatExpiresSooner(t *testing.T) {
	t.Parallel()
	long, err := nodetls.NewAuthority(rand.Reader, time.Now(), 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	short := authority(t) // an hour
	first, sooner := serverPair(t, long, "localhost"), serverPair(t, short, "localhost")
	f := newFiles(t, first)
	log := &logSink{}
	r := serverReloader(t, f, nodetls.ReloadConfig{Interval: time.Minute, Logger: log.logger()})
	if !r.NotAfter().Equal(long.NotAfter()) {
		t.Fatalf("NotAfter = %s, want %s", r.NotAfter(), long.NotAfter())
	}

	f.write(t, sooner)
	if !r.Poll(t.Context()) {
		t.Fatalf("a valid certificate that expires sooner was refused:\n%s", log)
	}
	if !r.NotAfter().Equal(short.NotAfter()) {
		t.Errorf("NotAfter = %s, want %s", r.NotAfter(), short.NotAfter())
	}
	if warned := log.lines(`"level":"WARN"`, "expires sooner"); len(warned) != 1 {
		t.Errorf("a shorter lifetime was not warned of:\n%s", log)
	}
	// A certificate of no CA at all is served too: whom its clients trust is
	// not the node's to know.
	f.write(t, selfSigned(t, x509.ExtKeyUsageServerAuth))
	if !r.Poll(t.Context()) {
		t.Errorf("a self-signed server certificate was refused:\n%s", log)
	}
	// One that says nothing about its use, as well.
	f.write(t, selfSigned(t))
	if !r.Poll(t.Context()) {
		t.Errorf("a certificate without extended key usage was refused:\n%s", log)
	}
}

// TestReloadSwapsUnderConcurrentHandshakes: handshakes that run while the
// certificate is replaced, again and again, all succeed, each with one of
// the certificates there were. Run it with -race.
func TestReloadSwapsUnderConcurrentHandshakes(t *testing.T) {
	t.Parallel()
	ca := authority(t)
	pairs := []pair{serverPair(t, ca, "localhost"), serverPair(t, ca, "localhost")}
	known := map[string]bool{serialOf(t, pairs[0].cert): true, serialOf(t, pairs[1].cert): true}
	f := newFiles(t, pairs[0])
	r := serverReloader(t, f, nodetls.ReloadConfig{Interval: time.Minute})
	address := serveServer(t, r)

	stop := make(chan struct{})
	failures := make(chan error, 8)
	var clients sync.WaitGroup
	for range 8 {
		clients.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				conn, serial, err := connect(t, address, ca)
				if err == nil {
					_ = conn.Close()
					if known[serial] {
						continue
					}
					err = errors.New("a handshake was shown a certificate that never was: " + serial)
				}
				select {
				case failures <- err:
				default:
				}
				return
			}
		})
	}
	swaps := 0
	for i := 1; i <= 40; i++ {
		f.write(t, pairs[i%2])
		// A look between the two writes sees half a pair and waits; the
		// writes here are whole before each look.
		if r.Poll(t.Context()) {
			swaps++
		}
	}
	close(stop)
	clients.Wait()
	select {
	case err := <-failures:
		t.Errorf("a handshake during a replacement failed: %v", err)
	default:
	}
	if swaps != 40 || r.Replaced() != 40 {
		t.Errorf("replaced %d times, counted %d; want 40", swaps, r.Replaced())
	}
}

// serveNode accepts connections on a node's listener. To each caller it
// writes the serial number of the certificate the caller showed, then echoes.
func serveNode(t *testing.T, id *nodetls.Identity, allow func(string) bool) string {
	t.Helper()
	listener, err := id.Listen(t.Context(), "127.0.0.1:0", allow)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				secured, ok := conn.(interface{ ConnectionState() tls.ConnectionState })
				if !ok {
					return
				}
				caller := secured.ConnectionState().PeerCertificates[0].SerialNumber.String()
				if _, err := io.WriteString(conn, caller+"\n"); err != nil {
					return
				}
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String()
}

// call dials a node as another and returns the connection, a reader of it,
// the serial number of the certificate the dialled node showed, and the
// serial number it says the caller showed.
func call(t *testing.T, from *nodetls.Identity, to, address string) (net.Conn, *bufio.Reader, string, string) {
	t.Helper()
	conn, err := from.Dial(t.Context(), nodetls.Address(to, address))
	if err != nil {
		t.Fatalf("Dial %s: %v", to, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	secured, ok := conn.(*tls.Conn)
	if !ok {
		t.Fatal("Dial did not return a TLS connection")
	}
	state := secured.ConnectionState()
	if peer, err := nodetls.PeerNodeID(state); err != nil || peer != to || state.Version != tls.VersionTLS13 {
		t.Fatalf("dialled %s, reached %q over TLS %x, %v", to, peer, state.Version, err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	seen, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read from %s: %v", to, err)
	}
	return conn, reader, state.PeerCertificates[0].SerialNumber.String(), strings.TrimSpace(seen)
}

// eventually waits a short while for what another goroutine is about to do.
func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRotatedNodeCertificateIsWhatPeersSee: two nodes over mutual TLS. When
// A's certificate is replaced, B's new connections to A verify the new one
// and A's new connections to B present it, and a connection from before goes
// on. A node at A's address whose certificate names B is refused by B - and
// A itself refuses to load such a certificate.
func TestRotatedNodeCertificateIsWhatPeersSee(t *testing.T) {
	t.Parallel()
	ca := authority(t)
	first, second := nodePair(t, ca, "meta-1"), nodePair(t, ca, "meta-2")
	renewed := nodePair(t, ca, "meta-1")
	f := newFiles(t, first)
	a, err := nodetls.Load(f.cert, f.key, writeCA(t, ca))
	if err != nil {
		t.Fatal(err)
	}
	b, err := nodetls.New(second.cert, second.key, ca.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	log := &logSink{}
	r, err := a.Reloader(nodetls.ReloadConfig{
		CertFile: f.cert, KeyFile: f.key, Interval: time.Minute, Clock: porttest.NewClock(time.Now()), Logger: log.logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	everyone := func(string) bool { return true }
	addressA, addressB := serveNode(t, a, everyone), serveNode(t, b, everyone)

	early, earlyReader, shownByA, _ := call(t, b, "meta-1", addressA)
	if _, _, _, presented := call(t, a, "meta-2", addressB); shownByA != serialOf(t, first.cert) || presented != serialOf(t, first.cert) {
		t.Fatalf("before the replacement: A showed %s and presented %s, want %s", shownByA, presented, serialOf(t, first.cert))
	}

	f.write(t, renewed)
	if !r.Poll(t.Context()) {
		t.Fatalf("the renewed certificate was refused:\n%s", log)
	}
	if !a.NotAfter().Equal(ca.NotAfter()) || a.NodeID() != "meta-1" {
		t.Errorf("after the replacement: NotAfter = %s, NodeID = %s", a.NotAfter(), a.NodeID())
	}
	// B, dialling A anew, verifies A's new certificate...
	if _, _, shownByA, _ := call(t, b, "meta-1", addressA); shownByA != serialOf(t, renewed.cert) {
		t.Errorf("B's new connection to A was shown %s, want %s", shownByA, serialOf(t, renewed.cert))
	}
	// ...and A, dialling B anew, presents it.
	if _, _, _, presented := call(t, a, "meta-2", addressB); presented != serialOf(t, renewed.cert) {
		t.Errorf("A's new connection to B presented %s, want %s", presented, serialOf(t, renewed.cert))
	}
	// The connection B opened before is not torn down.
	roundTrip(t, early, earlyReader, "still here")

	// A does not load a certificate that names B: its peers never see it.
	f.write(t, nodePair(t, ca, "meta-2"))
	if r.Poll(t.Context()) || r.Refused(nodetls.RefusedIdentity) != 1 {
		t.Errorf("A loaded a certificate that names B; refused for identity %d times", r.Refused(nodetls.RefusedIdentity))
	}
	if _, _, shownByA, _ := call(t, b, "meta-1", addressA); shownByA != serialOf(t, renewed.cert) {
		t.Errorf("after the refusal A showed %s, want the one in use, %s", shownByA, serialOf(t, renewed.cert))
	}
	// And had it not refused: a node that answers for meta-1 with a
	// certificate that names meta-2 is refused by B, whoever it is.
	impostor := identity(t, ca, "meta-2")
	if conn, err := b.Dial(t.Context(), nodetls.Address("meta-1", serveNode(t, impostor, everyone))); err == nil {
		_ = conn.Close()
		t.Error("B accepted, as meta-1, a node whose certificate names meta-2")
	}
	if got := b.PeersRefused(nodetls.PeerRefusedIdentity); got != 1 {
		t.Errorf("B counted %d peers refused for their identity, want 1", got)
	}
	// A node of another CA is refused from both ends, and counted.
	stranger := identity(t, authority(t), "meta-1")
	if conn, err := b.Dial(t.Context(), nodetls.Address("meta-1", serveNode(t, stranger, everyone))); err == nil {
		_ = conn.Close()
		t.Error("B accepted a node of another CA")
	}
	if got := b.PeersRefused(nodetls.PeerRefusedUntrusted); got != 1 {
		t.Errorf("B counted %d peers refused as untrusted when dialling, want 1", got)
	}
	// The caller trusts B, as a node of B's cluster would, and shows a
	// certificate of the other CA.
	theirs := nodePair(t, authority(t), "meta-1")
	shownByCaller, err := tls.X509KeyPair(theirs.cert, theirs.key)
	if err != nil {
		t.Fatal(err)
	}
	callerConfig := b.Client("meta-2")
	callerConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &shownByCaller, nil }
	if conn, err := (&tls.Dialer{Config: callerConfig}).DialContext(t.Context(), "tcp", addressB); err == nil {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		if n, err := conn.Read(make([]byte, 1)); err == nil || n > 0 {
			t.Error("a node of another CA was served by B")
		}
		_ = conn.Close()
	}
	eventually(t, "B to count the caller of another CA", func() bool { return b.PeersRefused(nodetls.PeerRefusedUntrusted) == 2 })
	// A listener that admits only some turns the others away, and counts.
	voters := serveNode(t, b, func(nodeID string) bool { return nodeID == "meta-3" })
	if conn, err := a.Dial(t.Context(), nodetls.Address("meta-2", voters)); err == nil {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		if n, err := conn.Read(make([]byte, 1)); err == nil || n > 0 {
			t.Error("a node the listener does not admit was served")
		}
		_ = conn.Close()
	}
	eventually(t, "B to count the node it does not admit", func() bool { return b.PeersRefused(nodetls.PeerRefusedNotAdmitted) == 1 })
	// A caller that is no TLS client at all.
	raw, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addressB)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(raw, "GET / HTTP/1.0\r\n\r\n")
	eventually(t, "B to count the caller that spoke no TLS", func() bool { return b.PeersRefused(nodetls.PeerRefusedHandshake) == 1 })
	_ = raw.Close()
	// A caller that trusts B and shows no certificate at all.
	bare := b.Client("meta-2")
	bare.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &tls.Certificate{}, nil }
	if conn, err := (&tls.Dialer{Config: bare}).DialContext(t.Context(), "tcp", addressB); err == nil {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		if n, err := conn.Read(make([]byte, 1)); err == nil || n > 0 {
			t.Error("a caller without a certificate was served by B")
		}
		_ = conn.Close()
	}
	eventually(t, "B to count the caller without a certificate", func() bool {
		return b.PeersRefused(nodetls.PeerRefusedNoCertificate) == 1
	})
	if got := b.PeersRefused(nodetls.PeerRefusedHandshake); got != 1 {
		t.Errorf("B counted %d handshakes that failed for no reason it names, want 1", got)
	}
	if b.PeersRefused("no such reason") != 0 || r.Refused("no such reason") != 0 || len(nodetls.PeerRefusals()) != 6 {
		t.Error("a reason that does not exist was counted")
	}
	assertNoKeyMaterial(t, log.String(), first.key, renewed.key, second.key)
}

// TestReloadDoesNotHoldUpAccessorsOrShutdown: a reading that does not end -
// a mount that hangs. Whoever asks the Reloader something is answered, a
// second look gives up with its context, Poll and Run return when theirs is
// cancelled, and when the reading does end the files are looked at again.
func TestReloadDoesNotHoldUpAccessorsOrShutdown(t *testing.T) {
	t.Parallel()
	ca := authority(t)
	first, second := serverPair(t, ca, "localhost"), serverPair(t, ca, "localhost")
	f := newFiles(t, first)
	steps := newStepper()
	r := serverReloader(t, f, nodetls.ReloadConfig{Interval: nodetls.MinReloadInterval, Tick: steps.tick})
	entered, release := make(chan struct{}, 16), make(chan struct{})
	r.ReadWith(func(path string) ([]byte, error) {
		entered <- struct{}{}
		<-release
		return os.ReadFile(filepath.Clean(path))
	})
	f.write(t, second)
	prompt := func(what string, done <-chan struct{}) {
		t.Helper()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s waited for a reading that does not end", what)
		}
	}

	pollCtx, stopPoll := context.WithCancel(t.Context())
	polled := make(chan bool, 1)
	go func() { polled <- r.Poll(pollCtx) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("Poll did not read the files")
	}

	// Every accessor answers while the reading hangs.
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		if r.Pending() != "" || r.Replaced() != 0 || r.Refused(nodetls.RefusedUnreadable) != 0 || r.NotAfter().IsZero() {
			t.Error("a reading in progress changed what the Reloader says")
		}
		if certificate, err := r.GetCertificate(nil); err != nil || certificate == nil {
			t.Errorf("GetCertificate = %v, %v", certificate, err)
		}
	}()
	prompt("an accessor", answered)

	// A second look waits for the first, and no longer than its context.
	brief, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	gaveUp := make(chan struct{})
	go func() {
		defer close(gaveUp)
		if r.Poll(brief) {
			t.Error("a second look replaced the certificate while the first was reading")
		}
	}()
	prompt("a second look", gaveUp)

	// The loop, too, ends with its context while the reading hangs.
	loopCtx, stopLoop := context.WithCancel(t.Context())
	looped := make(chan struct{})
	go func() {
		defer close(looped)
		r.Run(loopCtx)
	}()
	steps.await(t)
	select {
	case steps.ticks <- time.Now():
	case <-time.After(30 * time.Second):
		t.Fatal("the reloader did not take its tick")
	}
	stopLoop()
	prompt("Run, after its context was cancelled,", looped)

	stopPoll()
	select {
	case replaced := <-polled:
		if replaced {
			t.Error("a look that was given up replaced the certificate")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Poll did not return when its context was cancelled")
	}
	if r.Replaced() != 0 {
		t.Errorf("Replaced = %d with no reading ended", r.Replaced())
	}

	// The reading ends: what it found is dropped, and the next look is free.
	close(release)
	if !r.Poll(t.Context()) || r.Replaced() != 1 || r.Pending() != "" {
		t.Errorf("after the reading ended: Replaced = %d, Pending = %q", r.Replaced(), r.Pending())
	}
}

// TestReloadLogsAtStartWhatAReplacementWouldBeRefusedFor: the pair the node
// starts with is held to what a replacement is held to. When it fails, the
// node says so as an error - and starts, and serves it: nothing is refused.
func TestReloadLogsAtStartWhatAReplacementWouldBeRefusedFor(t *testing.T) {
	t.Parallel()
	ca := authority(t)
	tests := []struct {
		name  string
		pair  pair
		clock time.Duration
		// reason is what the log must name; none when it must say nothing.
		reason nodetls.ReloadRefusal
	}{
		{name: "a good certificate", pair: serverPair(t, ca, "localhost")},
		{name: "a certificate without extended key usage", pair: selfSigned(t)},
		{name: "an expired certificate", pair: serverPair(t, ca, "localhost"), clock: 2 * time.Hour, reason: nodetls.RefusedExpired},
		{name: "a certificate that is not valid yet", pair: serverPair(t, ca, "localhost"), clock: -time.Hour, reason: nodetls.RefusedNotYetValid},
		{name: "a certificate for clients only", pair: selfSigned(t, x509.ExtKeyUsageClientAuth), reason: nodetls.RefusedUsage},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFiles(t, tc.pair)
			log := &logSink{}
			r, err := nodetls.LoadServer(t.Context(), nodetls.ReloadConfig{
				CertFile: f.cert, KeyFile: f.key, Interval: time.Minute,
				Clock: porttest.NewClock(time.Now().Add(tc.clock)), Logger: log.logger(),
			})
			if err != nil {
				t.Fatalf("LoadServer: %v: the node must start", err)
			}
			if certificate, err := r.GetCertificate(nil); err != nil || certificate.Leaf.SerialNumber.String() != serialOf(t, tc.pair.cert) {
				t.Errorf("the pair read at start is not served: %v", err)
			}
			if r.Pending() != "" || r.Replaced() != 0 {
				t.Errorf("Pending = %q, Replaced = %d: the pair on disk is the one in use", r.Pending(), r.Replaced())
			}
			for _, reason := range nodetls.ReloadRefusals() {
				if r.Refused(reason) != 0 {
					t.Errorf("Refused(%s) = %d: nothing was refused", reason, r.Refused(reason))
				}
			}
			failed := log.lines(`"level":"ERROR"`)
			if tc.reason == "" {
				if len(log.lines(`"level"`)) != 0 {
					t.Errorf("a good certificate at start was remarked on:\n%s", log)
				}
				return
			}
			said := log.lines(`"level":"ERROR"`, "read at start", `"reason":"`+string(tc.reason)+`"`, `"certificate":"server"`,
				`"serial":"`+serialOf(t, tc.pair.cert)+`"`)
			if len(said) != 1 || len(failed) != 1 {
				t.Errorf("want one ERROR line with reason %s; the log:\n%s", tc.reason, log)
			}
			// Looked at again, the same files are the ones in use: no more is said.
			if r.Poll(t.Context()) || len(log.lines(`"level"`)) != 1 {
				t.Errorf("the pair in use was judged again:\n%s", log)
			}
			assertNoKeyMaterial(t, log.String(), tc.pair.key)
		})
	}
}

// TestReloadSaysAgainARefusalThatStands: a refusal is counted once and logged
// once - and logged again, at the level it had, every hour by the node's
// clock for as long as the files are what they were, so that the one line
// cannot leave the log unnoticed. When the files are put right it stops.
func TestReloadSaysAgainARefusalThatStands(t *testing.T) {
	t.Parallel()
	const self = "node-1"
	// Long-lived, so that hours on the clock expire nothing.
	ca, err := nodetls.NewAuthority(rand.Reader, time.Now(), 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		// spoil puts on disk what is refused.
		spoil  func(t *testing.T, f files)
		reason nodetls.ReloadRefusal
		level  string
	}{
		{
			name:   "half a pair, a warning",
			spoil:  func(t *testing.T, f files) { writeFile(t, f.cert, nodePair(t, ca, self).cert) },
			reason: nodetls.RefusedPair, level: "WARN",
		},
		{
			name:   "a certificate that names another node, an error",
			spoil:  func(t *testing.T, f files) { f.write(t, nodePair(t, ca, "node-2")) },
			reason: nodetls.RefusedIdentity, level: "ERROR",
		},
		{
			name: "no key file, a warning",
			spoil: func(t *testing.T, f files) {
				if err := os.Remove(f.key); err != nil {
					t.Fatal(err)
				}
			},
			reason: nodetls.RefusedUnreadable, level: "WARN",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFiles(t, nodePair(t, ca, self))
			id, err := nodetls.Load(f.cert, f.key, writeCA(t, ca))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			log, clock := &logSink{}, porttest.NewClock(time.Now())
			r, err := id.Reloader(nodetls.ReloadConfig{CertFile: f.cert, KeyFile: f.key, Interval: time.Minute, Clock: clock, Logger: log.logger()})
			if err != nil {
				t.Fatal(err)
			}
			said := func() int {
				return len(log.lines(`"level":"`+tc.level+`"`, `"reason":"`+string(tc.reason)+`"`))
			}
			// look looks at the files a few times and tells how often the
			// refusal has been logged by then.
			look := func() int {
				for range 3 {
					if r.Poll(t.Context()) {
						t.Fatal("what is refused was put to use")
					}
				}
				return said()
			}

			tc.spoil(t, f)
			if got := look(); got != 1 {
				t.Fatalf("logged %d times at first, want once:\n%s", got, log)
			}
			clock.Advance(nodetls.RefusalReminderInterval - time.Second)
			if got := look(); got != 1 {
				t.Errorf("logged %d times within the hour, want once:\n%s", got, log)
			}
			clock.Advance(time.Second)
			if got := look(); got != 2 {
				t.Errorf("logged %d times after an hour, want twice:\n%s", got, log)
			}
			if again := log.lines(`"reason":"`+string(tc.reason)+`"`, `"refused_since"`); len(again) != 1 {
				t.Errorf("the reminder does not say since when:\n%s", log)
			}
			clock.Advance(3 * nodetls.RefusalReminderInterval)
			if got := look(); got != 3 {
				t.Errorf("logged %d times after hours without a look, want three times:\n%s", got, log)
			}
			if r.Refused(tc.reason) != 1 || r.Pending() != tc.reason || len(log.lines(`"reason"`)) != 3 {
				t.Errorf("Refused(%s) = %d, Pending = %q: a reminder is no new refusal\n%s", tc.reason, r.Refused(tc.reason), r.Pending(), log)
			}

			// Put right: taken, and nothing more is said of it.
			f.write(t, nodePair(t, ca, self))
			if !r.Poll(t.Context()) {
				t.Fatalf("a good pair was not put to use:\n%s", log)
			}
			clock.Advance(2 * nodetls.RefusalReminderInterval)
			if r.Poll(t.Context()) || said() != 3 || r.Pending() != "" {
				t.Errorf("a refusal that no longer stands was said again:\n%s", log)
			}
		})
	}
}

// TestPeerRefusalOf: a handshake that failed is sorted by what failed it. A
// caller that was too slow is told from one that showed nothing, and both
// from one whose certificate was judged.
func TestPeerRefusalOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want nodetls.PeerRefusal
	}{
		{name: "the time was up", err: context.DeadlineExceeded, want: nodetls.PeerRefusedTimeout},
		{name: "the time was up, wrapped", err: fmt.Errorf("handshake: %w", context.DeadlineExceeded), want: nodetls.PeerRefusedTimeout},
		{name: "a read ran into its deadline", err: os.ErrDeadlineExceeded, want: nodetls.PeerRefusedTimeout},
		{name: "a network timeout", err: &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}, want: nodetls.PeerRefusedTimeout},
		{name: "no certificate", err: errors.New("tls: client didn't provide a certificate"), want: nodetls.PeerRefusedNoCertificate},
		{name: "closed half-way", err: io.EOF, want: nodetls.PeerRefusedHandshake},
		{name: "no TLS", err: tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, want: nodetls.PeerRefusedHandshake},
		{
			name: "another CA", err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}},
			want: nodetls.PeerRefusedUntrusted,
		},
		{
			name: "another node", err: &tls.CertificateVerificationError{Err: x509.HostnameError{Certificate: &x509.Certificate{}, Host: "meta-1"}},
			want: nodetls.PeerRefusedIdentity,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := nodetls.PeerRefusalOf(tc.err); got != tc.want {
				t.Errorf("PeerRefusalOf(%v) = %q, want %q", tc.err, got, tc.want)
			}
			if !slices.Contains(nodetls.PeerRefusals(), tc.want) {
				t.Errorf("%q is not one of PeerRefusals", tc.want)
			}
		})
	}
}
