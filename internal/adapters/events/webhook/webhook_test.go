package webhook_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/egress"
	"github.com/albievan/cactusfs/internal/adapters/events/webhook"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// Marked values: none of them may appear in an error, a printed form or a
// log line.
const (
	markHidden  = "MARK-hidden-b6f1"
	markSigning = "MARK-signing-29ac"
	markPath    = "/hooks/MARK-path-7d40/in"
	markBody    = "MARK-body-5e2c"
)

const (
	testVersion = "1.2.3-test"
	// testHost is the name of the receiver: httptest's certificate is
	// made for it, and the test's resolver gives loopback for it.
	testHost = "example.com"
)

// testTime is the time of every attempt of a test.
var testTime = time.Unix(1_800_000_000, 0)

// names is a resolver of a test.
type names map[string][]netip.Addr

// LookupNetIP implements egress.Resolver.
func (n names) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if addrs, ok := n[host]; ok {
		return addrs, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// received is what a receiver was sent.
type received struct {
	method string
	path   string
	header http.Header
	body   []byte
}

// receiver is an httptest TLS server that records what it is sent.
type receiver struct {
	server *httptest.Server
	// arrived is sent a value for each request, once it is recorded.
	arrived chan struct{}
	mu      sync.Mutex
	got     []received
}

// serve starts a TLS receiver that records each request, says that it
// arrived, and then hands it to handler.
func serve(t *testing.T, handler http.HandlerFunc) *receiver {
	t.Helper()
	r := &receiver{arrived: make(chan struct{}, 64)}
	r.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.got = append(r.got, received{method: req.Method, path: req.URL.Path, header: req.Header.Clone(), body: body})
		r.mu.Unlock()
		select {
		case r.arrived <- struct{}{}:
		default:
		}
		handler(w, req)
	}))
	// What a test makes fail on purpose - a hijacked connection, a client
	// that gave up - the server would log.
	r.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	r.server.StartTLS()
	t.Cleanup(r.server.Close)
	return r
}

// arrival waits until a request has arrived and is recorded. It fails the
// test when none arrives within a time far beyond what any should take;
// that bound is never what a passing test waits for.
func (r *receiver) arrival(t *testing.T) {
	t.Helper()
	select {
	case <-r.arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("no request arrived")
	}
}

// requests returns what the receiver was sent so far.
func (r *receiver) requests() []received {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]received(nil), r.got...)
}

// endpoint returns the origin a target names the receiver by.
func (r *receiver) endpoint(t *testing.T) string {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(r.server.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	return "https://" + testHost + ":" + port
}

// client returns an egress client that trusts the receiver's certificate,
// resolves testHost to loopback and admits loopback; timeout is its
// response timeout, zero for the default.
func (r *receiver) client(t *testing.T, timeout time.Duration) *egress.Client {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(r.server.Certificate())
	client, _, err := egress.NewClient(egress.Policy{
		Allow:           []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
		RootCAs:         pool,
		ResponseTimeout: timeout,
		Resolver:        names{testHost: {netip.MustParseAddr("127.0.0.1")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

// seal seals value through kms as the target use case does.
func seal(t *testing.T, kms ports.KMS, value string, aad []byte) iam.SealedSecret {
	t.Helper()
	key, err := kms.GenerateDataKey(t.Context(), aad)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key.Plaintext)
	sealed, err := iam.Seal(key.Plaintext, rand.Reader, []byte(value), aad)
	if err != nil {
		t.Fatal(err)
	}
	sealed.KMSKeyID, sealed.WrappedKey = kms.KeyID(), key.Wrapped
	return sealed
}

func newKMS(t *testing.T) *porttest.KMS {
	t.Helper()
	kms, err := porttest.NewKMS("test-key")
	if err != nil {
		t.Fatal(err)
	}
	return kms
}

// target returns a webhook target of the receiver with a path, a bearer
// secret and a signing key, each the marked value, sealed through kms.
func target(t *testing.T, kms ports.KMS, endpoint string) delivery.Target {
	t.Helper()
	const name = "hook"
	return delivery.Target{
		Name: name, Kind: delivery.KindWebhook, Endpoint: endpoint,
		SealedPath: seal(t, kms, markPath, delivery.PathAAD(name)),
		Buckets:    []string{"photos"},
		Auth:       delivery.Auth{Mode: delivery.AuthBearer},
		Secret:     seal(t, kms, markHidden, delivery.SecretAAD(name)),
		SignKey:    seal(t, kms, markSigning, delivery.SignKeyAAD(name)),
	}
}

func newPublisher(t *testing.T, tgt delivery.Target, kms ports.KMS, client *egress.Client) *webhook.Publisher {
	t.Helper()
	p, err := webhook.New(tgt, kms, client, webhook.Options{Version: testVersion, Clock: porttest.NewClock(testTime)})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// record is a body of a notification that carries the marked body.
type record struct {
	Records []map[string]any `json:"Records"`
}

func message() ports.Event {
	return ports.Event{
		ID: "000000000000000004d2", Type: ports.MessageNotificationObject,
		Data: record{Records: []map[string]any{{"eventName": "ObjectCreated:Put", "s3": map[string]any{"object": map[string]any{"key": markBody}}}}},
	}
}

// leaks fails the test when text holds any marked value.
func leaks(t *testing.T, what, text string) {
	t.Helper()
	for _, mark := range []string{markHidden, markSigning, markPath, "MARK-path-7d40", markBody} {
		if strings.Contains(text, mark) {
			t.Errorf("%s holds a marked value %q: %s", what, mark, text)
		}
	}
}

// errorLeaks fails the test when any link of err's chain - every error
// errors.Unwrap reaches, and every one of a join - holds a marked value,
// printed with %v or %+v. The text of a delivery.Error is its class alone;
// what it wraps is what a log of the node that leads would print.
func errorLeaks(t *testing.T, err error) {
	t.Helper()
	links := 0
	for queue := []error{err}; len(queue) > 0; queue = queue[1:] {
		link := queue[0]
		if link == nil {
			continue
		}
		links++
		leaks(t, fmt.Sprintf("link %d (%T) of the error", links, link), fmt.Sprintf("%v", link))
		leaks(t, fmt.Sprintf("link %d (%T) of the error with %%+v", links, link), fmt.Sprintf("%+v", link))
		// The links this one wraps itself, as errors.Unwrap finds them
		// and as a join holds them: errors.As would search the whole
		// chain, and this walks it one link at a time.
		if next := errors.Unwrap(link); next != nil {
			queue = append(queue, next)
		}
		if joined, ok := any(link).(interface{ Unwrap() []error }); ok {
			queue = append(queue, joined.Unwrap()...)
		}
	}
	if err != nil && links < 2 {
		t.Errorf("the error %v wraps nothing: the scan saw only its class", err)
	}
}

// TestPublishSigned: a message is POSTed to the endpoint and its sealed path
// with every header of ADR-0025; the signature verifies with the signing key
// over the timestamp and the body, and does not for a changed body or a
// changed timestamp.
func TestPublishSigned(t *testing.T) {
	t.Parallel()
	kms := newKMS(t)
	rcv := serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	p := newPublisher(t, target(t, kms, rcv.endpoint(t)), kms, rcv.client(t, 0))

	raw := json.RawMessage(`{"Records":[{"eventName":"ObjectRemoved:Delete"}]}`)
	audit := ports.Event{ID: "audit-42", Type: ports.MessageAuditExported, Data: raw}
	for _, e := range []ports.Event{message(), audit} {
		if err := p.Publish(t.Context(), e); err != nil {
			t.Fatalf("Publish(%s): %v", e.Type, err)
		}
	}
	got := rcv.requests()
	if len(got) != 2 {
		t.Fatalf("the receiver got %d requests, want 2", len(got))
	}
	wantBody, err := json.Marshal(message().Data)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct {
		id, kind string
		body     []byte
	}{
		{id: "000000000000000004d2", kind: ports.MessageNotificationObject, body: wantBody},
		{id: "audit-42", kind: ports.MessageAuditExported, body: raw},
	} {
		r := got[i]
		if r.method != http.MethodPost || r.path != markPath {
			t.Errorf("request %d: %s %s, want POST to the sealed path", i, r.method, r.path)
		}
		if !bytes.Equal(r.body, want.body) {
			t.Errorf("request %d: body %s, want %s", i, r.body, want.body)
		}
		headers := map[string]string{
			"Content-Type":          "application/json",
			"User-Agent":            "cactusfs/" + testVersion,
			webhook.HeaderEventID:   want.id,
			webhook.HeaderEventType: want.kind,
			webhook.HeaderTimestamp: strconv.FormatInt(testTime.Unix(), 10),
			"Authorization":         "Bearer " + markHidden,
			webhook.HeaderSignature: webhook.Sign([]byte(markSigning), strconv.FormatInt(testTime.Unix(), 10), want.body),
		}
		for name, value := range headers {
			if got := r.header.Get(name); got != value {
				t.Errorf("request %d: %s is %q, want %q", i, name, got, value)
			}
		}
		timestamp, signature := r.header.Get(webhook.HeaderTimestamp), r.header.Get(webhook.HeaderSignature)
		if !strings.HasPrefix(signature, webhook.SignaturePrefix) || len(signature) != len(webhook.SignaturePrefix)+64 {
			t.Errorf("request %d: signature %q is not v1= and 64 hexadecimal digits", i, signature)
		}
		if !webhook.Verify([]byte(markSigning), timestamp, r.body, signature) {
			t.Errorf("request %d: the signature does not verify with the signing key", i)
		}
		changed := append(bytes.Clone(r.body), ' ')
		if webhook.Verify([]byte(markSigning), timestamp, changed, signature) {
			t.Errorf("request %d: the signature verifies for a changed body", i)
		}
		if webhook.Verify([]byte(markSigning), timestamp+"1", r.body, signature) {
			t.Errorf("request %d: the signature verifies for a changed timestamp", i)
		}
		if webhook.Verify([]byte("another key"), timestamp, r.body, signature) {
			t.Errorf("request %d: the signature verifies with another key", i)
		}
	}
}

// TestPublishWithoutSecretOrPath: a target without a secret sends no
// Authorization, and one without a path is posted to its origin.
func TestPublishWithoutSecretOrPath(t *testing.T) {
	t.Parallel()
	kms := newKMS(t)
	rcv := serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	tgt := target(t, kms, rcv.endpoint(t))
	tgt.Auth, tgt.Secret, tgt.SealedPath = delivery.Auth{Mode: delivery.AuthNone}, iam.SealedSecret{}, iam.SealedSecret{}
	p := newPublisher(t, tgt, kms, rcv.client(t, 0))
	if err := p.Publish(t.Context(), message()); err != nil {
		t.Fatal(err)
	}
	got := rcv.requests()
	if len(got) != 1 {
		t.Fatalf("the receiver got %d requests, want 1", len(got))
	}
	if _, ok := got[0].header["Authorization"]; ok {
		t.Error("a target without a secret sent Authorization")
	}
	if got[0].path != "/" {
		t.Errorf("path %q, want /", got[0].path)
	}
	if got[0].header.Get(webhook.HeaderSignature) == "" {
		t.Error("the request is not signed")
	}
}

// failure is a test of a failure: what the receiver does, and what the
// publisher must say of it.
type failure struct {
	name    string
	handler http.HandlerFunc
	// timeout is the response timeout of the client; zero for the
	// default.
	timeout time.Duration
	// deadline, when not zero, is the deadline of the attempt.
	deadline time.Duration
	// cancel says that the test cancels the attempt once the receiver
	// has the request.
	cancel bool
	class  delivery.Class
	// is, when not nil, is what the error wraps.
	is error
}

// slow is a receiver that answers nothing until the client gives up.
func slow(_ http.ResponseWriter, r *http.Request) {
	<-r.Context().Done()
}

// hangUp closes the connection without an answer.
func hangUp(w http.ResponseWriter, _ *http.Request) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		// An answer no test expects, so that the test fails.
		w.WriteHeader(http.StatusTeapot)
		return
	}
	conn, _, err := hijacker.Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if code >= 300 && code <= 399 {
			w.Header().Set("Location", "https://"+testHost+"/elsewhere")
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, "answer of the receiver")
	}
}

func failures() []failure {
	tests := []failure{
		{name: "a closed connection", handler: hangUp, class: delivery.ClassServer, is: webhook.ErrNoAnswer},
		{name: "a slow answer", handler: slow, timeout: 500 * time.Millisecond, class: delivery.ClassTimeout},
		{name: "an answer after the deadline", handler: slow, deadline: 2 * time.Second, class: delivery.ClassTimeout},
		{name: "an attempt cancelled while it waits", handler: slow, cancel: true, class: delivery.ClassInternal},
	}
	for code, class := range map[int]delivery.Class{
		400: delivery.ClassRejected, 404: delivery.ClassRejected, 413: delivery.ClassRejected, 422: delivery.ClassRejected,
		401: delivery.ClassAuth, 403: delivery.ClassAuth,
		408: delivery.ClassServer, 429: delivery.ClassServer, 500: delivery.ClassServer, 502: delivery.ClassServer, 503: delivery.ClassServer,
	} {
		tests = append(tests, failure{name: "status " + strconv.Itoa(code), handler: status(code), class: class, is: webhook.ErrStatus})
	}
	// A redirect is refused by the egress client, which never follows one.
	for _, code := range []int{301, 302, 307, 308} {
		tests = append(tests, failure{name: "status " + strconv.Itoa(code), handler: status(code), class: delivery.ClassRedirect, is: egress.ErrRedirect})
	}
	return tests
}

// TestPublishClasses: 2xx is delivered; every other answer, and no answer,
// is a failure of the class ADR-0025 gives it; only rejected passes the
// item. No error holds a marked value.
func TestPublishClasses(t *testing.T) {
	t.Parallel()
	for _, code := range []int{200, 201, 202, 204} {
		t.Run("status "+strconv.Itoa(code), func(t *testing.T) {
			t.Parallel()
			kms := newKMS(t)
			rcv := serve(t, status(code))
			p := newPublisher(t, target(t, kms, rcv.endpoint(t)), kms, rcv.client(t, 0))
			if err := p.Publish(t.Context(), message()); err != nil {
				t.Fatalf("a %d is delivered; got %v", code, err)
			}
		})
	}
	for _, tt := range failures() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			kms := newKMS(t)
			rcv := serve(t, tt.handler)
			p := newPublisher(t, target(t, kms, rcv.endpoint(t)), kms, rcv.client(t, tt.timeout))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.deadline > 0 {
				ctx, cancel = context.WithTimeout(ctx, tt.deadline)
				defer cancel()
			}
			done := make(chan error, 1)
			go func() { done <- p.Publish(ctx, message()) }()
			// The request is counted once the receiver has it, whatever
			// Publish has seen of it by then.
			rcv.arrival(t)
			if tt.cancel {
				cancel()
			}
			err := <-done
			if err == nil {
				t.Fatal("Publish succeeded")
			}
			if got := delivery.ClassOf(err); got != tt.class {
				t.Errorf("class %q, want %q (%v)", got, tt.class, err)
			}
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Errorf("error %v, want one that wraps %v", err, tt.is)
			}
			if passes := delivery.ClassOf(err).Passes(); passes != (tt.class == delivery.ClassRejected) {
				t.Errorf("passes = %v for class %q", passes, tt.class)
			}
			if n := len(rcv.requests()); n != 1 {
				t.Errorf("the receiver got %d requests, want 1", n)
			}
			if err.Error() != "delivery failed: "+string(tt.class) {
				t.Errorf("the text of the error is %q, want its class alone", err)
			}
			errorLeaks(t, err)
		})
	}
}

// TestPublishNothingListens: an address where nothing listens is a failure
// to connect - nothing was sent, so it is not ErrNoAnswer.
func TestPublishNothingListens(t *testing.T) {
	t.Parallel()
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	client, _, err := egress.NewClient(egress.Policy{
		Allow:    []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
		Resolver: names{testHost: {netip.MustParseAddr("127.0.0.1")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	kms := newKMS(t)
	p := newPublisher(t, target(t, kms, "https://"+testHost+":"+port), kms, client)
	err = p.Publish(t.Context(), message())
	if got := delivery.ClassOf(err); got != delivery.ClassConnect {
		t.Errorf("class %q, want connect (%v)", got, err)
	}
	if errors.Is(err, webhook.ErrNoAnswer) {
		t.Errorf("a connection that was never made is ErrNoAnswer: %v", err)
	}
	errorLeaks(t, err)
}

// TestPublishRefusedAddress: an address the policy does not admit is never
// connected to, and is egress_refused.
func TestPublishRefusedAddress(t *testing.T) {
	t.Parallel()
	kms := newKMS(t)
	rcv := serve(t, status(http.StatusOK))
	pool := x509.NewCertPool()
	pool.AddCert(rcv.server.Certificate())
	client, _, err := egress.NewClient(egress.Policy{
		RootCAs: pool, Resolver: names{testHost: {netip.MustParseAddr("127.0.0.1")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	p := newPublisher(t, target(t, kms, rcv.endpoint(t)), kms, client)
	err = p.Publish(t.Context(), message())
	if got := delivery.ClassOf(err); got != delivery.ClassEgressRefused {
		t.Errorf("class %q, want egress_refused (%v)", got, err)
	}
	if len(rcv.requests()) != 0 {
		t.Error("a refused address was sent to")
	}
	errorLeaks(t, err)
}

// TestPublishRefusesMessage: what is no message for a target is never sent,
// and fails as internal: it is this side's. An attempt whose deadline has
// passed is a timeout, one that was cancelled internal; neither sends.
func TestPublishRefusesMessage(t *testing.T) {
	t.Parallel()
	kms := newKMS(t)
	rcv := serve(t, status(http.StatusOK))
	p := newPublisher(t, target(t, kms, rcv.endpoint(t)), kms, rcv.client(t, 0))
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, cancelExpired := context.WithDeadline(t.Context(), time.Unix(1, 0))
	defer cancelExpired()

	tests := []struct {
		name  string
		ctx   context.Context
		event func(e *ports.Event)
		class delivery.Class
	}{
		{name: "an event of the in-process bus", event: func(e *ports.Event) { e.Type = ports.EventObjectCreated }},
		{name: "no type", event: func(e *ports.Event) { e.Type = "" }},
		{name: "no ID", event: func(e *ports.Event) { e.ID = "" }},
		{name: "an ID with a line break", event: func(e *ports.Event) { e.ID = "1\r\nX-Injected: " + markBody }},
		{name: "an ID too long", event: func(e *ports.Event) { e.ID = strings.Repeat("a", 129) }},
		{name: "bytes that are no JSON", event: func(e *ports.Event) { e.Data = []byte("{" + markBody) }},
		{name: "raw JSON that is none", event: func(e *ports.Event) { e.Data = json.RawMessage(markBody) }},
		{name: "data that does not marshal", event: func(e *ports.Event) { e.Data = map[string]any{markBody: func() {}} }},
		{name: "a context that was cancelled", ctx: cancelled, event: func(*ports.Event) {}},
		{name: "a deadline that passed", ctx: expired, event: func(*ports.Event) {}, class: delivery.ClassTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.ctx
			if ctx == nil {
				ctx = t.Context()
			}
			want := tt.class
			if want == delivery.ClassNone {
				want = delivery.ClassInternal
			}
			e := message()
			tt.event(&e)
			err := p.Publish(ctx, e)
			if got := delivery.ClassOf(err); got != want {
				t.Errorf("class %q, want %q (%v)", got, want, err)
			}
			errorLeaks(t, err)
		})
	}
	if n := len(rcv.requests()); n != 0 {
		t.Errorf("the receiver got %d requests, want none", n)
	}
}

// TestPublishCredentials: a sealed field that does not open - another KMS
// key, a KMS that fails, a field moved from another target - and a secret
// no header can carry fail as internal, and nothing is sent.
func TestPublishCredentials(t *testing.T) {
	t.Parallel()
	rcv := serve(t, status(http.StatusOK))
	client := rcv.client(t, 0)
	// The subtests run in parallel after this function returns; the
	// receiver is asked when they are all done.
	t.Cleanup(func() {
		if n := len(rcv.requests()); n != 0 {
			t.Errorf("the receiver got %d requests, want none", n)
		}
	})
	tests := []struct {
		name   string
		change func(t *testing.T, kms *porttest.KMS, tgt *delivery.Target)
		is     error
	}{
		{name: "sealed under another KMS key", change: func(_ *testing.T, _ *porttest.KMS, tgt *delivery.Target) {
			tgt.SignKey.KMSKeyID = "another-key"
		}, is: domain.ErrKeyUnavailable},
		{name: "a KMS that fails", change: func(_ *testing.T, kms *porttest.KMS, _ *delivery.Target) {
			kms.FailUnwrap = fmt.Errorf("%w: vault sealed", domain.ErrKeyUnavailable)
		}, is: domain.ErrKeyUnavailable},
		{name: "a secret moved from another field", change: func(_ *testing.T, _ *porttest.KMS, tgt *delivery.Target) {
			tgt.Secret = tgt.SignKey
		}, is: domain.ErrKeyUnavailable},
		{name: "a path that is sealed for another target", change: func(t *testing.T, kms *porttest.KMS, tgt *delivery.Target) {
			tgt.SealedPath = seal(t, kms, markPath, delivery.PathAAD("other"))
		}, is: domain.ErrKeyUnavailable},
		{name: "a ciphertext that was changed", change: func(_ *testing.T, _ *porttest.KMS, tgt *delivery.Target) {
			tgt.Secret.Ciphertext[0] ^= 1
		}, is: domain.ErrIntegrity},
		{name: "a secret with a line break", change: func(t *testing.T, kms *porttest.KMS, tgt *delivery.Target) {
			tgt.Secret = seal(t, kms, markHidden+"\r\nX-Injected: 1", delivery.SecretAAD(tgt.Name))
		}, is: webhook.ErrCredential},
		{name: "a path with a query", change: func(t *testing.T, kms *porttest.KMS, tgt *delivery.Target) {
			tgt.SealedPath = seal(t, kms, markPath+"?token="+markHidden, delivery.PathAAD(tgt.Name))
		}, is: webhook.ErrCredential},
		{name: "a path that names another host", change: func(t *testing.T, kms *porttest.KMS, tgt *delivery.Target) {
			tgt.SealedPath = seal(t, kms, "@evil.example"+markPath, delivery.PathAAD(tgt.Name))
		}, is: webhook.ErrCredential},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			kms := newKMS(t)
			tgt := target(t, kms, rcv.endpoint(t))
			tt.change(t, kms, &tgt)
			p, err := webhook.New(tgt, kms, client, webhook.Options{Version: testVersion})
			if err != nil {
				t.Fatal(err)
			}
			err = p.Publish(t.Context(), message())
			if got := delivery.ClassOf(err); got != delivery.ClassInternal {
				t.Errorf("class %q, want internal (%v)", got, err)
			}
			if !errors.Is(err, tt.is) {
				t.Errorf("error %v, want one that wraps %v", err, tt.is)
			}
			errorLeaks(t, err)
		})
	}
}

// TestNew: what New refuses, without repeating more of the target than its
// name.
func TestNew(t *testing.T) {
	t.Parallel()
	kms := newKMS(t)
	client, _, err := egress.NewClient(egress.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	valid := target(t, kms, "https://hooks.example.com")
	tests := []struct {
		name    string
		change  func(tgt *delivery.Target, kms *ports.KMS, client **egress.Client, opts *webhook.Options)
		invalid bool
	}{
		{name: "a valid target", change: func(*delivery.Target, *ports.KMS, **egress.Client, *webhook.Options) {}},
		{name: "a NATS target", invalid: true, change: func(tgt *delivery.Target, _ *ports.KMS, _ **egress.Client, _ *webhook.Options) {
			tgt.Kind = delivery.KindNATS
		}},
		{name: "no signing key", invalid: true, change: func(tgt *delivery.Target, _ *ports.KMS, _ **egress.Client, _ *webhook.Options) {
			tgt.SignKey = iam.SealedSecret{}
		}},
		{name: "an endpoint with its path", invalid: true, change: func(tgt *delivery.Target, _ *ports.KMS, _ **egress.Client, _ *webhook.Options) {
			tgt.Endpoint += markPath
		}},
		{name: "an endpoint of http", invalid: true, change: func(tgt *delivery.Target, _ *ports.KMS, _ **egress.Client, _ *webhook.Options) {
			tgt.Endpoint = "http://hooks.example.com"
		}},
		{name: "no KMS", invalid: true, change: func(_ *delivery.Target, kms *ports.KMS, _ **egress.Client, _ *webhook.Options) {
			*kms = nil
		}},
		{name: "no client", invalid: true, change: func(_ *delivery.Target, _ *ports.KMS, client **egress.Client, _ *webhook.Options) {
			*client = nil
		}},
		{name: "no version", invalid: true, change: func(_ *delivery.Target, _ *ports.KMS, _ **egress.Client, opts *webhook.Options) {
			opts.Version = ""
		}},
		{name: "a version with a space", invalid: true, change: func(_ *delivery.Target, _ *ports.KMS, _ **egress.Client, opts *webhook.Options) {
			opts.Version = "1.0 (evil)"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tgt, secrets, c, opts := valid.Clone(), ports.KMS(kms), client, webhook.Options{Version: testVersion}
			tt.change(&tgt, &secrets, &c, &opts)
			p, err := webhook.New(tgt, secrets, c, opts)
			if !tt.invalid {
				if err != nil || p == nil {
					t.Fatalf("New refused a valid target: %v", err)
				}
				return
			}
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("error %v, want ErrInvalidArgument", err)
			}
			errorLeaks(t, err)
			if strings.Contains(err.Error(), "hooks.example.com") {
				t.Errorf("the error names the endpoint: %v", err)
			}
		})
	}
}

// TestPrintedForms: every printed form of a publisher is its target's name
// and kind - no endpoint, nothing sealed.
func TestPrintedForms(t *testing.T) {
	t.Parallel()
	kms := newKMS(t)
	client, _, err := egress.NewClient(egress.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	p := newPublisher(t, target(t, kms, "https://hooks.example.com"), kms, client)
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("publisher", "publisher", p)
	for _, text := range []string{
		fmt.Sprint(p), fmt.Sprintf("%+v", p), fmt.Sprintf("%#v", p), fmt.Sprintf("%q", p), fmt.Sprintf("%x", p),
		p.String(), logged.String(),
	} {
		if !strings.Contains(text, "hook") || strings.Contains(text, "example.com") || strings.Contains(text, "Ciphertext") {
			t.Errorf("printed form %q is more or less than the target's name and kind", text)
		}
		leaks(t, "a printed form", text)
	}
}

// TestSignVerify: properties of the signature over arbitrary inputs: it
// verifies for what was signed and for nothing that differs in one byte.
func TestSignVerify(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key, timestamp, body string
		want                 string
	}{
		// A fixed vector, so that the scheme cannot change unnoticed:
		// HMAC-SHA256 of "1800000000.{}" under "key".
		{key: "key", timestamp: "1800000000", body: "{}", want: "v1=7321475872b03ff21bdc2dfd86ea2d99ec29c479fbe4a77aa6acf4509505493f"},
		{key: "", timestamp: "", body: ""},
		{key: markSigning, timestamp: "1", body: `{"Records":[]}`},
	}
	for _, tt := range tests {
		signature := webhook.Sign([]byte(tt.key), tt.timestamp, []byte(tt.body))
		if tt.want != "" && signature != tt.want {
			t.Errorf("Sign(%q, %q, %q) = %q, want %q", tt.key, tt.timestamp, tt.body, signature, tt.want)
		}
		if !webhook.Verify([]byte(tt.key), tt.timestamp, []byte(tt.body), signature) {
			t.Errorf("Sign(%q, %q, %q) does not verify", tt.key, tt.timestamp, tt.body)
		}
		if webhook.Verify([]byte(tt.key), tt.timestamp, []byte(tt.body+"x"), signature) {
			t.Errorf("Sign(%q, %q, %q) verifies a longer body", tt.key, tt.timestamp, tt.body)
		}
		// The full stop separates: moving a byte across it changes the
		// signature.
		if tt.timestamp != "" && webhook.Verify([]byte(tt.key), tt.timestamp[:len(tt.timestamp)-1], []byte(tt.timestamp[len(tt.timestamp)-1:]+"."+tt.body), signature) {
			t.Errorf("Sign(%q, %q, %q) verifies with the timestamp's last digit moved into the body", tt.key, tt.timestamp, tt.body)
		}
	}
}

// FuzzSignVerify: a signature verifies for what was signed, and not for a
// body with one byte changed.
func FuzzSignVerify(f *testing.F) {
	f.Add([]byte("key"), "1800000000", []byte("{}"), 0)
	f.Add([]byte{}, "", []byte{}, 0)
	f.Fuzz(func(t *testing.T, key []byte, timestamp string, body []byte, at int) {
		signature := webhook.Sign(key, timestamp, body)
		if !webhook.Verify(key, timestamp, body, signature) {
			t.Fatal("a signature does not verify what it signed")
		}
		if len(body) == 0 {
			return
		}
		changed := bytes.Clone(body)
		i := at % len(changed)
		if i < 0 {
			i = -i
		}
		changed[i] ^= 0x01
		if webhook.Verify(key, timestamp, changed, signature) {
			t.Fatal("a signature verifies a changed body")
		}
	})
}
