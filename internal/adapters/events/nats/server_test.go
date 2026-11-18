package nats

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/egress"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// answer is how the fake server answers a publish.
type answer int

const (
	answerAck      answer = iota // the stream's acknowledgement
	answerNoStream               // a 503: no responders
	answerNothing                // silence
	answerDenied                 // a permissions violation
)

// fakeServer speaks enough of the NATS protocol for a client to connect,
// over TLS, and publish one message after another.
type fakeServer struct {
	listener net.Listener
	tls      *tls.Config
	// plain offers no TLS at all.
	plain bool
	// token is the token the server takes; empty takes any.
	token   string
	publish answer

	mu        sync.Mutex
	connects  []map[string]any
	published []published
	// clearText is everything read before TLS began.
	clearText strings.Builder
}

type published struct {
	subject string
	header  string
	payload string
}

// newFakeServer starts a server on loopback with a certificate for
// 127.0.0.1, and returns it with the certificate as PEM.
func newFakeServer(t *testing.T, configure func(*testing.T, *fakeServer)) (*fakeServer, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "nats test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	l, err := listen(t)
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{
		listener: l,
		tls:      &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}},
	}
	if configure != nil {
		configure(t, s)
	}
	t.Cleanup(func() { _ = l.Close() })
	go s.serve(t.Context())
	return s, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func (s *fakeServer) endpoint() string {
	return "tls://" + s.listener.Addr().String()
}

// listen listens on a free port of loopback.
func listen(t *testing.T) (net.Listener, error) {
	t.Helper()
	var config net.ListenConfig
	l, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	return l, nil
}

func (s *fakeServer) serve(ctx context.Context) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(ctx, conn)
	}
}

func (s *fakeServer) handle(ctx context.Context, raw net.Conn) {
	defer func() { _ = raw.Close() }()
	info := map[string]any{
		"server_id": "fake", "version": "2.11.0", "proto": 1, "headers": true,
		"max_payload": 1 << 20, "tls_required": !s.plain,
	}
	line, err := json.Marshal(info)
	if err != nil {
		return
	}
	if _, err := fmt.Fprintf(raw, "INFO %s\r\n", line); err != nil {
		return
	}
	var conn io.ReadWriter
	if s.plain {
		// Whatever the client sends now is sent in the clear.
		tee := io.TeeReader(raw, writerFunc(func(p []byte) {
			s.mu.Lock()
			s.clearText.Write(p)
			s.mu.Unlock()
		}))
		conn = struct {
			io.Reader
			io.Writer
		}{tee, raw}
	} else {
		secured := tls.Server(raw, s.tls)
		if err := secured.HandshakeContext(ctx); err != nil {
			// The alert is sent; what the client writes meanwhile is
			// read, until it closes, so that it reads the alert - a
			// close now would reset its write first on Windows.
			_ = raw.SetReadDeadline(time.Now().Add(10 * time.Second))
			_, _ = io.Copy(io.Discard, raw)
			return
		}
		conn = secured
	}
	r := bufio.NewReader(conn)
	sid := ""
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, rest, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "CONNECT":
			var connect map[string]any
			if err := json.Unmarshal([]byte(rest), &connect); err != nil {
				return
			}
			s.mu.Lock()
			s.connects = append(s.connects, connect)
			s.mu.Unlock()
			if s.token != "" && connect["auth_token"] != s.token {
				_, _ = io.WriteString(conn, "-ERR 'Authorization Violation'\r\n")
				// What the client sent after its CONNECT (its PING) is
				// read until it closes, as after a refused handshake: a
				// close now, with that unread, resets the connection, and
				// on Windows the reset discards the refusal before the
				// client reads it.
				_ = raw.SetReadDeadline(time.Now().Add(10 * time.Second))
				_, _ = io.Copy(io.Discard, r)
				return
			}
		case "PING":
			if _, err := io.WriteString(conn, "PONG\r\n"); err != nil {
				return
			}
		case "SUB":
			fields := strings.Fields(rest)
			sid = fields[len(fields)-1]
		case "HPUB":
			fields := strings.Fields(rest)
			if len(fields) != 4 {
				return
			}
			headerLen, _ := strconv.Atoi(fields[2])
			total, _ := strconv.Atoi(fields[3])
			body := make([]byte, total+2)
			if _, err := io.ReadFull(r, body); err != nil {
				return
			}
			s.mu.Lock()
			s.published = append(s.published, published{
				subject: fields[0], header: string(body[:headerLen]), payload: string(body[headerLen:total]),
			})
			s.mu.Unlock()
			reply := fields[1]
			switch s.publish {
			case answerAck:
				ack := `{"stream":"EVENTS","seq":1}`
				_, err = fmt.Fprintf(conn, "MSG %s %s %d\r\n%s\r\n", reply, sid, len(ack), ack)
			case answerNoStream:
				status := "NATS/1.0 503\r\n\r\n"
				_, err = fmt.Fprintf(conn, "HMSG %s %s %d %d\r\n%s\r\n", reply, sid, len(status), len(status), status)
			case answerNothing:
			case answerDenied:
				_, err = fmt.Fprintf(conn, "-ERR 'Permissions Violation for Publish to \"%s\"'\r\n", fields[0])
			}
			if err != nil {
				return
			}
		}
	}
}

type writerFunc func([]byte)

func (f writerFunc) Write(p []byte) (int, error) {
	f(p)
	return len(p), nil
}

// loopbackDial returns the dial function of an egress client that admits
// loopback, and counts its dials.
func loopbackDial(t *testing.T) (egress.DialFunc, *atomic.Int64) {
	t.Helper()
	_, dial, err := egress.NewClient(egress.Policy{Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	if err != nil {
		t.Fatal(err)
	}
	var dials atomic.Int64
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		return dial(ctx, network, address)
	}, &dials
}

// serverTarget is a target of the fake server, proved with markedValue.
func serverTarget(endpoint string, ca []byte) (delivery.Target, *fakeSecrets) {
	target := natsTarget()
	target.Endpoint, target.CAPEM = endpoint, ca
	return target, &fakeSecrets{plain: map[string]string{"sealed": markedValue}}
}

// assertNothingTold fails when err tells the secret anywhere, or the
// endpoint in its text.
func assertNothingTold(t *testing.T, err error, endpoint string) {
	t.Helper()
	address := strings.TrimPrefix(endpoint, "tls://")
	for _, text := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err)} {
		if strings.Contains(text, address) || strings.Contains(text, markedValue) {
			t.Errorf("the error tells the endpoint or the secret: %q", text)
		}
	}
	if strings.Contains(chainText(err), markedValue) {
		t.Errorf("an error the failure wraps tells the secret: %q", chainText(err))
	}
}

func TestPublishThroughTheServer(t *testing.T) {
	t.Parallel()
	server, ca := newFakeServer(t, func(_ *testing.T, s *fakeServer) { s.token = markedValue })
	target, secrets := serverTarget(server.endpoint(), ca)
	dial, dials := loopbackDial(t)
	p, err := New(target, secrets, dial)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, id := range []string{"00000000000000000001", "00000000000000000002"} {
		e := ports.Event{ID: id, Type: ports.MessageNotificationObject, Data: json.RawMessage(`{"Records":[{"eventName":"ObjectCreated:Put"}]}`)}
		if err := p.Publish(ctx, e); err != nil {
			t.Fatalf("Publish(%s) = %v", id, err)
		}
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if dials.Load() != 1 || len(server.connects) != 1 {
		t.Errorf("%d dials and %d connections, want one of each", dials.Load(), len(server.connects))
	}
	if len(server.published) != 2 {
		t.Fatalf("%d messages published, want 2", len(server.published))
	}
	for i, m := range server.published {
		if m.subject != "cactusfs.notifications" || m.payload != `{"Records":[{"eventName":"ObjectCreated:Put"}]}` {
			t.Errorf("message %d: %q %q", i, m.subject, m.payload)
		}
		id := fmt.Sprintf("0000000000000000000%d", i+1)
		if !strings.Contains(m.header, "Nats-Msg-Id: "+id+"\r\n") ||
			!strings.Contains(m.header, "Cactusfs-Event-Type: "+ports.MessageNotificationObject+"\r\n") {
			t.Errorf("message %d: headers %q", i, m.header)
		}
	}
	if name := server.connects[0]["name"]; name != "cactusfs" {
		t.Errorf("the client is named %v", name)
	}
}

func TestPublishFailsAsTheServerAnswers(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		configure func(*testing.T, *fakeServer)
		// timeout is the deadline of the attempt; the connection is
		// made first, under a deadline of its own, so that a deadline
		// that ends the attempt is never one that ended the connect.
		timeout time.Duration
		want    delivery.Class
		// published is how many messages the server read.
		published int
	}{
		"the token is refused": {
			configure: func(_ *testing.T, s *fakeServer) { s.token = "another-token" },
			want:      delivery.ClassAuth,
		},
		"no stream takes the subject": {
			configure: func(_ *testing.T, s *fakeServer) { s.publish = answerNoStream },
			want:      delivery.ClassNoStream,
			// The first attempt and the client's two retries.
			published: 3,
		},
		"no acknowledgement comes": {
			configure: func(_ *testing.T, s *fakeServer) { s.publish = answerNothing },
			timeout:   300 * time.Millisecond,
			want:      delivery.ClassTimeout,
			published: 1,
		},
		"publishing is not permitted": {
			configure: func(_ *testing.T, s *fakeServer) { s.publish = answerDenied },
			timeout:   500 * time.Millisecond,
			want:      delivery.ClassAuth,
			published: 1,
		},
		"the certificate is not the target's": {
			configure: func(t *testing.T, s *fakeServer) {
				other, _ := newFakeServer(t, nil)
				s.tls = other.tls
			},
			want: delivery.ClassTLS,
		},
		"the server requires a client certificate": {
			configure: func(_ *testing.T, s *fakeServer) {
				// In TLS 1.3 the client's handshake is over before the
				// server looks at its certificate; the refusal is an
				// alert on the first read.
				s.tls.MinVersion = tls.VersionTLS13
				s.tls.ClientAuth = tls.RequireAnyClientCert
			},
			want: delivery.ClassTLS,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server, ca := newFakeServer(t, tc.configure)
			target, secrets := serverTarget(server.endpoint(), ca)
			dial, _ := loopbackDial(t)
			p, err := New(target, secrets, dial)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			if tc.timeout != 0 {
				// Connect first, with a message the server is not
				// asked to acknowledge: the attempt below then waits
				// for nothing but its acknowledgement.
				s, err := p.connect(t.Context())
				if err != nil {
					t.Fatalf("connect = %v", err)
				}
				if err := p.keep(s); err != nil {
					t.Fatal(err)
				}
			}
			timeout := tc.timeout
			if timeout == 0 {
				timeout = 5 * time.Second
			}
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			err = p.Publish(ctx, event("00000000000000000001"))
			server.mu.Lock()
			published := len(server.published)
			server.mu.Unlock()
			if published != tc.published {
				t.Errorf("the server read %d messages, want %d", published, tc.published)
			}
			if got := delivery.ClassOf(err); got != tc.want {
				t.Fatalf("class %q, want %q (%v)", got, tc.want, chainText(err))
			}
			assertNothingTold(t, err, server.endpoint())
		})
	}
}

func TestNoCredentialToAServerWithoutTLS(t *testing.T) {
	t.Parallel()
	server, ca := newFakeServer(t, func(_ *testing.T, s *fakeServer) { s.plain = true })
	target, secrets := serverTarget(server.endpoint(), ca)
	dial, _ := loopbackDial(t)
	p, err := New(target, secrets, dial)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = p.Publish(ctx, event("1"))
	if got := delivery.ClassOf(err); got != delivery.ClassTLS {
		t.Fatalf("class %q, want tls (%v)", got, chainText(err))
	}
	assertNothingTold(t, err, server.endpoint())
	// The client may close the socket before the server has read what
	// little it sent; what it sent is what counts.
	time.Sleep(50 * time.Millisecond)
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.connects) != 0 || strings.Contains(server.clearText.String(), markedValue) {
		t.Errorf("the credential was sent in the clear: %q", server.clearText.String())
	}
}

func TestNewDoesNotConnect(t *testing.T) {
	t.Parallel()
	// An address nothing listens on: a port that was open and is closed.
	l, err := listen(t)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "tls://" + l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	target, secrets := serverTarget(endpoint, nil)
	dial, dials := loopbackDial(t)

	start := time.Now()
	p, err := New(target, secrets, dial)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("New took %s", elapsed)
	}
	if dials.Load() != 0 || secrets.calls != 0 {
		t.Fatalf("New dialled %d times and opened the secret %d times", dials.Load(), secrets.calls)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = p.Publish(ctx, event("1"))
	if got := delivery.ClassOf(err); got != delivery.ClassConnect {
		t.Fatalf("class %q, want connect (%v)", got, chainText(err))
	}
	if dials.Load() == 0 {
		t.Error("Publish did not dial through the egress dialer")
	}
	assertNothingTold(t, err, endpoint)
}

func TestPublishRefusedByTheEgressPolicy(t *testing.T) {
	t.Parallel()
	server, ca := newFakeServer(t, nil)
	target, secrets := serverTarget(server.endpoint(), ca)
	// The policy admits no loopback.
	_, dial, err := egress.NewClient(egress.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(target, secrets, dial)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = p.Publish(ctx, event("1"))
	if got := delivery.ClassOf(err); got != delivery.ClassEgressRefused {
		t.Fatalf("class %q, want egress_refused (%v)", got, chainText(err))
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.connects) != 0 {
		t.Error("connected to an address the policy refuses")
	}
}

// The marked values are never in a printed publisher or target either.
func TestPrintedPublisherTellsNothing(t *testing.T) {
	t.Parallel()
	target, secrets := serverTarget("tls://marked-host.example.com:4222", nil)
	target.Secret = iam.SealedSecret{Ciphertext: []byte(markedValue)}
	p, err := New(target, secrets, noDial)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{p.String(), fmt.Sprintf("%v", p), fmt.Sprintf("%+v", p)} {
		if strings.Contains(text, "marked-host") || strings.Contains(text, markedValue) {
			t.Errorf("printed: %q", text)
		}
	}
}
