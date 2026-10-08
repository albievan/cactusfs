package nodetls_test

import (
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/nodetls"
	"github.com/albievan/cactusfs/internal/domain"
)

func authority(t *testing.T) *nodetls.Authority {
	t.Helper()
	a, err := nodetls.NewAuthority(rand.Reader, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func identity(t *testing.T, a *nodetls.Authority, nodeID string) *nodetls.Identity {
	t.Helper()
	certPEM, keyPEM, err := a.Issue(nodeID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	id, err := nodetls.New(certPEM, keyPEM, a.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// echo accepts connections on a node's listener and writes back, to each,
// the name of the node that called.
func echo(t *testing.T, id *nodetls.Identity) string {
	t.Helper()
	listener, err := id.Listen(t.Context(), "127.0.0.1:0", func(nodeID string) bool { return nodeID != "banned-1" })
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
				caller, err := nodetls.PeerNodeID(secured.ConnectionState())
				if err != nil {
					return
				}
				_, _ = io.WriteString(conn, caller)
			}()
		}
	}()
	return listener.Addr().String()
}

// TestNodesProveWhoTheyAre: a connection between nodes is TLS 1.3, both ends
// present a certificate of the cluster's CA, and each learns the other's
// name from it - never from the address.
func TestNodesProveWhoTheyAre(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ca := authority(t)
	server, client := identity(t, ca, "meta-1"), identity(t, ca, "gateway-7")
	address := echo(t, server)

	conn, err := client.Dial(ctx, nodetls.Address("meta-1", address))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	seen, err := io.ReadAll(conn)
	if err != nil || string(seen) != "gateway-7" {
		t.Errorf("the server saw the caller as %q, %v; want gateway-7", seen, err)
	}
	secured, ok := conn.(*tls.Conn)
	if !ok {
		t.Fatal("Dial did not return a TLS connection")
	}
	state := secured.ConnectionState()
	if state.Version != tls.VersionTLS13 {
		t.Errorf("TLS version = %x, want 1.3", state.Version)
	}
	if peer, err := nodetls.PeerNodeID(state); err != nil || peer != "meta-1" {
		t.Errorf("the client saw the server as %q, %v", peer, err)
	}
	_ = conn.Close()

	// A node the listener does not admit is cut off after the handshake,
	// whatever its certificate: the Raft port serves voters only.
	banned := identity(t, ca, "banned-1")
	if conn, err := banned.Dial(ctx, nodetls.Address("meta-1", address)); err == nil {
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if n, err := conn.Read(make([]byte, 1)); err == nil || n > 0 {
			t.Error("a node the listener does not admit was served")
		}
		_ = conn.Close()
	}
	// A connection that never speaks holds nothing up: others are served.
	silent, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = silent.Close() }()
	if conn, err := client.Dial(ctx, nodetls.Address("meta-1", address)); err != nil {
		t.Errorf("Dial while another connection is silent: %v", err)
	} else {
		_ = conn.Close()
	}

	// The address answers, but not as the node that was meant.
	if _, err := client.Dial(ctx, nodetls.Address("meta-2", address)); err == nil {
		t.Error("a connection to meta-2 was accepted from a node that is meta-1")
	}
	// A node of another cluster is refused by the server...
	stranger := identity(t, authority(t), "gateway-7")
	if conn, err := stranger.Dial(ctx, nodetls.Address("meta-1", address)); err == nil {
		// TLS 1.3 reports a rejected client certificate on first use.
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if n, err := conn.Read(make([]byte, 1)); err == nil || n > 0 {
			t.Error("a node with a certificate of another CA was served")
		}
		_ = conn.Close()
	}
	// ...and a server of another cluster is refused by the client.
	if _, err := client.Dial(ctx, nodetls.Address("gateway-7", echo(t, stranger))); err == nil {
		t.Error("a server with a certificate of another CA was trusted")
	}
	// No certificate at all, or an old protocol version.
	for name, cfg := range map[string]*tls.Config{
		"no client certificate": {RootCAs: client.Client("meta-1").RootCAs, ServerName: "meta-1", MinVersion: tls.VersionTLS13},
	} {
		raw, err := (&tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: cfg}).DialContext(ctx, "tcp", address)
		if err == nil {
			_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
			if n, err := raw.Read(make([]byte, 1)); err == nil || n > 0 {
				t.Errorf("%s: served", name)
			}
			_ = raw.Close()
		}
	}
	if server.Server().MinVersion != tls.VersionTLS13 || client.Client("x").MinVersion != tls.VersionTLS13 ||
		server.Server().ClientAuth != tls.RequireAndVerifyClientCert {
		t.Error("node TLS is not mutual TLS 1.3")
	}

	for _, bad := range []string{"", "meta-1", "@host:1", "meta-1@", "Bad Name@host:1"} {
		if _, err := client.Dial(ctx, bad); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("Dial(%q) = %v, want %v", bad, err, domain.ErrInvalidArgument)
		}
	}
	if id, hostPort, err := nodetls.SplitAddress("n1@host:9003"); err != nil || id != "n1" || hostPort != "host:9003" {
		t.Errorf("SplitAddress = %q, %q, %v", id, hostPort, err)
	}
	if _, err := nodetls.PeerNodeID(tls.ConnectionState{}); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("PeerNodeID of an unverified connection = %v", err)
	}
}

// TestIdentityIsCheckedWhenItIsLoaded: a certificate that the CA did not
// issue, that names no node, or that does not go with the key is refused at
// start, not at the first connection.
func TestIdentityIsCheckedWhenItIsLoaded(t *testing.T) {
	t.Parallel()
	ca, other := authority(t), authority(t)
	certPEM, keyPEM, err := ca.Issue("node-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	otherCert, otherKey, err := other.Issue("node-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cert, key, caFile := write("node.pem", certPEM), write("node.key", keyPEM), write("ca.pem", ca.CertPEM)
	loaded, err := nodetls.Load(cert, key, caFile)
	if err != nil || loaded.NodeID() != "node-1" {
		t.Fatalf("Load: %v", err)
	}

	refused := map[string][3]string{
		"certificate of another CA":   {write("o.pem", otherCert), write("o.key", otherKey), caFile},
		"key of another certificate":  {cert, write("o2.key", otherKey), caFile},
		"CA file without certificate": {cert, key, write("empty.pem", []byte("not a certificate"))},
		"missing certificate":         {filepath.Join(dir, "absent.pem"), key, caFile},
		"missing key":                 {cert, filepath.Join(dir, "absent.key"), caFile},
		"missing CA":                  {cert, key, filepath.Join(dir, "absent-ca.pem")},
		"oversized file":              {write("big.pem", []byte(strings.Repeat("x", 2<<20))), key, caFile},
		// The CA's own certificate is not a node's.
		"the CA certificate as node certificate": {caFile, key, caFile},
	}
	for name, files := range refused {
		if _, err := nodetls.Load(files[0], files[1], files[2]); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}

	if _, _, err := ca.Issue("Not A Node", time.Now()); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Issue for an invalid node ID = %v", err)
	}
	if _, _, err := ca.Issue("node-2", time.Now().Add(2*time.Hour)); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Issue from an expired CA = %v", err)
	}
	// Two certificates never share a serial number or a key.
	again, againKey, _ := ca.Issue("node-1", time.Now())
	if string(again) == string(certPEM) || string(againKey) == string(keyPEM) {
		t.Error("two issued certificates are identical")
	}
}

// TestClosingTheListenerClosesItsConnections: a node that stops must not
// leave its peers writing into connections that nobody reads any more. They
// would wait for answers that never come.
func TestClosingTheListenerClosesItsConnections(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ca := authority(t)
	server, client := identity(t, ca, "meta-1"), identity(t, ca, "meta-2")
	listener, err := server.Listen(ctx, "127.0.0.1:0", func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	conn, err := client.Dial(ctx, nodetls.Address("meta-1", listener.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	held := <-accepted // the server keeps it open and reads nothing

	if err := listener.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("after the listener closed, a peer's read = %v; want the connection closed", err)
	}
	if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Accept after Close = %v", err)
	}
	// Closing a connection twice, from either side, is harmless.
	_ = held.Close()
}
