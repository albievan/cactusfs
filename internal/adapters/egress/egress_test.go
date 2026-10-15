package egress

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain/delivery"
)

// testCA is a certificate authority of a test, and the certificates it
// issues.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t *testing.T) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "egress test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return testCA{cert: cert, key: key, pool: pool}
}

// issue returns a certificate for target.test and 127.0.0.1.
func (ca testCA) issue(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "target.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"target.test"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// serve starts a TLS server on 127.0.0.1 with a certificate of ca. config,
// when not nil, is the server's TLS configuration otherwise.
func serve(t *testing.T, ca testCA, config *tls.Config, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	if config == nil {
		config = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	config.Certificates = []tls.Certificate{ca.issue(t)}
	server.TLS = config
	// A handshake a test makes fail is logged by the server; it is meant.
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

// portOf returns the port of a server's address.
func portOf(t *testing.T, address string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(address, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// names is a resolver of a test: the addresses it answers for each name.
type names map[string][]netip.Addr

// LookupNetIP implements Resolver.
func (n names) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	addrs, ok := n[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return addrs, nil
}

func loopback() netip.Addr { return netip.MustParseAddr("127.0.0.1") }

func metadataService() netip.Addr { return netip.MustParseAddr("169.254.169.254") }

func newClient(t *testing.T, policy Policy) (*Client, DialFunc) {
	t.Helper()
	client, dial, err := NewClient(policy)
	if err != nil {
		t.Fatal(err)
	}
	return client, dial
}

// answer is what a test is told of a response: its status, the length it
// declared and its body as far as it was read. The status is 0 when there
// was no response.
type answer struct {
	status        int
	contentLength int64
	body          []byte
}

// get sends GET url with client, reads the body of the response and closes
// it, and returns what it got and the error.
func get(t *testing.T, client *Client, url string) (answer, error) {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(r)
	if err != nil {
		return answer{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return answer{status: resp.StatusCode, contentLength: resp.ContentLength, body: body}, err
}

// TestRefusedAtConnect: an address the policy does not admit is refused in
// the Control hook, as it is dialled - a name that resolves to it gains
// nothing - and nothing reaches the listener behind it; the same address,
// allowed, connects.
func TestRefusedAtConnect(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	var hits atomic.Int64
	server := serve(t, ca, nil, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	port := portOf(t, server.URL)
	resolver := names{
		"target.test":   {loopback()},
		"metadata.test": {metadataService()},
	}

	tests := []struct {
		name  string
		allow []string
		url   string
	}{
		{name: "a name that resolves to loopback", url: "https://target.test:" + port + "/"},
		{name: "loopback as a literal", url: "https://127.0.0.1:" + port + "/"},
		{name: "loopback written as IPv6", url: "https://[::ffff:127.0.0.1]:" + port + "/"},
		{name: "loopback allowed for IPv6 only", allow: []string{"::1/128"}, url: "https://target.test:" + port + "/"},
		{name: "the metadata service, its range allowed", allow: []string{"169.254.0.0/16"}, url: "https://metadata.test:" + port + "/"},
		{name: "the metadata service, everything allowed", allow: []string{"0.0.0.0/0", "::/0"}, url: "https://metadata.test:" + port + "/"},
		{name: "the metadata service as a literal", allow: []string{"0.0.0.0/0"}, url: "https://169.254.169.254:" + port + "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			allow := make([]netip.Prefix, 0, len(tt.allow))
			for _, text := range tt.allow {
				allow = append(allow, netip.MustParsePrefix(text))
			}
			client, dial := newClient(t, Policy{Allow: allow, RootCAs: ca.pool, Resolver: resolver})
			started := time.Now()
			got, err := get(t, client, tt.url)
			if got.status != 0 {
				t.Fatalf("a response: %d", got.status)
			}
			if !errors.Is(err, ErrRefused) || delivery.ClassOf(err) != delivery.ClassEgressRefused {
				t.Fatalf("GET = %v (class %q), want %v", err, delivery.ClassOf(err), ErrRefused)
			}
			// Refused before a packet is sent: never the connect timeout.
			if elapsed := time.Since(started); elapsed >= ConnectTimeout {
				t.Errorf("refused after %s", elapsed)
			}
			host := strings.TrimPrefix(tt.url, "https://")
			conn, err := dial(t.Context(), "tcp", strings.TrimSuffix(host, "/"))
			if conn != nil {
				_ = conn.Close()
			}
			if !errors.Is(err, ErrRefused) || delivery.ClassOf(err) != delivery.ClassEgressRefused {
				t.Errorf("dial = %v, want %v", err, ErrRefused)
			}
		})
	}
	t.Run("then allowed", func(t *testing.T) {
		// After the refusals: the listener was reached by none of them.
		t.Parallel()
		client, dial := newClient(t, Policy{Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, RootCAs: ca.pool, Resolver: resolver})
		conn, err := dial(t.Context(), "tcp", "target.test:"+port)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		_ = conn.Close()
		got, err := get(t, client, "https://target.test:"+port+"/")
		if err != nil || got.status != http.StatusNoContent {
			t.Fatalf("GET = %d, %v", got.status, err)
		}
	})
	t.Cleanup(func() {
		if got := hits.Load(); got != 1 {
			t.Errorf("the server answered %d requests, want the one that was allowed", got)
		}
	})
}

// TestEveryAddressIsJudged: of the addresses of a name, the refused one is
// skipped and the next is connected to.
func TestEveryAddressIsJudged(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	server := serve(t, ca, nil, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	resolver := names{"target.test": {metadataService(), loopback()}}
	client, _ := newClient(t, Policy{Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, RootCAs: ca.pool, Resolver: resolver})
	got, err := get(t, client, "https://target.test:"+portOf(t, server.URL)+"/")
	if err != nil || got.status != http.StatusNoContent {
		t.Fatalf("GET = %d, %v", got.status, err)
	}
}

func allowLoopback(ca testCA) Policy {
	return Policy{
		Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, RootCAs: ca.pool,
		Resolver: names{"target.test": {loopback()}},
	}
}

// TestNoRedirect: a redirect is a failure of class redirect, and where it
// points is never asked.
func TestNoRedirect(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	var elsewhere atomic.Int64
	other := serve(t, ca, nil, func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		server := serve(t, ca, nil, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://target.test:"+portOf(t, other.URL)+"/", status)
		})
		client, _ := newClient(t, allowLoopback(ca))
		_, err := get(t, client, "https://target.test:"+portOf(t, server.URL)+"/")
		if !errors.Is(err, ErrRedirect) || delivery.ClassOf(err) != delivery.ClassRedirect {
			t.Errorf("%d: GET = %v (class %q), want %v", status, err, delivery.ClassOf(err), ErrRedirect)
		}
	}
	if got := elsewhere.Load(); got != 0 {
		t.Errorf("a redirect was followed %d times", got)
	}

	// Every 3xx is refused before its Location is read: one that cannot
	// be parsed, none, and a 307 to a request whose body cannot be sent
	// again - which http.Client would hand back as an answer.
	mark := strings.Join([]string{"TOKEN", "51ab", "IN", "LOCATION"}, "-")
	server := serve(t, ca, nil, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/malformed":
			w.Header().Set("Location", "https://target.test/"+mark+"/%zz")
			w.WriteHeader(http.StatusFound)
		case "/none":
			w.WriteHeader(http.StatusFound)
		case "/307":
			w.Header().Set("Location", "https://target.test:"+portOf(t, other.URL)+"/"+mark)
			w.WriteHeader(http.StatusTemporaryRedirect)
		case "/300":
			w.Header().Set("Location", "/"+mark)
			w.WriteHeader(http.StatusMultipleChoices)
		case "/304":
			w.WriteHeader(http.StatusNotModified)
		}
	})
	client, _ := newClient(t, allowLoopback(ca))
	for _, path := range []string{"/malformed", "/none", "/307", "/300", "/304"} {
		method, body := http.MethodGet, io.Reader(nil)
		if path == "/307" {
			// A reader of no type net/http knows: no GetBody.
			method, body = http.MethodPost, struct{ io.Reader }{strings.NewReader("an event")}
		}
		r, err := http.NewRequestWithContext(t.Context(), method, "https://target.test:"+portOf(t, server.URL)+path, body)
		if err != nil {
			t.Fatal(err)
		}
		if r.GetBody != nil {
			t.Fatal("the request can be sent again")
		}
		resp, err := client.Do(r)
		if resp != nil {
			_ = resp.Body.Close()
			t.Errorf("%s: a response, %d", path, resp.StatusCode)
		}
		if !errors.Is(err, ErrRedirect) || delivery.ClassOf(err) != delivery.ClassRedirect {
			t.Errorf("%s: %v (class %q), want %v", path, err, delivery.ClassOf(err), ErrRedirect)
		}
		if err != nil && strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), mark) {
			t.Errorf("%s: the error tells the Location: %v", path, err)
		}
	}
	if got := elsewhere.Load(); got != 0 {
		t.Errorf("a redirect was followed %d times", got)
	}
}

// TestNoProxy: the proxy the environment names is not used. Not parallel:
// it sets the environment of the process.
func TestNoProxy(t *testing.T) {
	ca := newTestCA(t)
	var proxied atomic.Int64
	proxy, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	go func() {
		for {
			conn, err := proxy.Accept()
			if err != nil {
				return
			}
			proxied.Add(1)
			_ = conn.Close()
		}
	}()
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(name, "http://"+proxy.Addr().String())
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	server := serve(t, ca, nil, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	client, _ := newClient(t, allowLoopback(ca))
	got, err := get(t, client, "https://target.test:"+portOf(t, server.URL)+"/")
	if err != nil || got.status != http.StatusNoContent {
		t.Fatalf("GET = %d, %v", got.status, err)
	}
	if got := proxied.Load(); got != 0 {
		t.Errorf("the proxy was connected to %d times", got)
	}
}

// TestResponseBodyBounded: a response of 1 MiB is read to 64 KiB and ends
// in ErrResponseTooLarge, not in io.EOF; one of exactly 64 KiB is read
// whole; Discard takes either.
func TestResponseBodyBounded(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	large, exact := bytes.Repeat([]byte("x"), 1<<20), bytes.Repeat([]byte("y"), MaxResponseBody)
	server := serve(t, ca, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/exact" {
			_, _ = w.Write(exact)
			return
		}
		_, _ = w.Write(large)
	})
	client, _ := newClient(t, allowLoopback(ca))
	url := "https://target.test:" + portOf(t, server.URL) + "/"

	got, err := get(t, client, url)
	if !errors.Is(err, ErrResponseTooLarge) || delivery.ClassOf(err) != delivery.ClassServer {
		t.Errorf("reading 1 MiB: %v (class %q), want %v", err, delivery.ClassOf(err), ErrResponseTooLarge)
	}
	if len(got.body) != MaxResponseBody || got.contentLength != -1 {
		t.Errorf("read %d bytes of a body said to be %d, want %d", len(got.body), got.contentLength, MaxResponseBody)
	}
	got, err = get(t, client, url+"exact")
	if err != nil || !bytes.Equal(got.body, exact) {
		t.Errorf("reading 64 KiB: %d bytes, %v", len(got.body), err)
	}

	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := Discard(resp); err != nil {
		t.Errorf("Discard: %v", err)
	}
	if err := Discard(nil); err != nil {
		t.Errorf("Discard(nil): %v", err)
	}
}

// TestCertificateOutsideTheCA: a certificate another CA issued fails, and
// the system's roots are not asked instead.
func TestCertificateOutsideTheCA(t *testing.T) {
	t.Parallel()
	issuer, other := newTestCA(t), newTestCA(t)
	server := serve(t, issuer, nil, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	url := "https://target.test:" + portOf(t, server.URL) + "/"

	client, _ := newClient(t, allowLoopback(other))
	_, err := get(t, client, url)
	var verification *tls.CertificateVerificationError
	if !errors.As(err, &verification) || delivery.ClassOf(err) != delivery.ClassTLS {
		t.Fatalf("GET = %v (class %q), want a failed verification", err, delivery.ClassOf(err))
	}
	system, _ := newClient(t, Policy{Allow: allowLoopback(issuer).Allow, Resolver: allowLoopback(issuer).Resolver})
	if _, err := get(t, system, url); !errors.As(err, &verification) {
		t.Errorf("GET with the system's roots = %v, want a failed verification", err)
	}
	trusted, _ := newClient(t, allowLoopback(issuer))
	if got, err := get(t, trusted, url); err != nil || got.status != http.StatusNoContent {
		t.Errorf("GET with the issuing CA = %d, %v", got.status, err)
	}
}

// tls11Server accepts connections on 127.0.0.1, reads the ClientHello of
// each and answers with a ServerHello that chooses TLS 1.1 - what a server
// that speaks nothing newer answers. It returns the port and, for each
// connection, what the client sent back: the description of its alert, or
// -1 for anything that is not an alert - a client that went on with the
// handshake - or for nothing at all.
func tls11Server(t *testing.T) (string, <-chan int) {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	answers := make(chan int, 8)
	// A ServerHello of TLS 1.1 (0x0302) choosing TLS_RSA_WITH_AES_128_CBC_SHA,
	// without extensions, in a handshake record of TLS 1.1: the type and
	// length of the message (4 bytes), the version (2), the random (32), an
	// empty session ID (1), the cipher suite (2) and no compression (1).
	const helloLength = 4 + 2 + 32 + 1 + 2 + 1
	hello := []byte{0x02, 0x00, 0x00, helloLength - 4, 0x03, 0x02}
	hello = append(hello, bytes.Repeat([]byte{0x11}, 32)...)
	hello = append(hello, 0x00, 0x00, 0x2f, 0x00)
	if len(hello) != helloLength {
		t.Fatalf("the ServerHello is %d bytes, want %d", len(hello), helloLength)
	}
	record := append([]byte{0x16, 0x03, 0x02, 0x00, helloLength}, hello...)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				header := make([]byte, 5)
				if _, err := io.ReadFull(conn, header); err != nil || header[0] != 0x16 {
					return
				}
				if _, err := io.ReadFull(conn, make([]byte, int(header[3])<<8|int(header[4]))); err != nil {
					return
				}
				if _, err := conn.Write(record); err != nil {
					return
				}
				// What comes back is an alert (21) of two bytes - its
				// level and its description - or the client going on.
				description := -1
				alert := make([]byte, 2)
				if _, err := io.ReadFull(conn, header); err == nil && header[0] == 0x15 {
					if _, err := io.ReadFull(conn, alert); err == nil {
						description = int(alert[1])
					}
				}
				answers <- description
			}()
		}
	}()
	return portOf(t, listener.Addr().String()), answers
}

// TestTLS11Fails: a target that speaks TLS 1.1 at most is not talked to.
// What is asserted is the protocol on the wire - the client answers the
// ServerHello with the alert protocol_version (70) and nothing else - and
// not the words of an error, which a release of Go may change.
func TestTLS11Fails(t *testing.T) {
	t.Parallel()
	const alertProtocolVersion = 70
	ca := newTestCA(t)
	port, answers := tls11Server(t)
	client, _ := newClient(t, allowLoopback(ca))
	if _, err := get(t, client, "https://target.test:"+port+"/"); err == nil {
		t.Fatal("GET of a target of TLS 1.1 succeeded")
	}
	select {
	case got := <-answers:
		if got != alertProtocolVersion {
			t.Errorf("the client answered TLS 1.1 with %d, want the alert protocol_version (%d)", got, alertProtocolVersion)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server saw no answer to its ServerHello")
	}
	if config := allowLoopback(ca).TLSConfig(); config.MinVersion != tls.VersionTLS12 || config.InsecureSkipVerify {
		t.Errorf("TLSConfig = %+v", config)
	}
}

// zeros is an endless body of zeros.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// TestAPeerThatStopsReadingFails: a target that accepts the connection and
// the handshake and then reads nothing of a large request fails the request
// within about the response timeout, instead of holding the worker.
func TestAPeerThatStopsReadingFails(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	config := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{ca.issue(t)}}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				server := tls.Server(conn, config)
				if err := server.HandshakeContext(context.Background()); err != nil {
					return
				}
				<-stop
			}()
		}
	}()

	policy := allowLoopback(ca)
	policy.ResponseTimeout = 300 * time.Millisecond
	client, _ := newClient(t, policy)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPut, "https://target.test:"+portOf(t, listener.Addr().String())+"/b/k",
		io.LimitReader(zeros{}, 1<<30))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	resp, err := client.Do(r)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || ctx.Err() != nil {
		t.Fatalf("PUT to a target that reads nothing = %v (context %v)", err, ctx.Err())
	}
	if got := delivery.ClassOf(err); got != delivery.ClassTimeout {
		t.Errorf("PUT to a target that reads nothing: class %q, want %q (%v)", got, delivery.ClassTimeout, err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("failed after %s, want about %s", elapsed, policy.ResponseTimeout)
	}
}

// TestATricklingBodyFails: a body that comes slower than the response
// timeout fails; one that comes steadily, for longer than the timeout in
// all, does not.
func TestATricklingBodyFails(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	server := serve(t, ca, nil, func(w http.ResponseWriter, r *http.Request) {
		pause := 100 * time.Millisecond
		if r.URL.Path == "/slow" {
			pause = 2 * time.Second
		}
		flusher, _ := w.(http.Flusher)
		for range 12 {
			if _, err := w.Write(bytes.Repeat([]byte("t"), 64)); err != nil {
				return
			}
			flusher.Flush()
			select {
			case <-time.After(pause):
			case <-r.Context().Done():
				return
			}
		}
	})
	policy := allowLoopback(ca)
	policy.ResponseTimeout = 400 * time.Millisecond
	client, _ := newClient(t, policy)
	base := "https://target.test:" + portOf(t, server.URL)

	started := time.Now()
	got, err := get(t, client, base+"/slow")
	if !isTimeout(err) || delivery.ClassOf(err) != delivery.ClassTimeout {
		t.Errorf("a trickling body: %d bytes, %v (class %q); want a timeout", len(got.body), err, delivery.ClassOf(err))
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("failed after %s, want about %s", elapsed, policy.ResponseTimeout)
	}
	if got, err := get(t, client, base+"/steady"); err != nil || len(got.body) != 12*64 {
		t.Errorf("a steady body of 1.2 s: %d bytes, %v", len(got.body), err)
	}
}

// TestCloseIdleConnections: the client closes the connections it keeps,
// through the transport it wraps.
func TestCloseIdleConnections(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	closed := make(chan struct{}, 4)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{ca.issue(t)}}
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	server.StartTLS()
	t.Cleanup(server.Close)

	client, _ := newClient(t, allowLoopback(ca))
	if got, err := get(t, client, "https://target.test:"+portOf(t, server.URL)+"/"); err != nil || got.status != http.StatusNoContent {
		t.Fatalf("GET = %d, %v", got.status, err)
	}
	select {
	case <-closed:
		t.Fatal("the connection was closed before it was asked to be")
	case <-time.After(100 * time.Millisecond):
	}
	client.CloseIdleConnections()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the idle connection is still open")
	}
}

// TestErrorsNameNoPath: whatever fails, the error names the origin of the
// request and nothing of its path or query, which may hold a token.
func TestErrorsNameNoPath(t *testing.T) {
	t.Parallel()
	mark := strings.Join([]string{"TOKEN", "9c1f", "IN", "PATH"}, "-")
	ca, other := newTestCA(t), newTestCA(t)
	release := make(chan struct{})
	server := serve(t, ca, nil, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/redirect/"):
			http.Redirect(w, r, "https://target.test/elsewhere/"+mark+"?token="+mark, http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/malformed/"):
			w.Header().Set("Location", "https://target.test/"+mark+"/%zz")
			w.WriteHeader(http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/slow/"):
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
	})
	// After serve: cleanups run last first, and the server waits for its
	// handlers when it closes.
	t.Cleanup(func() { close(release) })
	port := portOf(t, server.URL)
	slow := allowLoopback(ca)
	slow.ResponseTimeout = 200 * time.Millisecond
	tests := []struct {
		name   string
		policy Policy
		url    string
	}{
		{"refused", Policy{RootCAs: ca.pool, Resolver: allowLoopback(ca).Resolver}, "https://target.test:" + port + "/hook/" + mark + "?token=" + mark},
		{"redirected", allowLoopback(ca), "https://target.test:" + port + "/redirect/" + mark + "?token=" + mark},
		{"redirected, malformed", allowLoopback(ca), "https://target.test:" + port + "/malformed/" + mark + "?token=" + mark},
		{"another CA", allowLoopback(other), "https://target.test:" + port + "/hook/" + mark + "?token=" + mark},
		{"in the clear", allowLoopback(ca), "http://target.test:" + port + "/hook/" + mark + "?token=" + mark},
		{"too slow", slow, "https://target.test:" + port + "/slow/" + mark + "?token=" + mark},
		{"unknown name", allowLoopback(ca), "https://nowhere.test:" + port + "/hook/" + mark + "?token=" + mark},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client, _ := newClient(t, tt.policy)
			_, err := get(t, client, tt.url)
			if err == nil {
				t.Fatal("no error")
			}
			if text := fmt.Sprintf("%v %+v %#v", err, err, err); strings.Contains(text, mark) {
				t.Errorf("the error tells the path: %s", text)
			}
			if !strings.Contains(err.Error(), ".test:"+port) {
				t.Errorf("the error does not name the origin: %v", err)
			}
		})
	}
}

// noReplay is a request body net/http cannot send again: a POST of it has
// no GetBody, and fails rather than being retried on another connection.
func noReplay(text string) io.Reader {
	return struct{ io.Reader }{strings.NewReader(text)}
}

// TestPooledConnectionsAreReused: a connection kept for the next request is
// never handed out as the progress deadline of its parked read fires. A
// POST that cannot be sent again, after a pause of about half the response
// timeout, the whole of it, and a little more, succeeds every time - and
// the shorter pauses do reuse the connection.
func TestPooledConnectionsAreReused(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	server := serve(t, ca, nil, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	})
	const idle = 100 * time.Millisecond
	policy := allowLoopback(ca)
	policy.ResponseTimeout = idle
	client, _ := newClient(t, policy)
	url := "https://target.test:" + portOf(t, server.URL) + "/hook"
	post := func(reused *atomic.Int64) error {
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
			if info.Reused {
				reused.Add(1)
			}
		}}
		r, err := http.NewRequestWithContext(httptrace.WithClientTrace(t.Context(), trace), http.MethodPost, url, noReplay("an event"))
		if err != nil {
			t.Fatal(err)
		}
		if r.GetBody != nil {
			t.Fatal("the request can be sent again")
		}
		resp, err := client.Do(r)
		if err != nil {
			return err
		}
		return Discard(resp)
	}
	var reused atomic.Int64
	failures := 0
	for round := range 8 {
		for _, pause := range []time.Duration{idle/2 - 10*time.Millisecond, idle / 2, idle/2 + 5*time.Millisecond, idle - 5*time.Millisecond, idle, idle + 5*time.Millisecond} {
			if err := post(&reused); err != nil {
				t.Fatalf("round %d: the first POST: %v", round, err)
			}
			time.Sleep(pause)
			if err := post(&reused); err != nil {
				failures++
				t.Errorf("round %d: a POST %s after the last: %v", round, pause, err)
			}
		}
	}
	if failures == 0 && reused.Load() == 0 {
		t.Error("no connection was ever reused")
	}
}

// TestFailuresHaveAClass: what fails on a connection that was made is a
// timeout or a connection that failed - never an error of no class.
func TestFailuresHaveAClass(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)

	// A target that completes the handshake and hangs up.
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	config := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{ca.issue(t)}}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				server := tls.Server(conn, config)
				_ = server.HandshakeContext(context.Background())
				_ = conn.Close()
			}()
		}
	}()
	client, _ := newClient(t, allowLoopback(ca))
	_, err = get(t, client, "https://target.test:"+portOf(t, listener.Addr().String())+"/")
	if err == nil || delivery.ClassOf(err) != delivery.ClassConnect {
		t.Errorf("a target that hangs up: %v (class %q), want %q", err, delivery.ClassOf(err), delivery.ClassConnect)
	}

	// A request body handed over more slowly than the response timeout is
	// no progress.
	server := serve(t, ca, nil, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	})
	policy := allowLoopback(ca)
	policy.ResponseTimeout = 200 * time.Millisecond
	slowClient, _ := newClient(t, policy)
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "https://target.test:"+portOf(t, server.URL)+"/b/k",
		io.MultiReader(strings.NewReader("a start"), slowReader{pause: 2 * time.Second}))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	resp, err := slowClient.Do(r)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || delivery.ClassOf(err) != delivery.ClassTimeout {
		t.Errorf("a slow request body: %v (class %q), want %q", err, delivery.ClassOf(err), delivery.ClassTimeout)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("failed after %s", elapsed)
	}
}

// slowReader gives one byte after a pause, then ends.
type slowReader struct {
	pause time.Duration
}

func (s slowReader) Read(p []byte) (int, error) {
	time.Sleep(s.pause)
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = 'x'
	return 1, io.EOF
}

// TestDiscardKeepsTheConnection: a body of exactly 64 KiB, of a known
// length or chunked with its end coming after the data, is read to its end
// by Discard, and its connection is used again; one byte more is no error
// of Discard.
func TestDiscardKeepsTheConnection(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	server := serve(t, ca, nil, func(w http.ResponseWriter, r *http.Request) {
		size := MaxResponseBody
		if r.URL.Path == "/over" {
			size++
		}
		if r.URL.Path != "/chunked" {
			_, _ = w.Write(bytes.Repeat([]byte("z"), size))
			return
		}
		// Headers sent before the body is whole: no length, chunks. The
		// 64 KiB go out at once and the end of the body a moment later,
		// so that it is not in the buffer the last of them came in: it is
		// read only by a read past the 64 KiB.
		flusher, _ := w.(http.Flusher)
		flusher.Flush()
		_, _ = w.Write(bytes.Repeat([]byte("z"), size))
		flusher.Flush()
		time.Sleep(50 * time.Millisecond)
	})
	client, _ := newClient(t, allowLoopback(ca))
	base := "https://target.test:" + portOf(t, server.URL)
	var reused atomic.Int64
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		if info.Reused {
			reused.Add(1)
		}
	}}
	for i, path := range []string{"/exact", "/chunked", "/exact", "/chunked"} {
		r, err := http.NewRequestWithContext(httptrace.WithClientTrace(t.Context(), trace), http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		if path == "/chunked" && resp.ContentLength != -1 {
			t.Fatalf("%s: a body of the length %d, want chunks", path, resp.ContentLength)
		}
		if err := Discard(resp); err != nil {
			t.Errorf("request %d (%s): Discard: %v", i, path, err)
		}
	}
	if got := reused.Load(); got != 3 {
		t.Errorf("the connection was used again %d times, want 3", got)
	}
	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/over", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := Discard(resp); err != nil {
		t.Errorf("Discard of 64 KiB and a byte: %v", err)
	}
}

// endReader is a body that records how far it was read: whether its end
// was reached, and how many bytes were taken.
type endReader struct {
	r     io.Reader
	taken int
	ended bool
}

func (e *endReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	e.taken += n
	if errors.Is(err, io.EOF) {
		e.ended = true
	}
	return n, err
}

func (*endReader) Close() error { return nil }

// TestDiscardReadsToTheEnd: Discard reads a body of this client to its end
// - not to 64 KiB and no further, which leaves the end of a body of exactly
// 64 KiB unread - and of a longer one, to the bound and a byte past it,
// which is how it knows, and is no error.
func TestDiscardReadsToTheEnd(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		size      int
		wantEnded bool
		wantTaken int
	}{
		{size: 10, wantEnded: true, wantTaken: 10},
		{size: MaxResponseBody, wantEnded: true, wantTaken: MaxResponseBody},
		{size: MaxResponseBody + 1, wantEnded: false, wantTaken: MaxResponseBody + 1},
		{size: 1 << 20, wantEnded: false, wantTaken: MaxResponseBody + 1},
	} {
		body := &endReader{r: bytes.NewReader(make([]byte, tt.size))}
		resp := &http.Response{Body: &limitedBody{body: body, left: MaxResponseBody}}
		if err := Discard(resp); err != nil {
			t.Errorf("%d bytes: Discard: %v", tt.size, err)
		}
		if body.ended != tt.wantEnded || body.taken != tt.wantTaken {
			t.Errorf("%d bytes: read %d, to the end %v; want %d, %v", tt.size, body.taken, body.ended, tt.wantTaken, tt.wantEnded)
		}
	}
	// A body of another client is read to the bound.
	other := &endReader{r: bytes.NewReader(make([]byte, 1<<20))}
	if err := Discard(&http.Response{Body: other}); err != nil || other.taken != MaxResponseBody {
		t.Errorf("another body: read %d, %v", other.taken, err)
	}
}

// TestNothingInTheClear: http:// is not sent.
func TestNothingInTheClear(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	client, _ := newClient(t, allowLoopback(newTestCA(t)))
	if _, err := get(t, client, server.URL); !errors.Is(err, ErrNotTLS) {
		t.Errorf("GET http:// = %v, want %v", err, ErrNotTLS)
	}
	if hits.Load() != 0 {
		t.Error("a request was sent in the clear")
	}
}

// TestResponseTimeout: a target that does not answer within the delivery
// timeout fails.
func TestResponseTimeout(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	release := make(chan struct{})
	server := serve(t, ca, nil, func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	t.Cleanup(func() { close(release) })
	policy := allowLoopback(ca)
	policy.ResponseTimeout = 200 * time.Millisecond
	client, _ := newClient(t, policy)
	_, err := get(t, client, "https://target.test:"+portOf(t, server.URL)+"/")
	if !isTimeout(err) || delivery.ClassOf(err) != delivery.ClassTimeout {
		t.Errorf("GET = %v (class %q), want a timeout", err, delivery.ClassOf(err))
	}
}

func TestDialRefuses(t *testing.T) {
	t.Parallel()
	_, dial := newClient(t, Policy{Resolver: names{"empty.test": {}}})
	tests := map[string]struct {
		network, address string
		class            delivery.Class
	}{
		"udp":                {network: "udp", address: "192.0.2.1:53", class: delivery.ClassInternal},
		"no port":            {network: "tcp", address: "192.0.2.1", class: delivery.ClassConnect},
		"port 0":             {network: "tcp", address: "192.0.2.1:0", class: delivery.ClassConnect},
		"a name unknown":     {network: "tcp", address: "nowhere.test:443", class: delivery.ClassConnect},
		"a name of nothing":  {network: "tcp", address: "empty.test:443", class: delivery.ClassConnect},
		"unspecified, tcp4":  {network: "tcp4", address: "0.0.0.0:443", class: delivery.ClassEgressRefused},
		"multicast, tcp6":    {network: "tcp6", address: "[ff02::1]:443", class: delivery.ClassEgressRefused},
		"metadata over IPv6": {network: "tcp", address: "[fd00:ec2::254]:443", class: delivery.ClassEgressRefused},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			conn, err := dial(t.Context(), tt.network, tt.address)
			if conn != nil {
				_ = conn.Close()
			}
			if err == nil || delivery.ClassOf(err) != tt.class {
				t.Errorf("dial = %v (class %q), want class %q", err, delivery.ClassOf(err), tt.class)
			}
		})
	}
}

func TestNewClientRefuses(t *testing.T) {
	t.Parallel()
	for name, policy := range map[string]Policy{
		"a negative timeout":            {ResponseTimeout: -time.Second},
		"a timeout of 1 ns":             {ResponseTimeout: time.Nanosecond},
		"a timeout under a millisecond": {ResponseTimeout: MinResponseTimeout - time.Microsecond},
		"a range of nothing":            {Allow: []netip.Prefix{{}}},
	} {
		if _, _, err := NewClient(policy); !errors.Is(err, errPolicy) {
			t.Errorf("%s: %v, want %v", name, err, errPolicy)
		}
	}
	client, dial, err := NewClient(Policy{Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/8")}})
	if err != nil || client == nil || dial == nil {
		t.Fatalf("NewClient = %v", err)
	}
	if _, _, err := NewClient(Policy{ResponseTimeout: MinResponseTimeout}); err != nil {
		t.Errorf("a timeout of %s: %v", MinResponseTimeout, err)
	}
}

// do sends r with client and returns what it got, as get does.
func do(t *testing.T, client *Client, r *http.Request) (answer, error) {
	t.Helper()
	resp, err := client.Do(r)
	if err != nil {
		return answer{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return answer{status: resp.StatusCode, contentLength: resp.ContentLength, body: body}, err
}

// TestServerNameIsTheURLHost: the certificate is verified for the host of
// the URL - not for the address it resolved to, and not for a Host header
// the request carries.
func TestServerNameIsTheURLHost(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	// The certificate names target.test and 127.0.0.1, and nothing else.
	server := serve(t, ca, nil, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	port := portOf(t, server.URL)
	policy := allowLoopback(ca)
	policy.Resolver = names{"target.test": {loopback()}, "wrong.test": {loopback()}}
	client, _ := newClient(t, policy)
	request := func(url, host string) *http.Request {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if host != "" {
			r.Host = host
		}
		return r
	}
	tests := []struct {
		name, url, host string
		wantClass       delivery.Class
	}{
		{"the name of the certificate", "https://target.test:" + port + "/", "", delivery.ClassNone},
		{"another name, at the same address", "https://wrong.test:" + port + "/", "", delivery.ClassTLS},
		{"another name, the Host header the name of the certificate", "https://wrong.test:" + port + "/", "target.test", delivery.ClassTLS},
		{"the address of the certificate", "https://127.0.0.1:" + port + "/", "", delivery.ClassNone},
	}
	for _, tt := range tests {
		got, err := do(t, client, request(tt.url, tt.host))
		if class := delivery.ClassOf(err); class != tt.wantClass {
			t.Errorf("%s: %v (class %q), want class %q", tt.name, err, class, tt.wantClass)
			continue
		}
		var hostname x509.HostnameError
		if tt.wantClass == delivery.ClassTLS && !errors.As(err, &hostname) {
			t.Errorf("%s: %v, want a certificate for another host", tt.name, err)
		}
		if tt.wantClass == delivery.ClassNone && got.status != http.StatusNoContent {
			t.Errorf("%s: %d", tt.name, got.status)
		}
	}
}

// TestTransportDialsNothingInTheClear: the transport inside the client,
// handed a request that is not https, dials nothing - not with a dialer of
// its own, outside the policy.
func TestTransportDialsNothingInTheClear(t *testing.T) {
	t.Parallel()
	var accepted atomic.Int64
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
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
			accepted.Add(1)
			_ = conn.Close()
		}
	}()
	client, _ := newClient(t, Policy{Allow: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}})
	outer, ok := client.http.Transport.(bounded)
	if !ok {
		t.Fatalf("the transport is a %T", client.http.Transport)
	}
	inner, ok := outer.next.(*http.Transport)
	if !ok {
		t.Fatalf("the inner transport is a %T", outer.next)
	}
	for _, url := range []string{"http://169.254.169.254/latest/meta-data/", "http://" + listener.Addr().String() + "/"} {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		resp, err := inner.RoundTrip(r)
		cancel()
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, ErrNotTLS) {
			t.Errorf("%s through the transport: %v, want %v", url, err, ErrNotTLS)
		}
	}
	// Anything dialled would have been accepted by now.
	time.Sleep(50 * time.Millisecond)
	if got := accepted.Load(); got != 0 {
		t.Errorf("the listener was connected to %d times", got)
	}
}

// blockingResolver answers no name until the context of the lookup ends,
// and says when it was asked.
type blockingResolver struct {
	asked chan struct{}
}

func (b blockingResolver) LookupNetIP(ctx context.Context, _, _ string) ([]netip.Addr, error) {
	b.asked <- struct{}{}
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestCancellationIsOneClass: a request cancelled by its sender while the
// name is resolved, during the handshake or while the response is awaited
// is ClassInternal - no target failed - and says it was cancelled.
func TestCancellationIsOneClass(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)

	// The handshake: a listener that accepts and says nothing.
	silent, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{}, 4)
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	t.Cleanup(func() {
		_ = silent.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})
	go func() {
		for {
			conn, err := silent.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			accepted <- struct{}{}
		}
	}()
	// The response: a handler that answers when the request is gone.
	entered := make(chan struct{}, 4)
	server := serve(t, ca, nil, func(_ http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-r.Context().Done()
	})

	asked := make(chan struct{}, 4)
	tests := []struct {
		name   string
		policy Policy
		url    string
		begun  <-chan struct{}
	}{
		{"while the name is resolved", Policy{Resolver: blockingResolver{asked: asked}}, "https://target.test/", asked},
		{"during the handshake", allowLoopback(ca), "https://target.test:" + portOf(t, silent.Addr().String()) + "/", accepted},
		{"while the response is awaited", allowLoopback(ca), "https://target.test:" + portOf(t, server.URL) + "/", entered},
	}
	for _, tt := range tests {
		client, _ := newClient(t, tt.policy)
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			select {
			case <-tt.begun:
			case <-time.After(10 * time.Second):
			}
			cancel()
		}()
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, tt.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = do(t, client, r)
		cancel()
		if delivery.ClassOf(err) != delivery.ClassInternal || !errors.Is(err, context.Canceled) {
			t.Errorf("%s: %v (class %q), want a cancellation of class %q", tt.name, err, delivery.ClassOf(err), delivery.ClassInternal)
		}
	}
}

// TestResponseTLSState: a response tells the TLS state of its connection,
// a new one and one used again.
func TestResponseTLSState(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	server := serve(t, ca, nil, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	client, _ := newClient(t, allowLoopback(ca))
	for i := range 2 {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://target.test:"+portOf(t, server.URL)+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		state := resp.TLS
		if err := Discard(resp); err != nil {
			t.Fatal(err)
		}
		if state == nil {
			t.Fatalf("request %d: no TLS state", i)
		}
		if !state.HandshakeComplete || state.Version < tls.VersionTLS12 || state.ServerName != "target.test" ||
			len(state.PeerCertificates) == 0 || state.PeerCertificates[0].Subject.CommonName != "target.test" {
			t.Errorf("request %d: TLS state %+v", i, state)
		}
	}
}
