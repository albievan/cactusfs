package nats

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/albievan/cactusfs/internal/adapters/sealed"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// Marked values: what must never be in an error's text.
const (
	markedValue = "MARKED-token-6f1c"
	markedUser  = "marked-user"
)

// fakeSession is a JetStream session that answers what it is told to.
type fakeSession struct {
	mu      sync.Mutex
	errs    []error
	msgs    []*natsgo.Msg
	opts    [][]jetstream.PublishOpt
	closed  bool
	closes  int
	publish func(ctx context.Context) error
}

func (s *fakeSession) PublishMsg(ctx context.Context, msg *natsgo.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	s.mu.Lock()
	s.msgs = append(s.msgs, msg)
	s.opts = append(s.opts, opts)
	var err error
	if len(s.errs) > 0 {
		err, s.errs = s.errs[0], s.errs[1:]
	}
	publish := s.publish
	s.mu.Unlock()
	if publish != nil {
		err = publish(ctx)
	}
	if err != nil {
		return nil, err
	}
	return &jetstream.PubAck{Stream: "S", Sequence: uint64(len(s.msgs))}, nil
}

func (s *fakeSession) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *fakeSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.closes++
}

// fakeConnect makes fake sessions, or fails as it is told to.
type fakeConnect struct {
	mu       sync.Mutex
	err      error
	sessions []*fakeSession
	// next is given the next session made, when set.
	next func(*fakeSession)
}

func (c *fakeConnect) connect(context.Context) (session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	s := &fakeSession{}
	if c.next != nil {
		c.next(s)
	}
	c.sessions = append(c.sessions, s)
	return s, nil
}

func natsTarget() delivery.Target {
	return delivery.Target{
		Name: "events", Kind: delivery.KindNATS, Endpoint: "tls://nats.example.com:4222",
		Subject: "cactusfs.notifications", Auth: delivery.Auth{Mode: delivery.AuthToken},
		Secret: iam.SealedSecret{KMSKeyID: "k", WrappedKey: []byte("w"), Nonce: []byte("n"), Ciphertext: []byte("sealed")},
	}
}

func event(id string) ports.Event {
	return ports.Event{ID: id, Type: ports.MessageNotificationObject, Data: json.RawMessage(`{"Records":[]}`)}
}

func TestPublishSendsTheMessage(t *testing.T) {
	t.Parallel()
	fc := &fakeConnect{}
	p := newPublisher(natsTarget(), fc.connect)
	for _, id := range []string{"00000000000000000001", "00000000000000000002"} {
		if err := p.Publish(context.Background(), event(id)); err != nil {
			t.Fatalf("Publish(%s) = %v", id, err)
		}
	}
	if len(fc.sessions) != 1 {
		t.Fatalf("%d connections made, want 1", len(fc.sessions))
	}
	s := fc.sessions[0]
	for i, want := range []string{"00000000000000000001", "00000000000000000002"} {
		msg := s.msgs[i]
		if msg.Subject != "cactusfs.notifications" {
			t.Errorf("subject %q", msg.Subject)
		}
		if got := msg.Header.Get(jetstream.MsgIDHeader); got != want {
			t.Errorf("Nats-Msg-Id %q, want %q", got, want)
		}
		if got := msg.Header.Get("Cactusfs-Event-Type"); got != ports.MessageNotificationObject {
			t.Errorf("Cactusfs-Event-Type %q", got)
		}
		if string(msg.Data) != `{"Records":[]}` {
			t.Errorf("payload %q", msg.Data)
		}
		if len(s.opts[i]) != 1 {
			t.Errorf("%d publish options, want WithMsgID alone", len(s.opts[i]))
		}
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if !s.closed {
		t.Error("Close left the connection open")
	}
	if err := p.Publish(context.Background(), event("3")); !errors.Is(err, ErrClosed) || delivery.ClassOf(err) != delivery.ClassInternal {
		t.Errorf("Publish after Close = %v", err)
	}
}

func TestPublishPayload(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		data any
		want string
		fail bool
	}{
		"bytes as they are":    {data: []byte("raw"), want: "raw"},
		"raw JSON as it is":    {data: json.RawMessage(`{"a":1}`), want: `{"a":1}`},
		"anything else, JSON":  {data: map[string]int{"a": 1}, want: `{"a":1}`},
		"what JSON cannot say": {data: make(chan int), fail: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fc := &fakeConnect{}
			p := newPublisher(natsTarget(), fc.connect)
			err := p.Publish(context.Background(), ports.Event{ID: "1", Type: ports.MessageAuditExported, Data: tc.data})
			if tc.fail {
				if delivery.ClassOf(err) != delivery.ClassInternal || len(fc.sessions) != 0 {
					t.Fatalf("Publish = %v after %d connections, want internal before any", err, len(fc.sessions))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := string(fc.sessions[0].msgs[0].Data); got != tc.want {
				t.Errorf("payload %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPublishRefusesAMessageWithoutIDOrType(t *testing.T) {
	t.Parallel()
	fc := &fakeConnect{}
	p := newPublisher(natsTarget(), fc.connect)
	for _, e := range []ports.Event{{Type: ports.MessageNotificationObject}, {ID: "1"}} {
		err := p.Publish(context.Background(), e)
		if !errors.Is(err, ErrNoEventID) || delivery.ClassOf(err) != delivery.ClassInternal {
			t.Errorf("Publish(%+v) = %v", e, err)
		}
	}
	if len(fc.sessions) != 0 {
		t.Error("a message that cannot be sent made a connection")
	}
}

func TestPublishClasses(t *testing.T) {
	t.Parallel()
	apiErr := &jetstream.APIError{Code: 503, ErrorCode: 10008, Description: "jetstream unavailable"}
	tests := map[string]struct {
		connectErr error
		publishErr error
		async      error
		want       delivery.Class
		// kept says the connection outlives the failure.
		kept bool
	}{
		"no stream takes the subject":   {publishErr: fmt.Errorf("publish: %w", jetstream.ErrNoStreamResponse), want: delivery.ClassNoStream, kept: true},
		"no responders":                 {publishErr: natsgo.ErrNoResponders, want: delivery.ClassNoStream, kept: true},
		"the token is refused":          {connectErr: fmt.Errorf("connect: %w", natsgo.ErrAuthorization), want: delivery.ClassAuth},
		"the credential expired":        {connectErr: natsgo.ErrAuthExpired, want: delivery.ClassAuth},
		"the credential was revoked":    {connectErr: natsgo.ErrAuthRevoked, want: delivery.ClassAuth},
		"publishing is not permitted":   {publishErr: context.DeadlineExceeded, async: fmt.Errorf("%w: Permissions Violation for Publish to \"x\"", natsgo.ErrPermissionViolation), want: delivery.ClassAuth},
		"no acknowledgement in time":    {publishErr: fmt.Errorf("publish: %w", context.DeadlineExceeded), want: delivery.ClassTimeout},
		"the client's timeout":          {publishErr: natsgo.ErrTimeout, want: delivery.ClassTimeout},
		"the handshake failed":          {connectErr: fmt.Errorf("%w: %w", natsgo.ErrTLS, errors.New("bad certificate")), want: delivery.ClassTLS},
		"the server offers no TLS":      {connectErr: natsgo.ErrSecureConnWanted, want: delivery.ClassTLS},
		"an unknown authority":          {connectErr: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, want: delivery.ClassTLS},
		"the dialer's class is kept":    {connectErr: delivery.Failed(delivery.ClassEgressRefused, errors.New("refused")), want: delivery.ClassEgressRefused},
		"the dialer could not connect":  {connectErr: delivery.Failed(delivery.ClassConnect, errors.New("refused")), want: delivery.ClassConnect},
		"no server could be reached":    {connectErr: natsgo.ErrNoServers, want: delivery.ClassConnect},
		"the connection was closed":     {publishErr: natsgo.ErrConnectionClosed, want: delivery.ClassConnect},
		"the connection ended":          {publishErr: io.EOF, want: delivery.ClassConnect},
		"the connection was reset":      {connectErr: fmt.Errorf("connect: %w", &net.OpError{Op: "write", Net: "tcp", Err: errors.New("reset by peer")}), want: delivery.ClassConnect},
		"the stream refused":            {publishErr: fmt.Errorf("nats: %w", apiErr), want: delivery.ClassServer, kept: true},
		"no acknowledgement to be read": {publishErr: jetstream.ErrInvalidJSAck, want: delivery.ClassServer, kept: true},
		"the server is full":            {connectErr: natsgo.ErrMaxConnectionsExceeded, want: delivery.ClassServer},
		"a message too large":           {publishErr: natsgo.ErrMaxPayload, want: delivery.ClassServer, kept: true},
		"the stream's maximum size":     {publishErr: jetstream.ErrStreamMessageExceedsMaximum, want: delivery.ClassServer, kept: true},
		"anything else":                 {publishErr: errors.New("strange"), want: delivery.ClassInternal},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var p *Publisher
			fc := &fakeConnect{err: tc.connectErr}
			fc.next = func(s *fakeSession) {
				s.publish = func(context.Context) error {
					if tc.async != nil {
						p.recordAsync(tc.async)
					}
					return tc.publishErr
				}
			}
			p = newPublisher(natsTarget(), fc.connect)
			err := p.Publish(context.Background(), event("1"))
			if got := delivery.ClassOf(err); got != tc.want {
				t.Fatalf("class %q, want %q (%v)", got, tc.want, err)
			}
			if got := err.Error(); got != "delivery failed: "+string(tc.want) {
				t.Errorf("text %q tells more than the class", got)
			}
			if tc.connectErr != nil {
				return
			}
			s := fc.sessions[0]
			if s.closed == tc.kept {
				t.Errorf("connection closed %v, want %v", s.closed, !tc.kept)
			}
		})
	}
}

func TestPublishCancelledIsInternal(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fc := &fakeConnect{}
	fc.next = func(s *fakeSession) { s.publish = func(ctx context.Context) error { return ctx.Err() } }
	err := newPublisher(natsTarget(), fc.connect).Publish(ctx, event("1"))
	if got := delivery.ClassOf(err); got != delivery.ClassInternal {
		t.Errorf("class %q, want internal: this side stopped", got)
	}
}

func TestPublishConnectsAgainAfterAFailure(t *testing.T) {
	t.Parallel()
	fc := &fakeConnect{}
	fc.next = func(s *fakeSession) { s.errs = []error{natsgo.ErrTimeout} }
	p := newPublisher(natsTarget(), fc.connect)
	if err := p.Publish(context.Background(), event("1")); delivery.ClassOf(err) != delivery.ClassTimeout {
		t.Fatalf("first Publish = %v", err)
	}
	if err := p.Publish(context.Background(), event("1")); delivery.ClassOf(err) != delivery.ClassTimeout {
		t.Fatalf("second Publish = %v", err)
	}
	fc.next = nil
	if err := p.Publish(context.Background(), event("1")); err != nil {
		t.Fatal(err)
	}
	if len(fc.sessions) != 3 {
		t.Errorf("%d connections, want one for each attempt after a timeout", len(fc.sessions))
	}

	// A connection the client closed by itself is not used again.
	fc.sessions[2].Close()
	if err := p.Publish(context.Background(), event("2")); err != nil {
		t.Fatal(err)
	}
	if len(fc.sessions) != 4 {
		t.Errorf("%d connections, want a new one for a closed one", len(fc.sessions))
	}
}

func TestCloseDoesNotWaitForAPublish(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	fc := &fakeConnect{}
	fc.next = func(s *fakeSession) {
		s.publish = func(ctx context.Context) error {
			close(started)
			// Waits for its acknowledgement until the connection is
			// closed, as the client does.
			for !s.Closed() {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Millisecond):
				}
			}
			return natsgo.ErrConnectionClosed
		}
	}
	p := newPublisher(natsTarget(), fc.connect)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- p.Publish(ctx, event("1")) }()
	<-started

	closed := make(chan struct{})
	go func() {
		_ = p.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close waited for the Publish in flight")
	}
	err := <-result
	if !errors.Is(err, natsgo.ErrConnectionClosed) || delivery.ClassOf(err) != delivery.ClassConnect {
		t.Errorf("the Publish in flight = %v; want the connection closed", err)
	}
	if err := p.Publish(ctx, event("2")); !errors.Is(err, ErrClosed) {
		t.Errorf("Publish after Close = %v", err)
	}
	if len(fc.sessions) != 1 {
		t.Errorf("%d connections, want 1", len(fc.sessions))
	}
}

func TestCloseWhileConnecting(t *testing.T) {
	t.Parallel()
	connecting, release := make(chan struct{}), make(chan struct{})
	made := &fakeSession{}
	p := newPublisher(natsTarget(), func(context.Context) (session, error) {
		close(connecting)
		<-release
		return made, nil
	})
	result := make(chan error, 1)
	go func() { result <- p.Publish(context.Background(), event("1")) }()
	<-connecting
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; !errors.Is(err, ErrClosed) {
		t.Errorf("Publish = %v; want closed", err)
	}
	if !made.Closed() {
		t.Error("the connection made after Close was left open")
	}
}

// fakeSecrets opens what it was given, for the target's secret only.
type fakeSecrets struct {
	plain map[string]string
	err   error
	calls int
}

func (s *fakeSecrets) Open(_ context.Context, sealed iam.SealedSecret, aad []byte) ([]byte, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	if !bytes.Equal(aad, delivery.SecretAAD("events")) {
		return nil, fmt.Errorf("%w: aad %q", domain.ErrIntegrity, aad)
	}
	plain, ok := s.plain[string(sealed.Ciphertext)]
	if !ok {
		return nil, domain.ErrIntegrity
	}
	return []byte(plain), nil
}

func noDial(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("dialled")
}

func TestNewRefuses(t *testing.T) {
	t.Parallel()
	change := func(f func(*delivery.Target)) delivery.Target {
		target := natsTarget()
		f(&target)
		return target
	}
	tests := map[string]struct {
		target  delivery.Target
		secrets sealed.Opener
		dial    func(context.Context, string, string) (net.Conn, error)
	}{
		"a webhook":                  {target: change(func(x *delivery.Target) { x.Kind = delivery.KindWebhook })},
		"no subject":                 {target: change(func(x *delivery.Target) { x.Subject = "" })},
		"an endpoint in the clear":   {target: change(func(x *delivery.Target) { x.Endpoint = "nats://secret-host.example.com:4222" })},
		"an endpoint with a user":    {target: change(func(x *delivery.Target) { x.Endpoint = "tls://user:pw@secret-host.example.com" })},
		"certificates that are none": {target: change(func(x *delivery.Target) { x.CAPEM = []byte("not a certificate") })},
		"a bearer token":             {target: change(func(x *delivery.Target) { x.Auth.Mode = delivery.AuthBearer })},
		"a token without a secret":   {target: change(func(x *delivery.Target) { x.Secret = iam.SealedSecret{} })},
		"a user without a password": {target: change(func(x *delivery.Target) {
			x.Auth = delivery.Auth{Mode: delivery.AuthBasic, ID: "u"}
			x.Secret = iam.SealedSecret{}
		})},
		"no dial function":             {target: natsTarget(), dial: nil, secrets: &fakeSecrets{}},
		"nothing to open secrets with": {target: natsTarget(), dial: noDial},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			secrets, dial := tc.secrets, tc.dial
			if secrets == nil && dial == nil {
				secrets, dial = &fakeSecrets{}, noDial
			}
			p, err := New(tc.target, secrets, dial)
			if err == nil || p != nil {
				t.Fatalf("New = %v, %v; want a refusal", p, err)
			}
			if !errors.Is(err, domain.ErrInvalidArgument) || delivery.ClassOf(err) != delivery.ClassInternal {
				t.Errorf("New = %v; want an invalid argument of class internal", err)
			}
			if chain := chainText(err); strings.Contains(chain, "secret-host") || strings.Contains(chain, "pw@") {
				t.Errorf("the error repeats the endpoint: %q", chain)
			}
		})
	}
}

func TestNewTakesEveryWayOfProvingItself(t *testing.T) {
	t.Parallel()
	for _, auth := range []delivery.Auth{{}, {Mode: delivery.AuthNone}, {Mode: delivery.AuthToken}, {Mode: delivery.AuthBasic, ID: "u"}} {
		target := natsTarget()
		target.Auth = auth
		if auth.Mode == "" || auth.Mode == delivery.AuthNone {
			target.Secret = iam.SealedSecret{}
		}
		if _, err := New(target, &fakeSecrets{}, noDial); err != nil {
			t.Errorf("New with %q = %v", auth.Mode, err)
		}
	}
}

// applied returns the options a connection would be made with.
func applied(t *testing.T, opts []natsgo.Option) natsgo.Options {
	t.Helper()
	o := natsgo.GetDefaultOptions()
	for _, opt := range opts {
		if err := opt(&o); err != nil {
			t.Fatal(err)
		}
	}
	return o
}

func TestConnectionOptions(t *testing.T) {
	t.Parallel()
	target := natsTarget()
	target.Endpoint = "tls://nats.example.com"
	c, err := newConnector(target, &fakeSecrets{}, noDial)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	o := applied(t, c.options(ctx, []byte(markedValue)))
	switch {
	case !o.Secure || o.TLSConfig == nil || o.TLSConfig.MinVersion < tls.VersionTLS12 || o.TLSConfig.InsecureSkipVerify:
		t.Error("TLS is not required, or not verified")
	case o.TLSConfig.ServerName != "nats.example.com":
		t.Errorf("the server is verified as %q", o.TLSConfig.ServerName)
	case o.AllowReconnect || !o.SkipHostLookup || !o.IgnoreDiscoveredServers:
		t.Error("the client reconnects, looks names up or uses servers it is told of by itself")
	case o.CustomDialer == nil:
		t.Error("the egress dialer is not used")
	case o.Token != "" || o.TokenHandler == nil || o.TokenHandler() != markedValue || o.UserInfo != nil:
		t.Error("the token is not what proves the client, or is kept in the options")
	case o.Timeout <= 0 || o.Timeout > time.Second:
		t.Errorf("a connect timeout of %s beyond the context's", o.Timeout)
	case c.url != "tls://nats.example.com:4222":
		t.Errorf("connects to %q", c.url)
	}

	target.Auth = delivery.Auth{Mode: delivery.AuthBasic, ID: markedUser}
	c, err = newConnector(target, &fakeSecrets{}, noDial)
	if err != nil {
		t.Fatal(err)
	}
	o = applied(t, c.options(context.Background(), []byte(markedValue)))
	if o.UserInfo == nil || o.User != "" || o.Password != "" || o.TokenHandler != nil {
		t.Fatal("the user and the password are not given through a handler")
	}
	if user, password := o.UserInfo(); user != markedUser || password != markedValue {
		t.Error("the user and the password are not what proves the client")
	}
	if o.Timeout != ConnectTimeout {
		t.Errorf("a connect timeout of %s without a deadline", o.Timeout)
	}

	target.Auth, target.Secret = delivery.Auth{}, iam.SealedSecret{}
	c, err = newConnector(target, &fakeSecrets{}, noDial)
	if err != nil {
		t.Fatal(err)
	}
	if o = applied(t, c.options(context.Background(), nil)); o.User != "" || o.Password != "" || o.Token != "" || o.TokenHandler != nil || o.UserInfo != nil {
		t.Error("a target that proves nothing gives a credential")
	}
}

func TestConnectFailsWhenTheSecretDoesNotOpen(t *testing.T) {
	t.Parallel()
	dialled := false
	dial := func(context.Context, string, string) (net.Conn, error) {
		dialled = true
		return nil, errors.New("dialled")
	}
	secrets := &fakeSecrets{err: fmt.Errorf("%w: unwrap", domain.ErrKeyUnavailable)}
	p, err := New(natsTarget(), secrets, dial)
	if err != nil {
		t.Fatal(err)
	}
	err = p.Publish(context.Background(), event("1"))
	if delivery.ClassOf(err) != delivery.ClassInternal || !errors.Is(err, domain.ErrKeyUnavailable) {
		t.Errorf("Publish = %v; want internal", err)
	}
	if dialled {
		t.Error("dialled without a credential")
	}
}

// chainText returns the text of err and of everything it wraps.
func chainText(err error) string {
	var b strings.Builder
	for _, e := range unwrapAll(err) {
		b.WriteString(e.Error())
		b.WriteByte('\n')
	}
	return b.String()
}

func unwrapAll(err error) []error {
	if err == nil {
		return nil
	}
	out := []error{err}
	if next := errors.Unwrap(err); next != nil {
		return append(out, unwrapAll(next)...)
	}
	// An error that wraps several has no single one to unwrap; As finds
	// err itself when it is one.
	var several interface{ Unwrap() []error }
	if errors.As(err, &several) {
		for _, e := range several.Unwrap() {
			out = append(out, unwrapAll(e)...)
		}
	}
	return out
}
