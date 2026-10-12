// Package nodetls is the transport security of a cluster: mutual TLS 1.3
// between nodes, for node RPC and for Raft alike.
//
// A node's identity is its certificate. The certificate is issued by the
// cluster's node CA - a CA that signs node certificates and nothing else -
// and names the node: its ID is the certificate's first DNS name. A node
// that dials another checks that the certificate it is shown names the node
// it meant to reach, wherever it reached it; a node that is dialled learns
// the caller's ID from the caller's certificate. Addresses are never trusted
// to say who is behind them.
package nodetls

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
)

// Identity is a node's certificate and the CA that it and its peers trust.
// The certificate can be replaced while the node runs (see Reloader); the
// node it names and the CA cannot.
type Identity struct {
	nodeID string
	roots  *x509.CertPool
	// current is the certificate every new handshake presents. A handshake
	// that has begun, and a connection that is open, keep the one they have.
	current atomic.Pointer[tls.Certificate]
	// digest tells the files the certificate was made from from any other.
	digest [sha256.Size]byte
	// refused counts the peers whose handshake was refused, by reason.
	refused map[PeerRefusal]*atomic.Uint64
}

// PeerRefusal is why a handshake with another node was refused. The set is
// fixed, so that it can label a metric.
type PeerRefusal string

const (
	// PeerRefusedHandshake - the handshake failed before any certificate
	// was judged, for a reason that is none of the others: a connection
	// that was closed half-way, a caller that speaks no TLS 1.3.
	PeerRefusedHandshake PeerRefusal = "handshake"
	// PeerRefusedTimeout - the caller connected and did not finish the
	// handshake in time.
	PeerRefusedTimeout PeerRefusal = "timeout"
	// PeerRefusedNoCertificate - the caller showed no certificate.
	PeerRefusedNoCertificate PeerRefusal = "no_certificate"
	// PeerRefusedUntrusted - the certificate is not a valid one of the node
	// CA.
	PeerRefusedUntrusted PeerRefusal = "untrusted"
	// PeerRefusedIdentity - the certificate names no node, or not the node
	// that was dialled.
	PeerRefusedIdentity PeerRefusal = "identity"
	// PeerRefusedNotAdmitted - the certificate is good, and the listener
	// does not admit the node it names.
	PeerRefusedNotAdmitted PeerRefusal = "not_admitted"
)

// PeerRefusals returns every PeerRefusal there is.
func PeerRefusals() []PeerRefusal {
	return []PeerRefusal{
		PeerRefusedHandshake, PeerRefusedTimeout, PeerRefusedNoCertificate, PeerRefusedUntrusted, PeerRefusedIdentity,
		PeerRefusedNotAdmitted,
	}
}

// PeersRefused returns how many handshakes with other nodes were refused for
// the given reason, on connections whose handshake this package does: those
// of Listen and of Dial, which carry Raft. gRPC does its own for node RPC,
// with the same configuration; they are not counted here.
func (i *Identity) PeersRefused(reason PeerRefusal) uint64 {
	if counter, ok := i.refused[reason]; ok {
		return counter.Load()
	}
	return 0
}

func (i *Identity) refuse(reason PeerRefusal) {
	i.refused[reason].Add(1)
}

// noCertificate is how crypto/tls says that a caller showed no certificate.
// It has no type for it; should the text change, such a caller is counted as
// PeerRefusedHandshake, and still counted.
const noCertificate = "client didn't provide a certificate"

// peerRefusalOf sorts the error of a failed handshake.
func peerRefusalOf(err error) PeerRefusal {
	var verification *tls.CertificateVerificationError
	if !errors.As(err, &verification) {
		var timeout net.Error
		switch {
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded),
			errors.As(err, &timeout) && timeout.Timeout():
			return PeerRefusedTimeout
		case strings.Contains(err.Error(), noCertificate):
			return PeerRefusedNoCertificate
		}
		return PeerRefusedHandshake
	}
	var name x509.HostnameError
	if errors.As(err, &name) {
		return PeerRefusedIdentity
	}
	return PeerRefusedUntrusted
}

// NotAfter returns when the certificate in use expires.
func (i *Identity) NotAfter() time.Time {
	return i.current.Load().Leaf.NotAfter
}

// Load reads a node's certificate, key and CA from PEM files. It checks, now
// rather than at the first connection, that the certificate is one the CA
// issued and that it names a node. The certificate and the key are read as a
// Reloader reads them again later: regular files, within their directory.
func Load(certFile, keyFile, caFile string) (*Identity, error) {
	var pems [3][]byte
	readers := []func(string) ([]byte, error){readConfined, readConfined, readFile}
	for i, path := range []string{certFile, keyFile, caFile} {
		data, err := readers[i](path)
		if err != nil {
			return nil, fmt.Errorf("nodetls.Load: %w", err)
		}
		pems[i] = data
	}
	certPEM, keyPEM, caPEM := pems[0], pems[1], pems[2]
	identity, err := New(certPEM, keyPEM, caPEM)
	if err != nil {
		return nil, fmt.Errorf("nodetls.Load: %w", err)
	}
	return identity, nil
}

// readFile reads the node CA, once, at start: within its directory, and
// whatever kind of file it is.
func readFile(path string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxPEMFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPEMFile {
		return nil, fmt.Errorf("%w: %s is too large to be a PEM file", domain.ErrInvalidArgument, path)
	}
	return data, nil
}

// New builds an Identity from PEM data.
func New(certPEM, keyPEM, caPEM []byte) (*Identity, error) {
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("%w: node certificate and key: %w", domain.ErrInvalidArgument, err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("%w: the node CA file holds no certificate", domain.ErrInvalidArgument)
	}
	leaf, err := leafOf(&certificate)
	if err != nil {
		return nil, fmt.Errorf("%w: node certificate: %w", domain.ErrInvalidArgument, err)
	}
	nodeID, err := nodeIDOf(leaf)
	if err != nil {
		return nil, err
	}
	// The zero time is the time it is.
	if err := verifyNode(leaf, roots, nodeID, time.Time{}); err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrInvalidArgument, err)
	}
	identity := &Identity{
		nodeID: nodeID, roots: roots, digest: pairDigest(certPEM, keyPEM), refused: map[PeerRefusal]*atomic.Uint64{},
	}
	for _, reason := range PeerRefusals() {
		identity.refused[reason] = &atomic.Uint64{}
	}
	identity.current.Store(&certificate)
	return identity, nil
}

// leafOf returns the parsed certificate of a pair, and keeps it with the
// pair.
func leafOf(certificate *tls.Certificate) (*x509.Certificate, error) {
	if certificate.Leaf != nil {
		return certificate.Leaf, nil
	}
	if len(certificate.Certificate) == 0 {
		return nil, errors.New("no certificate")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, err
	}
	certificate.Leaf = leaf
	return leaf, nil
}

// verifyNode checks, as a peer will at every handshake, that a certificate
// is one the node CA issued to the node with the given ID, good for both
// ends of a connection, and valid at the given time.
func verifyNode(leaf *x509.Certificate, roots *x509.CertPool, nodeID string, now time.Time) error {
	usages := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	for _, usage := range usages {
		options := x509.VerifyOptions{Roots: roots, DNSName: nodeID, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{usage}}
		if _, err := leaf.Verify(options); err != nil {
			return fmt.Errorf("the node certificate is not valid under the node CA, as server and as client: %w", err)
		}
	}
	return nil
}

// nodeIDOf returns the node a certificate names.
func nodeIDOf(leaf *x509.Certificate) (string, error) {
	// Exactly one name: a certificate with two could call as one node and
	// answer as another.
	if leaf == nil || len(leaf.DNSNames) != 1 || len(leaf.IPAddresses) != 0 {
		return "", fmt.Errorf("%w: a node certificate carries exactly one DNS name, the node's ID, and no IP address",
			domain.ErrAccessDenied)
	}
	if err := cluster.ValidateNodeID(leaf.DNSNames[0]); err != nil {
		return "", fmt.Errorf("%w: the certificate's first DNS name is not a node ID: %w", domain.ErrAccessDenied, err)
	}
	return leaf.DNSNames[0], nil
}

// NodeID returns the node the certificate names.
func (i *Identity) NodeID() string {
	return i.nodeID
}

// Server returns the configuration for accepting connections from other
// nodes: TLS 1.3, and a certificate of the node CA is demanded of every
// caller. Every handshake presents the certificate in use when it begins.
func (i *Identity) Server() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Never nil: there is a certificate from the moment the identity
		// exists, and it is only ever exchanged for another.
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return i.current.Load(), nil },
		ClientAuth:     tls.RequireAndVerifyClientCert,
		ClientCAs:      i.roots,
	}
}

// Client returns the configuration for connecting to the node with the given
// ID: TLS 1.3, and the certificate shown must be one of the node CA that
// names that node. Every handshake presents the certificate in use when it
// begins - a connection that is opened again after a replacement, too.
func (i *Identity) Client(nodeID string) *tls.Config {
	return &tls.Config{
		MinVersion:           tls.VersionTLS13,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return i.current.Load(), nil },
		RootCAs:              i.roots,
		ServerName:           nodeID,
	}
}

// PeerNodeID returns the node at the other end of a connection, from the
// certificate it was verified with.
func PeerNodeID(state tls.ConnectionState) (string, error) {
	if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
		return "", fmt.Errorf("%w: the connection has no verified node certificate", domain.ErrAccessDenied)
	}
	return nodeIDOf(state.VerifiedChains[0][0])
}

// handshakeTimeout bounds the TLS handshake of an accepted connection, so
// that a peer that connects and says nothing holds nothing for long.
const handshakeTimeout = 10 * time.Second

// Listen accepts connections from other nodes on address. A connection is
// handed out only after its handshake has completed and allow has accepted
// the node the caller's certificate names; any other is closed. allow lets
// the owner of the listener admit fewer nodes than the CA has certified -
// the Raft listener admits only the nodes that vote.
func (i *Identity) Listen(ctx context.Context, address string, allow func(nodeID string) bool) (net.Listener, error) {
	inner, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("nodetls.Listen: %w", err)
	}
	l := &listener{
		Listener: inner, config: i.Server(), allow: allow, accepted: make(chan net.Conn), closed: make(chan struct{}),
		refuse: i.refuse, open: map[*tracked]struct{}{},
	}
	// The listener outlives the call that opened it.
	go l.run(context.WithoutCancel(ctx))
	return l, nil
}

// listener hands out connections that are authenticated and admitted.
type listener struct {
	net.Listener
	config *tls.Config
	allow  func(nodeID string) bool
	// refuse is told why a caller was turned away.
	refuse   func(PeerRefusal)
	accepted chan net.Conn
	closed   chan struct{}
	once     sync.Once

	// open holds the connections that were handed out and are not closed
	// yet. Closing the listener closes them: a node that stops must not
	// leave its peers talking to connections nobody reads any more.
	mu   sync.Mutex
	open map[*tracked]struct{}
}

// tracked is a connection the listener handed out.
type tracked struct {
	net.Conn
	owner *listener
	state tls.ConnectionState
}

// ConnectionState returns the TLS state of the connection, from which
// PeerNodeID tells who is at the other end.
func (c *tracked) ConnectionState() tls.ConnectionState {
	return c.state
}

// Close implements net.Conn.
func (c *tracked) Close() error {
	c.owner.mu.Lock()
	delete(c.owner.open, c)
	c.owner.mu.Unlock()
	return c.Conn.Close()
}

func (l *listener) run(ctx context.Context) {
	for {
		raw, err := l.Listener.Accept()
		if err != nil {
			l.once.Do(func() { close(l.closed) })
			return
		}
		// Handshakes run side by side: a slow one must not hold up the next.
		go l.admit(ctx, raw)
	}
}

func (l *listener) admit(ctx context.Context, raw net.Conn) {
	conn := tls.Server(raw, l.config)
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	if err := conn.HandshakeContext(ctx); err != nil {
		l.refuse(peerRefusalOf(err))
		_ = conn.Close()
		return
	}
	nodeID, err := PeerNodeID(conn.ConnectionState())
	if err != nil {
		l.refuse(PeerRefusedIdentity)
		_ = conn.Close()
		return
	}
	if !l.allow(nodeID) {
		l.refuse(PeerRefusedNotAdmitted)
		_ = conn.Close()
		return
	}
	handed := &tracked{Conn: conn, owner: l, state: conn.ConnectionState()}
	l.mu.Lock()
	l.open[handed] = struct{}{}
	l.mu.Unlock()
	select {
	case l.accepted <- handed:
	case <-l.closed:
		_ = handed.Close()
	}
}

// Accept implements net.Listener.
func (l *listener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.accepted:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

// Close implements net.Listener.
func (l *listener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.closed) })
	l.mu.Lock()
	open := make([]*tracked, 0, len(l.open))
	for conn := range l.open {
		open = append(open, conn)
	}
	l.mu.Unlock()
	for _, conn := range open {
		_ = conn.Close()
	}
	return err
}

// Address joins a node ID and the address the node is reached at into the
// form Dial takes: "<node id>@<host:port>". Raft records its members by such
// addresses, so that whoever dials one knows which certificate to expect.
func Address(nodeID, hostPort string) string {
	return nodeID + "@" + hostPort
}

// SplitAddress undoes Address.
func SplitAddress(address string) (nodeID, hostPort string, err error) {
	nodeID, hostPort, ok := strings.Cut(address, "@")
	if !ok || hostPort == "" {
		return "", "", fmt.Errorf("%w: node address %q: want <node id>@<host:port>", domain.ErrInvalidArgument, address)
	}
	if err := cluster.ValidateNodeID(nodeID); err != nil {
		return "", "", err
	}
	return nodeID, hostPort, nil
}

// Dial connects to a node at an address of the form Address returns and
// completes the handshake, so that a connection that is returned is one to
// the node that was meant.
func (i *Identity) Dial(ctx context.Context, address string) (net.Conn, error) {
	nodeID, hostPort, err := SplitAddress(address)
	if err != nil {
		return nil, fmt.Errorf("nodetls.Dial: %w", err)
	}
	dialer := &tls.Dialer{Config: i.Client(nodeID)}
	conn, err := dialer.DialContext(ctx, "tcp", hostPort)
	if err != nil {
		// A node that cannot be reached refused nothing.
		var verification *tls.CertificateVerificationError
		if errors.As(err, &verification) {
			i.refuse(peerRefusalOf(err))
		}
		return nil, fmt.Errorf("nodetls.Dial: %s: %w", address, err)
	}
	return conn, nil
}
