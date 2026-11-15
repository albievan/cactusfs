// Package nats publishes the messages of a target of kind nats to the one
// subject of the target on a NATS server with JetStream (ADR-0025, "NATS
// JetStream"): a ports.EventPublisher, one a target.
//
//   - Nothing is connected until the first Publish: a target that cannot be
//     reached holds up neither the start of a node, nor its readiness, nor
//     a write.
//   - The connection is made through the dial function of the egress client
//     (ADR-0023), so every address - the endpoint's, and any the server
//     names - is judged by delivery.AllowedAddress when it is dialled. No
//     name is looked up outside it, servers the server tells of are not
//     used, and a connection that is lost is made again by the next
//     Publish, not in the background.
//   - TLS is required, 1.2 or newer, verified against the target's
//     certificates or the system's roots. A server that offers no TLS is
//     refused before anything of the credential is sent.
//   - The client proves who it is with the token, or the user and the
//     password, that the target holds sealed; the secret is opened when
//     the connection is made.
//   - Every message is published with the header Nats-Msg-Id set to the ID
//     of the message (jetstream.WithMsgID), by which JetStream deduplicates
//     it within its window, and Cactusfs-Event-Type; Publish returns once
//     the stream has acknowledged it.
//
// A failure is a delivery.Error whose class says what it was (ADR-0025,
// "Failures"): a subject that no stream takes is delivery.ClassNoStream, a
// credential the server refuses delivery.ClassAuth, a server that did not
// answer in time delivery.ClassTimeout. Its text is the class and nothing
// else; what it wraps names neither the secret nor anything of the target
// but what the error of a library or the system says.
package nats

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/albievan/cactusfs/internal/adapters/egress"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// EventTypeHeader is the header that names the type of a message: one of
// ports.MessageNotificationObject and ports.MessageAuditExported.
const EventTypeHeader = "Cactusfs-Event-Type"

// ConnectTimeout bounds the making of a connection once the socket is
// open: the TLS handshake, and the server's answer to the client's CONNECT.
// A Publish whose context ends sooner gives it less.
const ConnectTimeout = egress.HandshakeTimeout

// clientName is the name the client gives the server.
const clientName = "cactusfs"

// Errors of this package. Each is wrapped in a delivery.Error of the class
// it is.
var (
	// ErrClosed is a Publish after Close.
	ErrClosed = errors.New("nats: publisher closed")
	// ErrNoEventID is a message without an ID or a type: JetStream could
	// not deduplicate it, nor a consumer tell what it is.
	ErrNoEventID = errors.New("nats: a message needs an ID and a type")
)

// Secrets opens what a target holds sealed.
type Secrets interface {
	// Open returns the plaintext of sealed, which was sealed with aad.
	// The caller clears it when it is done with it.
	Open(ctx context.Context, sealed iam.SealedSecret, aad []byte) ([]byte, error)
}

// KMSSecrets opens a sealed field of a target through the KMS that sealed
// it, as the secrets of API keys are opened: the data key is unwrapped with
// the field's additional data, which binds it to the target and the field.
type KMSSecrets struct {
	// KMS is the node's KMS.
	KMS ports.KMS
}

// Open implements Secrets. A secret sealed under another KMS key than the
// node's wraps domain.ErrKeyUnavailable.
func (s KMSSecrets) Open(ctx context.Context, sealed iam.SealedSecret, aad []byte) ([]byte, error) {
	if s.KMS == nil {
		return nil, fmt.Errorf("nats.KMSSecrets: %w: no KMS", domain.ErrKeyUnavailable)
	}
	if sealed.KMSKeyID != s.KMS.KeyID() {
		return nil, fmt.Errorf("nats.KMSSecrets: %w: sealed under another KMS key", domain.ErrKeyUnavailable)
	}
	key, err := s.KMS.UnwrapDataKey(ctx, sealed.WrappedKey, aad)
	if err != nil {
		return nil, fmt.Errorf("nats.KMSSecrets: %w", err)
	}
	defer clear(key)
	plain, err := sealed.Open(key, aad)
	if err != nil {
		return nil, fmt.Errorf("nats.KMSSecrets: %w", err)
	}
	return plain, nil
}

// session is what a publisher needs of a connection to a server with
// JetStream. The tests stand a fake in for it.
type session interface {
	// PublishMsg publishes msg and waits for the stream's
	// acknowledgement, as jetstream.JetStream's does.
	PublishMsg(ctx context.Context, msg *natsgo.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error)
	// Closed reports whether the connection is closed.
	Closed() bool
	// Close closes the connection.
	Close()
}

// connectFunc makes a session.
type connectFunc func(ctx context.Context) (session, error)

// Publisher publishes the messages of one target. It is safe for concurrent
// use; messages are published one at a time, in the order Publish is
// called.
type Publisher struct {
	target  delivery.Target
	subject string
	connect connectFunc

	// mu makes Publish one at a time. It is held across a connect and
	// an acknowledgement's wait, so Close does not take it.
	mu sync.Mutex
	// state guards session and closed, and is never held across I/O.
	state   sync.Mutex
	session session
	closed  bool

	// async is the last error the server sent outside an answer since
	// the publish in progress began - a permissions violation is one.
	async atomic.Pointer[error]
}

// New returns the publisher of a target of kind nats. It connects to
// nothing: the connection is made by the first Publish, through dial, with
// the secret secrets opens. dial is the dial function of the egress client
// (egress.NewClient).
//
// A target that is not of kind nats, has no subject, has an endpoint that
// is not one or certificates that cannot be read, or proves itself in a way
// a NATS server is not proved to, is refused with an error that wraps
// domain.ErrInvalidArgument, of class delivery.ClassInternal.
func New(target delivery.Target, secrets Secrets, dial egress.DialFunc) (*Publisher, error) {
	if dial == nil || secrets == nil {
		return nil, delivery.Failed(delivery.ClassInternal, fmt.Errorf("nats.New: %w: no dial function or no secrets", domain.ErrInvalidArgument))
	}
	c, err := newConnector(target, secrets, dial)
	if err != nil {
		return nil, delivery.Failed(delivery.ClassInternal, fmt.Errorf("nats.New: %w", err))
	}
	p := &Publisher{target: target, subject: target.Subject}
	c.async = p.recordAsync
	p.connect = c.connect
	return p, nil
}

// newPublisher returns a publisher that makes its sessions with connect.
func newPublisher(target delivery.Target, connect connectFunc) *Publisher {
	return &Publisher{target: target, subject: target.Subject, connect: connect}
}

// String names the target of the publisher, and nothing else of it.
func (p *Publisher) String() string {
	return "nats publisher of " + p.target.String()
}

// Publish implements ports.EventPublisher. It publishes the payload of e to
// the target's subject with the ID of e as Nats-Msg-Id and the type of e as
// Cactusfs-Event-Type, and returns once the stream has acknowledged it. The
// payload is e.Data as it is when it is a []byte or a json.RawMessage, and
// e.Data as JSON otherwise.
//
// The connection is made when there is none; one that failed is closed,
// and the next Publish makes another.
func (p *Publisher) Publish(ctx context.Context, e ports.Event) error {
	if e.ID == "" || e.Type == "" {
		return delivery.Failed(delivery.ClassInternal, fmt.Errorf("nats.Publish: %w", ErrNoEventID))
	}
	payload, err := payloadOf(e.Data)
	if err != nil {
		return delivery.Failed(delivery.ClassInternal, fmt.Errorf("nats.Publish: %w", err))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.async.Store(nil)
	s, err := p.current()
	if err != nil {
		return err
	}
	if s == nil {
		if s, err = p.connect(ctx); err != nil {
			return delivery.Failed(classOf(ctx, err), fmt.Errorf("nats.Publish: %w", err))
		}
		if err := p.keep(s); err != nil {
			return err
		}
	}
	msg := natsgo.NewMsg(p.subject)
	msg.Data = payload
	// WithMsgID sets this header as well; it is set here too so that
	// what is sent does not hang on how an option is applied.
	msg.Header.Set(jetstream.MsgIDHeader, e.ID)
	msg.Header.Set(EventTypeHeader, e.Type)
	_, err = s.PublishMsg(ctx, msg, jetstream.WithMsgID(e.ID))
	if err == nil {
		return nil
	}
	if async := p.async.Load(); async != nil && errors.Is(*async, natsgo.ErrPermissionViolation) {
		// The server refused the publish for want of a permission and
		// the request ran out of time: the credential is at fault.
		err = fmt.Errorf("%w: %w", natsgo.ErrPermissionViolation, err)
	}
	class := classOf(ctx, err)
	if class != delivery.ClassNoStream && class != delivery.ClassServer {
		// Only an answer of the server says the connection is sound.
		p.forget(s)
	}
	return delivery.Failed(class, fmt.Errorf("nats.Publish: %w", err))
}

// Close closes the connection, if there is one, without waiting for a
// Publish in progress: one that waits for an acknowledgement fails with
// natsgo.ErrConnectionClosed, one that is connecting closes what it made.
// A Publish after Close fails.
func (p *Publisher) Close() error {
	p.state.Lock()
	p.closed = true
	s := p.session
	p.session = nil
	p.state.Unlock()
	if s != nil {
		s.Close()
	}
	return nil
}

// current returns the session to publish on: nil when one is to be made.
// A session the client closed by itself is forgotten.
func (p *Publisher) current() (session, error) {
	p.state.Lock()
	defer p.state.Unlock()
	if p.closed {
		return nil, delivery.Failed(delivery.ClassInternal, fmt.Errorf("nats.Publish: %w", ErrClosed))
	}
	if p.session != nil && p.session.Closed() {
		p.session.Close()
		p.session = nil
	}
	return p.session, nil
}

// keep makes s the session of the publisher, unless the publisher was
// closed while s was made: then s is closed.
func (p *Publisher) keep(s session) error {
	p.state.Lock()
	closed := p.closed
	if !closed {
		p.session = s
	}
	p.state.Unlock()
	if closed {
		s.Close()
		return delivery.Failed(delivery.ClassInternal, fmt.Errorf("nats.Publish: %w", ErrClosed))
	}
	return nil
}

// forget closes s and forgets it, if it is still the publisher's session.
func (p *Publisher) forget(s session) {
	p.state.Lock()
	if p.session == s {
		p.session = nil
	}
	p.state.Unlock()
	s.Close()
}

// recordAsync keeps an error the server sent outside an answer, for the
// publish in progress to be classed by.
func (p *Publisher) recordAsync(err error) {
	if err != nil {
		p.async.Store(&err)
	}
}

// payloadOf returns the bytes a message carries.
func payloadOf(data any) ([]byte, error) {
	switch d := data.(type) {
	case []byte:
		return d, nil
	case json.RawMessage:
		return d, nil
	}
	out, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("encode the message: %w", err)
	}
	return out, nil
}

// classOf returns the class of a failure of a phase that ran under ctx.
func classOf(ctx context.Context, err error) delivery.Class {
	var classed *delivery.Error
	var api *jetstream.APIError
	var certificate *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var record tls.RecordHeaderError
	var alert tls.AlertError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.As(err, &classed):
		// The egress dialer's - connect, egress_refused, timeout - and
		// the failure to open the secret.
		return classed.Class
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		// This side stopped: no target failed.
		return delivery.ClassInternal
	case errors.Is(err, jetstream.ErrNoStreamResponse) || errors.Is(err, natsgo.ErrNoResponders):
		return delivery.ClassNoStream
	case errors.Is(err, natsgo.ErrAuthorization) || errors.Is(err, natsgo.ErrAuthExpired) ||
		errors.Is(err, natsgo.ErrAuthRevoked) || errors.Is(err, natsgo.ErrAccountAuthExpired) ||
		errors.Is(err, natsgo.ErrPermissionViolation):
		return delivery.ClassAuth
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, natsgo.ErrTimeout) || ctx.Err() != nil || timeout(err):
		return delivery.ClassTimeout
	case errors.Is(err, natsgo.ErrTLS) || errors.Is(err, natsgo.ErrSecureConnWanted) ||
		errors.Is(err, natsgo.ErrSecureConnRequired) || errors.Is(err, natsgo.ErrConnectionNotTLS) ||
		errors.As(err, &certificate) || errors.As(err, &unknownAuthority) || errors.As(err, &hostname) ||
		errors.As(err, &record) || errors.As(err, &alert) || errors.As(err, &invalid) || remoteAlert(err):
		return delivery.ClassTLS
	case errors.Is(err, natsgo.ErrNoServers) || errors.Is(err, natsgo.ErrConnectionClosed) ||
		errors.Is(err, natsgo.ErrDisconnected) || errors.Is(err, natsgo.ErrStaleConnection) ||
		errors.Is(err, natsgo.ErrConnectionReconnecting) || errors.Is(err, natsgo.ErrNoInfoReceived) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed):
		return delivery.ClassConnect
	case errors.Is(err, natsgo.ErrMaxPayload) || errors.Is(err, jetstream.ErrStreamMessageExceedsMaximum):
		// A message larger than the server or the stream takes. The
		// bound is the operator's configuration, not this message's
		// fault: it is held, and the target stalls, until the bound is
		// raised - never passed, which would drop every record above it.
		return delivery.ClassServer
	case errors.As(err, &api) || errors.Is(err, jetstream.ErrInvalidJSAck) ||
		errors.Is(err, natsgo.ErrMaxConnectionsExceeded) || errors.Is(err, natsgo.ErrMaxAccountConnectionsExceeded):
		return delivery.ClassServer
	}
	return delivery.ClassInternal
}

// remoteAlert reports whether err is a TLS alert the server sent after the
// handshake - in TLS 1.3 a server that requires a client certificate says
// so only then - which crypto/tls returns as a net.OpError of the
// operation "remote error".
func remoteAlert(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "remote error"
}

func timeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

// connector makes the sessions of one target.
type connector struct {
	url     string
	host    string
	roots   *x509.CertPool
	auth    delivery.Auth
	sealed  iam.SealedSecret
	aad     []byte
	secrets Secrets
	dial    egress.DialFunc
	// async is told the errors the server sends outside an answer.
	async func(error)
}

// newConnector checks target and returns what connects to it.
func newConnector(target delivery.Target, secrets Secrets, dial egress.DialFunc) (*connector, error) {
	if target.Kind != delivery.KindNATS {
		return nil, fmt.Errorf("%w: %s is not of kind nats", domain.ErrInvalidArgument, target)
	}
	if target.Subject == "" {
		return nil, fmt.Errorf("%w: %s has no subject", domain.ErrInvalidArgument, target)
	}
	// The error of CheckEndpoint repeats nothing of the endpoint.
	endpoint, err := delivery.CheckEndpoint(delivery.KindNATS, target.Endpoint)
	if err != nil {
		return nil, err
	}
	c := &connector{
		url: endpoint.Origin(), host: endpoint.Host, auth: target.Auth,
		sealed: target.Secret, aad: delivery.SecretAAD(target.Name), secrets: secrets, dial: dial,
	}
	if len(target.CAPEM) > 0 {
		c.roots = x509.NewCertPool()
		if !c.roots.AppendCertsFromPEM(target.CAPEM) {
			return nil, fmt.Errorf("%w: the certificates of %s cannot be read", domain.ErrInvalidArgument, target)
		}
	}
	switch c.auth.Mode {
	case "", delivery.AuthNone:
	case delivery.AuthToken, delivery.AuthBasic:
		if !target.SecretSet() {
			return nil, fmt.Errorf("%w: %s proves itself with a secret it does not have", domain.ErrInvalidArgument, target)
		}
	default:
		return nil, fmt.Errorf("%w: %s proves itself in a way a NATS server is not proved to", domain.ErrInvalidArgument, target)
	}
	return c, nil
}

// tlsConfig returns the TLS configuration of a connection: TLS 1.2 or
// newer, verified against the target's certificates or the system's roots,
// for the host of the endpoint.
func (c *connector) tlsConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: c.roots, ServerName: c.host}
}

// dialFunc is a natsgo.CustomDialer made of a function.
type dialFunc func(network, address string) (net.Conn, error)

// Dial implements natsgo.CustomDialer.
func (f dialFunc) Dial(network, address string) (net.Conn, error) {
	return f(network, address)
}

// options returns the options of a connection made under ctx, proved with
// secret. The secret is given to the client through a handler, which reads
// it when the CONNECT is written, so that it is not kept as a string in the
// connection's options for as long as the connection lives; connect clears
// it once the connection is made, and the client never asks again
// (NoReconnect).
func (c *connector) options(ctx context.Context, secret []byte) []natsgo.Option {
	wait := ConnectTimeout
	if deadline, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(deadline))
	}
	// A time of nothing would be refused as invalid, not as late.
	wait = max(wait, time.Millisecond)
	// The dialer dials under the context of the Publish that makes the
	// connection. Connections are never made again in the background
	// (NoReconnect), so it is not called after that Publish.
	dial := dialFunc(func(network, address string) (net.Conn, error) {
		return c.dial(ctx, network, address)
	})
	opts := []natsgo.Option{
		natsgo.Name(clientName),
		natsgo.SetCustomDialer(dial),
		natsgo.Secure(c.tlsConfig()),
		natsgo.Timeout(wait),
		// Names are looked up by the egress dialer, which judges what
		// they resolve to; servers the server tells of are not used; a
		// lost connection is made again by the next Publish.
		natsgo.SkipHostLookup(),
		natsgo.IgnoreDiscoveredServers(),
		natsgo.NoReconnect(),
		natsgo.NoCallbacksAfterClientClose(),
		// The default handler writes to stderr; these are kept for the
		// publish in progress to be classed by, and are never logged.
		natsgo.ErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) {
			if c.async != nil {
				c.async(err)
			}
		}),
	}
	switch c.auth.Mode {
	case delivery.AuthToken:
		opts = append(opts, natsgo.TokenHandler(func() string { return string(secret) }))
	case delivery.AuthBasic:
		opts = append(opts, natsgo.UserInfoHandler(func() (string, string) { return c.auth.ID, string(secret) }))
	}
	return opts
}

// connect opens the secret, connects and returns a JetStream session.
func (c *connector) connect(ctx context.Context) (session, error) {
	var secret []byte
	if c.auth.Mode == delivery.AuthToken || c.auth.Mode == delivery.AuthBasic {
		var err error
		secret, err = c.secrets.Open(ctx, c.sealed, c.aad)
		if err != nil {
			// The credential could not be opened: a failure on this
			// side of the connection.
			return nil, delivery.Failed(delivery.ClassInternal, fmt.Errorf("open the secret: %w", err))
		}
		defer clear(secret)
	}
	nc, err := natsgo.Connect(c.url, c.options(ctx, secret)...)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	return &jsSession{conn: nc, js: js}, nil
}

// jsSession is a session over a connection of the NATS client.
type jsSession struct {
	conn *natsgo.Conn
	js   jetstream.JetStream
}

// PublishMsg implements session.
func (s *jsSession) PublishMsg(ctx context.Context, msg *natsgo.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	ack, err := s.js.PublishMsg(ctx, msg, opts...)
	if err != nil {
		return nil, fmt.Errorf("publish: %w", err)
	}
	return ack, nil
}

// Closed implements session.
func (s *jsSession) Closed() bool { return s.conn.IsClosed() }

// Close implements session.
func (s *jsSession) Close() { s.conn.Close() }

// Publisher is a ports.EventPublisher.
var _ ports.EventPublisher = (*Publisher)(nil)
