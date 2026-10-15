// Package egress makes the outbound connections a target causes - a
// webhook, a NATS server, another deployment - under the one policy of
// ADR-0023 ("Outbound connections"):
//
//   - the address a name resolved to is judged by delivery.AllowedAddress in
//     the dialer's Control hook, at every connect, so that a name that
//     resolves to one address when it is checked and to another when it is
//     used gains nothing;
//   - TLS 1.2 or newer, verified against the target's CA or the system's
//     roots, with no way to skip verification; HTTP is sent over TLS only;
//   - no redirect is followed and no proxy is used, whatever the
//     environment says;
//   - 5 s to connect, 5 s for the handshake, the delivery timeout for the
//     headers of a response and for every read and write of a connection
//     that makes no progress; a response body is read to 64 KiB at most;
//   - an error names the origin of a request - its scheme, host and port -
//     and never its path or query, which may hold a token.
//
// The rules that judge an address are the domain's; this package applies
// them to sockets.
package egress

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/albievan/cactusfs/internal/domain/delivery"
)

// The bounds of an outbound connection (ADR-0023).
const (
	// ConnectTimeout bounds the connect to one address.
	ConnectTimeout = 5 * time.Second
	// HandshakeTimeout bounds the TLS handshake.
	HandshakeTimeout = 5 * time.Second
	// DefaultResponseTimeout is how long a target has to send the headers
	// of its response once the request is sent, when the policy says
	// nothing: the default of the setting delivery-timeout.
	DefaultResponseTimeout = 30 * time.Second
	// MinResponseTimeout is the shortest response timeout a policy may
	// have.
	MinResponseTimeout = time.Millisecond
	// MaxResponseBody is the most of a response body that is ever read.
	MaxResponseBody = 64 << 10
	// maxResponseHeader bounds the headers of a response.
	maxResponseHeader = 64 << 10
	// maxAddresses is the most addresses of one name that are tried.
	maxAddresses = 8
	// idleTimeout is the longest a connection is kept for the next
	// request; NewClient keeps it for half the response timeout when that
	// is shorter.
	idleTimeout = 90 * time.Second
)

// Errors of this package. Each failure that has a class of its own
// (delivery.Class) is a *delivery.Error that wraps one of these.
var (
	// ErrRefused is an address the deployment does not connect to.
	ErrRefused = errors.New("egress: address refused")
	// ErrRedirect is a response that redirects, which is never followed.
	ErrRedirect = errors.New("egress: redirect not followed")
	// ErrNotTLS is a request that would be sent in the clear.
	ErrNotTLS = errors.New("egress: only https is sent")
	// ErrResponseTooLarge is a response body longer than MaxResponseBody,
	// read to that bound.
	ErrResponseTooLarge = errors.New("egress: response body larger than 64 KiB")
	// errPolicy is a policy that cannot be applied.
	errPolicy = errors.New("egress: invalid policy")
)

// Resolver looks up the addresses of a name. *net.Resolver is one; a test
// injects its own.
type Resolver interface {
	// LookupNetIP returns the addresses of host for network "ip", "ip4"
	// or "ip6".
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Policy is what an outbound connection may do.
type Policy struct {
	// Allow is the setting egress-allow: the ranges where loopback,
	// link-local and the translated IPv6 ranges are admitted
	// (delivery.AllowedAddress). It never admits the addresses of the
	// metadata services of clouds.
	Allow []netip.Prefix
	// RootCAs verifies the certificate of a target: its own CA, or nil
	// for the system's roots.
	RootCAs *x509.CertPool
	// ResponseTimeout is the setting delivery-timeout: how long a target
	// has to send the headers of its response, and how long a connection
	// may make no progress. Zero is DefaultResponseTimeout; anything else
	// below MinResponseTimeout is refused.
	ResponseTimeout time.Duration
	// Resolver looks names up; nil is net.DefaultResolver.
	Resolver Resolver
}

// TLSConfig returns the TLS configuration of a connection under the policy:
// TLS 1.2 or newer, verified against RootCAs or the system's roots. A
// connection that does not go through net/http sets ServerName.
func (p Policy) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: p.RootCAs}
}

// DialFunc connects to "host:port" over TCP under the policy. It is what
// NewClient's HTTP client dials with, and what a client of another
// protocol - NATS - is given. Its connections have no deadline of their
// own: a client of a long-lived connection keeps it alive and bounds it as
// its protocol does.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Client sends HTTP requests under a policy. It is an *http.Client whose
// errors name the origin of a request and never its path or query.
type Client struct {
	http *http.Client
}

// Do sends r and returns the response, as http.Client.Do does - but for
// an error, which names the scheme, the host and the port of r and nothing
// of its path or its query, comes without a response, and has a class
// (delivery.ClassOf): a redirect, an address refused, a timeout, a failed
// handshake or a failed connection. The body of a response is read to
// MaxResponseBody at most, and a longer one fails with ErrResponseTooLarge
// there.
//
// A connection must make progress within the response timeout, reading or
// writing: a request body that the caller's reader hands over more slowly
// than that is no progress either, and fails as a timeout.
func (c *Client) Do(r *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(r)
	if err == nil {
		return resp, nil
	}
	if resp != nil && resp.Body != nil {
		// Only CheckRedirect returns a response with an error, closed.
		_ = resp.Body.Close()
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return nil, &url.Error{Op: urlErr.Op, URL: origin(r.URL), Err: classify(urlErr.Err)}
	}
	return nil, classify(err)
}

// classify gives an error that has no class of delivery one: a timeout,
// a handshake or a certificate that failed, or else a connection that
// failed. An error that has a class keeps it.
//
// A request that was cancelled - its context, by whoever sent it - is
// ClassInternal in every phase: no target failed, this side stopped
// (failedClass).
func classify(err error) error {
	var failure *delivery.Error
	if err == nil || errors.As(err, &failure) {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return delivery.Failed(delivery.ClassInternal, err)
	}
	var (
		verification *tls.CertificateVerificationError
		alert        tls.AlertError
		record       tls.RecordHeaderError
		authority    x509.UnknownAuthorityError
		hostname     x509.HostnameError
		invalid      x509.CertificateInvalidError
	)
	switch {
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return delivery.Failed(delivery.ClassTimeout, err)
	case errors.As(err, &verification), errors.As(err, &alert), errors.As(err, &record),
		errors.As(err, &authority), errors.As(err, &hostname), errors.As(err, &invalid):
		return delivery.Failed(delivery.ClassTLS, err)
	}
	return delivery.Failed(delivery.ClassConnect, err)
}

// CloseIdleConnections closes the connections that are kept for the next
// request and carry none now.
func (c *Client) CloseIdleConnections() {
	c.http.CloseIdleConnections()
}

// origin returns the scheme, the host and the port of u.
func origin(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// NewClient returns an HTTP client and a dial function that apply the
// policy. The client follows no redirect, uses no proxy, sends nothing but
// https, and bounds every response body to MaxResponseBody; it has no
// overall timeout - the context of a request is its deadline - but a
// connection that makes no progress for the response timeout, reading or
// writing, fails.
func NewClient(policy Policy) (*Client, DialFunc, error) {
	if policy.ResponseTimeout == 0 {
		policy.ResponseTimeout = DefaultResponseTimeout
	}
	if policy.ResponseTimeout < MinResponseTimeout {
		// Shorter, half of it - how long a connection is kept idle -
		// would be no time, which http.Transport takes for no limit.
		return nil, nil, fmt.Errorf("egress.NewClient: %w: a response timeout of less than %s", errPolicy, MinResponseTimeout)
	}
	allow := make([]netip.Prefix, 0, len(policy.Allow))
	for _, prefix := range policy.Allow {
		if !prefix.IsValid() {
			return nil, nil, fmt.Errorf("egress.NewClient: %w: a range of egress-allow is not one", errPolicy)
		}
		allow = append(allow, prefix.Masked())
	}
	d := &dialer{allow: allow, resolver: policy.Resolver}
	if d.resolver == nil {
		d.resolver = net.DefaultResolver
	}
	d.net = net.Dialer{Timeout: ConnectTimeout, Control: d.control}

	// Every read and write of a connection must make progress within the
	// response timeout (progressConn). Between two requests the transport
	// parks a read on a pooled connection, under that deadline: it is
	// closed after half of it, so that it is never handed to a request as
	// the deadline fires - the request's first write sets it again, for
	// both directions. A connection is kept idle for at most
	// min(90 s, response timeout / 2).
	idle := policy.ResponseTimeout
	keep := min(idleTimeout, idle/2)
	transport := &http.Transport{
		// No proxy, whatever HTTPS_PROXY and the others say.
		Proxy: nil,
		// The handshake is made here, not by the transport, so that the
		// connection the transport holds is the TLS one with its progress
		// deadline around it: a TLS connection that is closed fails a
		// write by itself, without reaching the connection below.
		DialTLSContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return d.dialTLS(ctx, network, address, policy.TLSConfig(), idle)
		},
		// What is not https is never dialled - bounded refuses it first,
		// and this is the second refusal: the transport would otherwise
		// dial it with a dialer of its own, outside the policy.
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, delivery.Failed(delivery.ClassInternal, ErrNotTLS)
		},
		ResponseHeaderTimeout:  policy.ResponseTimeout,
		MaxResponseHeaderBytes: maxResponseHeader,
		// A compressed body would be bounded after it is inflated, but
		// nothing is gained by asking for one.
		DisableCompression: true,
		IdleConnTimeout:    keep,
	}
	client := &http.Client{
		Transport: bounded{next: transport},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return delivery.Failed(delivery.ClassRedirect, ErrRedirect)
		},
	}
	return &Client{http: client}, d.dial, nil
}

// Discard reads what is left of a response body, to MaxResponseBody at
// most, throws it away and closes the body. A body of this client is read
// to its end, so that its connection is kept for the next request; a
// longer one is no error here: it is not read past the bound. A body of
// another client is read to MaxResponseBody.
func Discard(resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return nil
	}
	var body io.Reader = resp.Body
	if _, ours := resp.Body.(*limitedBody); !ours {
		body = io.LimitReader(resp.Body, MaxResponseBody)
	}
	_, err := io.Copy(io.Discard, body)
	if errors.Is(err, ErrResponseTooLarge) {
		err = nil
	}
	if closeErr := resp.Body.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("egress.Discard: %w", err)
	}
	return nil
}

// dialer resolves a name with the resolver of the policy and connects to
// its addresses one by one; Control judges each address as it is dialled.
type dialer struct {
	allow    []netip.Prefix
	resolver Resolver
	net      net.Dialer
}

// dial implements DialFunc.
func (d *dialer) dial(ctx context.Context, network, address string) (net.Conn, error) {
	family := ""
	switch network {
	case "tcp":
		family = "ip"
	case "tcp4":
		family = "ip4"
	case "tcp6":
		family = "ip6"
	default:
		return nil, fmt.Errorf("egress.Dial: %w: network %q", errPolicy, network)
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, delivery.Failed(delivery.ClassConnect, fmt.Errorf("egress.Dial: %w", err))
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return nil, delivery.Failed(delivery.ClassConnect, fmt.Errorf("egress.Dial: port %q", portText))
	}
	var addrs []netip.Addr
	if literal, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{literal}
	} else {
		addrs, err = d.resolver.LookupNetIP(ctx, family, host)
		if err != nil {
			return nil, delivery.Failed(failedClass(ctx, err, delivery.ClassConnect), fmt.Errorf("egress.Dial: resolve: %w", err))
		}
	}
	if len(addrs) == 0 {
		return nil, delivery.Failed(delivery.ClassConnect, errors.New("egress.Dial: the name has no address"))
	}
	var errs []error
	for _, addr := range addrs[:min(len(addrs), maxAddresses)] {
		conn, err := d.net.DialContext(ctx, network, netip.AddrPortFrom(addr, uint16(port)).String())
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	err = errors.Join(errs...)
	if errors.Is(err, ErrRefused) {
		// Some address was refused and none connected: the failure is
		// the policy's, whatever happened to the others.
		return nil, delivery.Failed(delivery.ClassEgressRefused, fmt.Errorf("egress.Dial: %w", err))
	}
	return nil, delivery.Failed(failedClass(ctx, err, delivery.ClassConnect), fmt.Errorf("egress.Dial: %w", err))
}

// dialTLS connects under the policy, makes the TLS handshake within
// HandshakeTimeout with config, verifying the host of address, and returns
// the TLS connection with a progress deadline of idle (progressConn). A
// handshake that fails is ClassTLS, or ClassTimeout when it took too long.
func (d *dialer) dialTLS(ctx context.Context, network, address string, config *tls.Config, idle time.Duration) (net.Conn, error) {
	raw, err := d.dial(ctx, network, address)
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		_ = raw.Close()
		return nil, delivery.Failed(delivery.ClassConnect, fmt.Errorf("egress.Dial: %w", err))
	}
	config.ServerName = host
	conn := tls.Client(raw, config)
	handshake, cancel := context.WithTimeout(ctx, HandshakeTimeout)
	defer cancel()
	if err := conn.HandshakeContext(handshake); err != nil {
		_ = raw.Close()
		return nil, delivery.Failed(failedClass(handshake, err, delivery.ClassTLS), fmt.Errorf("egress: handshake: %w", err))
	}
	return &progressConn{Conn: conn, raw: raw, idle: idle}, nil
}

// control is the dialer's Control hook: it judges the address a socket is
// about to connect to, which is the one dialled and not one looked up
// before.
func (d *dialer) control(_, address string, _ syscall.RawConn) error {
	target, err := netip.ParseAddrPort(address)
	if err != nil || !delivery.AllowedAddress(target.Addr(), d.allow) {
		return ErrRefused
	}
	return nil
}

// failedClass returns the class of a failure of a phase that ran under
// ctx: ClassInternal when the context was cancelled - this side stopped,
// whatever the phase - ClassTimeout when it ran out of time, and otherwise
// the class given.
func failedClass(ctx context.Context, err error, otherwise delivery.Class) delivery.Class {
	switch {
	case errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled):
		return delivery.ClassInternal
	case ctx.Err() != nil || isTimeout(err):
		return delivery.ClassTimeout
	}
	return otherwise
}

func isTimeout(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

// progressConn is a connection on which every read and every write must
// make progress within idle: each call sets a deadline of idle from now, for
// reading and writing both, so that a peer that stops reading a request or
// trickles a response fails, and one that is busy with either does not.
//
// Once the deadline has fired in one direction, the transport closes the
// connection, and what the other direction is doing fails with "use of
// closed network connection"; that failure, and any after it, is a stall
// (Timeout reports true), so that it is classed as the timeout it is.
type progressConn struct {
	net.Conn
	// raw is the connection under TLS, which Close closes.
	raw     net.Conn
	idle    time.Duration
	expired atomic.Bool
}

// Close implements net.Conn. It ends what is blocked at once - the
// deadline is set to now - and closes the connection under TLS without a
// close_notify: tls.Conn.Close would wait up to 5 s for a write that a peer
// that stopped reading never takes, and HTTP/1.1, framed by its lengths,
// does not need one.
func (c *progressConn) Close() error {
	_ = c.SetDeadline(time.Now())
	if c.raw != nil {
		return c.raw.Close()
	}
	return c.Conn.Close()
}

// Read implements net.Conn. io.EOF is returned as it is.
func (c *progressConn) Read(p []byte) (int, error) {
	if err := c.SetDeadline(time.Now().Add(c.idle)); err != nil {
		return 0, c.failed(fmt.Errorf("egress: deadline: %w", err))
	}
	n, err := c.Conn.Read(p)
	return n, c.failed(err)
}

// Write implements net.Conn.
func (c *progressConn) Write(p []byte) (int, error) {
	if err := c.SetDeadline(time.Now().Add(c.idle)); err != nil {
		return 0, c.failed(fmt.Errorf("egress: deadline: %w", err))
	}
	n, err := c.Conn.Write(p)
	return n, c.failed(err)
}

// failed records a deadline that fired, and returns a failure after one as
// a stall. Neither io.EOF nor no error is changed.
func (c *progressConn) failed(err error) error {
	switch {
	case err == nil || errors.Is(err, io.EOF):
		return err
	case isTimeout(err):
		c.expired.Store(true)
		return err
	case c.expired.Load():
		return &stall{cause: err}
	}
	return err
}

// stall is a failure of a connection whose progress deadline has fired.
type stall struct {
	cause error
}

// Error implements error.
func (s *stall) Error() string {
	return "egress: the connection made no progress: " + s.cause.Error()
}

// Unwrap returns the failure itself.
func (s *stall) Unwrap() error {
	return s.cause
}

// Timeout reports true: a stall is a timeout.
func (s *stall) Timeout() bool {
	return true
}

// ConnectionState returns the state of the TLS connection, which the
// transport does not ask of a connection it did not make itself.
func (c *progressConn) ConnectionState() tls.ConnectionState {
	if conn, ok := c.Conn.(*tls.Conn); ok {
		return conn.ConnectionState()
	}
	return tls.ConnectionState{}
}

// bounded sends https only, bounds every response body to
// MaxResponseBody, and fills in the TLS state of a response.
type bounded struct {
	next http.RoundTripper
}

// CloseIdleConnections closes the idle connections of the transport it
// wraps, which http.Client.CloseIdleConnections reaches only through it.
func (b bounded) CloseIdleConnections() {
	if closer, ok := b.next.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// RoundTrip implements http.RoundTripper.
func (b bounded) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL == nil || r.URL.Scheme != "https" {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, delivery.Failed(delivery.ClassInternal, ErrNotTLS)
	}
	// The transport tells the TLS state of a connection it made itself
	// only; this one is the dialer's, and is asked here.
	var (
		mu   sync.Mutex
		used net.Conn
	)
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		mu.Lock()
		used = info.Conn
		mu.Unlock()
	}}
	resp, err := b.next.RoundTrip(r.WithContext(httptrace.WithClientTrace(r.Context(), trace)))
	if err != nil {
		return nil, err
	}
	mu.Lock()
	state, ok := used.(interface{ ConnectionState() tls.ConnectionState })
	mu.Unlock()
	if resp.TLS == nil && ok {
		connection := state.ConnectionState()
		resp.TLS = &connection
	}
	if resp.StatusCode >= 300 && resp.StatusCode <= 399 {
		// Refused here, before http.Client reads Location: a redirect is
		// never followed, and what it points to - which a malformed
		// Location would put in an error - is never looked at.
		_ = resp.Body.Close()
		return nil, delivery.Failed(delivery.ClassRedirect, ErrRedirect)
	}
	if resp.ContentLength > MaxResponseBody {
		resp.ContentLength = -1
	}
	resp.Body = &limitedBody{body: resp.Body, left: MaxResponseBody}
	return resp, nil
}

// limitedBody is a response body read to MaxResponseBody. What is longer
// ends in ErrResponseTooLarge, not in io.EOF: a body cut at the bound is not
// taken for a whole one.
type limitedBody struct {
	body io.ReadCloser
	left int64
}

// Read implements io.Reader.
func (l *limitedBody) Read(p []byte) (int, error) {
	if l.left > 0 {
		if int64(len(p)) > l.left {
			p = p[:l.left]
		}
		n, err := l.body.Read(p)
		l.left -= int64(n)
		return n, readError(err)
	}
	// At the bound: anything more is too much.
	var one [1]byte
	for {
		n, err := l.body.Read(one[:])
		switch {
		case n > 0:
			return 0, delivery.Failed(delivery.ClassServer, ErrResponseTooLarge)
		case err != nil:
			return 0, readError(err)
		}
	}
}

// Close implements io.Closer.
func (l *limitedBody) Close() error {
	return l.body.Close()
}

// readError returns io.EOF as it is - the end of a body is no failure - and
// gives any other error of reading a body its class.
func readError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, io.EOF):
		// The end of a body is io.EOF itself, never wrapped.
		return io.EOF
	}
	return classify(err)
}
