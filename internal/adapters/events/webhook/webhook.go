// Package webhook tells a target of kind webhook of what it is sent: a bucket
// notification or a record of the audit trail, POSTed to the target's
// endpoint, signed with the target's signing key (ADR-0025, "Delivery").
//
// A Publisher is one target's. It holds the target as the store keeps it -
// its path, its secret and its signing key sealed through the KMS - and opens
// each of them for one attempt only: what is open is cleared when the
// attempt no longer needs it, so that no plaintext outlives a request.
// Requests go through the egress client of ADR-0023, which judges the
// address at every connect, never follows a redirect, and names in an error
// the origin of a request and never its path.
//
// Every failure is a delivery.Error with its class (delivery.ClassOf): the
// classes of a webhook's answers are delivery.WebhookClass's, and a
// connection that ends after the request was sent without an answer is
// delivery.ClassServer - an answer that is no status at all. The text of
// every error Publish returns is its class and nothing else; what it wraps
// may name the origin of the endpoint, and no link of it holds the path,
// the secret, the signing key or the body. The package logs nothing.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/egress"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// The headers a request carries (ADR-0025).
const (
	// HeaderEventID is the ID of the message, by which a receiver
	// deduplicates: the same every time the message is sent.
	HeaderEventID = "X-Cactusfs-Event-Id"
	// HeaderEventType is ports.MessageNotificationObject or
	// ports.MessageAuditExported.
	HeaderEventType = "X-Cactusfs-Event-Type"
	// HeaderTimestamp is when this attempt was made, in seconds since the
	// Unix epoch.
	HeaderTimestamp = "X-Cactusfs-Timestamp"
	// HeaderSignature is SignaturePrefix and the lower-case hexadecimal
	// of the HMAC-SHA256, under the signing key, of the timestamp, a full
	// stop and the body (Sign).
	HeaderSignature = "X-Cactusfs-Signature"
	// SignaturePrefix names the scheme of a signature.
	SignaturePrefix = "v1="
)

// The bounds of what is sent.
const (
	// maxEventID bounds the ID of a message: a sequencer is 20
	// characters, "audit-<sequence>" at most 26.
	maxEventID = 128
	// maxVersion bounds the version named in the User-Agent.
	maxVersion = 64
)

// Errors of this package; each is the cause of a delivery.Error.
var (
	// ErrStatus is an answer of the target whose status is not a 2xx.
	ErrStatus = errors.New("webhook: the target did not take the message")
	// ErrNoAnswer is a connection that ended after the request was sent
	// and before an answer arrived.
	ErrNoAnswer = errors.New("webhook: the connection ended without an answer")
	// ErrMessage is a message this adapter does not send: of another type
	// than the two a target is sent, without a valid ID, or with a body
	// that is no JSON.
	ErrMessage = errors.New("webhook: not a message a webhook is sent")
	// ErrCredential is a sealed field of the target that could not be
	// opened, or holds what a request cannot carry.
	ErrCredential = errors.New("webhook: a credential of the target could not be used")
)

// Options are what a Publisher needs besides its target, its secrets and its
// client.
type Options struct {
	// Version is the version of this build, named in the User-Agent as
	// cactusfs/<version>: letters, digits, '.', '-', '+' and '_', at most
	// 64 of them.
	Version string
	// Clock tells the time of an attempt; nil is the system's clock.
	Clock ports.Clock
}

// systemClock is the clock of the system.
type systemClock struct{}

// Now implements ports.Clock.
func (systemClock) Now() time.Time { return time.Now() }

// Publisher is a ports.EventPublisher that POSTs every message to one target
// of kind webhook. It is safe for concurrent use, though the delivery worker
// sends to a target one message at a time.
//
// Every printed form of a Publisher - with any verb of fmt, and in a log - is
// the name and the kind of its target.
type Publisher struct {
	target    delivery.Target
	secrets   ports.KMS
	client    *egress.Client
	clock     ports.Clock
	userAgent string
}

var _ ports.EventPublisher = (*Publisher)(nil)

// New returns the publisher of a target of kind webhook. secrets is the KMS
// the target's sealed fields were sealed through: they are opened with it at
// every attempt, and never kept open. client is the egress client made for
// the target - with its CA as the roots, and the setting egress-allow - and
// is the only way the publisher reaches the network.
//
// The target must be one delivery.Target.Validate takes, of kind webhook,
// with a signing key: nothing is sent unsigned. The error wraps
// domain.ErrInvalidArgument and repeats nothing of the target but its name.
// New connects to nothing.
func New(target delivery.Target, secrets ports.KMS, client *egress.Client, opts Options) (*Publisher, error) {
	switch {
	case target.Kind != delivery.KindWebhook:
		return nil, fmt.Errorf("webhook.New: %w: %s is not a webhook", domain.ErrInvalidArgument, target)
	case secrets == nil || client == nil:
		return nil, fmt.Errorf("webhook.New: %w: no KMS or no client", domain.ErrInvalidArgument)
	case !versionText(opts.Version):
		return nil, fmt.Errorf("webhook.New: %w: the version is not one a User-Agent can name", domain.ErrInvalidArgument)
	case !target.SignKeySet():
		return nil, fmt.Errorf("webhook.New: %w: %s has no signing key", domain.ErrInvalidArgument, target)
	}
	if err := target.Validate(); err != nil {
		return nil, fmt.Errorf("webhook.New: %w", err)
	}
	if _, path := delivery.SplitEndpoint(target.Endpoint); path != "" {
		// Validate refuses an endpoint with a path; this is said again
		// so that what is concatenated below is an origin and a path.
		return nil, fmt.Errorf("webhook.New: %w: %s has an endpoint that is no origin", domain.ErrInvalidArgument, target)
	}
	clock := opts.Clock
	if clock == nil {
		clock = systemClock{}
	}
	return &Publisher{
		target: target.Clone(), secrets: secrets, client: client, clock: clock,
		userAgent: "cactusfs/" + opts.Version,
	}, nil
}

// String returns the name and the kind of the publisher's target.
func (p *Publisher) String() string {
	return "webhook publisher of " + p.target.String()
}

// Format implements fmt.Formatter: no verb prints more than String does,
// whatever the sealed fields of the target hold.
func (p *Publisher) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(p.String()))
}

// LogValue implements slog.LogValuer: a publisher in a log is its target's
// name and kind.
func (p *Publisher) LogValue() slog.Value {
	return p.target.LogValue()
}

// Publish implements ports.EventPublisher: it POSTs the message to the
// target and returns when the target has answered, ctx has ended, or the
// egress client gave up. The message is e.Data as JSON - bytes of JSON
// (json.RawMessage or []byte) as they are, anything else marshalled - and
// e.Type must be ports.MessageNotificationObject or ports.MessageAuditExported.
//
// nil is a 2xx. An error is a delivery.Error of the class of the failure
// (delivery.ClassOf): the class of the status (delivery.WebhookClass), the
// egress client's class of a failure to connect, or ClassServer for a
// connection that ended without an answer after the request was sent. A
// deadline of ctx that passed is ClassTimeout in every phase; a message
// that is not one to send, a credential that does not open, and a ctx that
// was cancelled are ClassInternal, and nothing is sent.
func (p *Publisher) Publish(ctx context.Context, e ports.Event) error {
	if err := ctx.Err(); err != nil {
		return delivery.Failed(localClass(err), fmt.Errorf("webhook.Publish: %w", err))
	}
	body, err := messageBody(e)
	if err != nil {
		return delivery.Failed(delivery.ClassInternal, fmt.Errorf("webhook.Publish: %w", err))
	}
	// Whether the request went out: a connection that fails after it did
	// has failed to answer, which is the target's (ClassServer).
	var wrote atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		if info.Err == nil {
			wrote.Store(true)
		}
	}})
	req, err := p.request(ctx, e, body)
	if err != nil {
		return delivery.Failed(localClass(err), fmt.Errorf("webhook.Publish: %w", err))
	}

	resp, err := p.client.Do(req)
	if err != nil {
		if wrote.Load() && delivery.ClassOf(err) == delivery.ClassConnect {
			return delivery.Failed(delivery.ClassServer, fmt.Errorf("webhook.Publish: %w: %w", ErrNoAnswer, err))
		}
		// Wrapped again, so that the text of every error of Publish is
		// its class and nothing else - not even the origin that the
		// egress client's url.Error names.
		return delivery.Failed(delivery.ClassOf(err), fmt.Errorf("webhook.Publish: %w", err))
	}
	// Discard closes the body; this close, a second one, does nothing then
	// and is what closes it if Discard is ever left out.
	defer func() { _ = resp.Body.Close() }()
	code := resp.StatusCode
	// The answer's body is read and closed, so that the connection is
	// kept for the next message; what it says is not looked at. Discard
	// bounds what it reads, and the connection's progress deadline the
	// time that takes.
	_ = egress.Discard(resp)

	if class := delivery.WebhookClass(code); class != delivery.ClassNone {
		return delivery.Failed(class, fmt.Errorf("webhook.Publish: %w: status %d", ErrStatus, code))
	}
	return nil
}

// localClass returns the class of a failure before anything was sent: a
// deadline that passed is ClassTimeout - the attempt ran out of time, as it
// does while a target is slow - and anything else, a cancelled context
// among it, is this side's: ClassInternal.
func localClass(err error) delivery.Class {
	if errors.Is(err, context.DeadlineExceeded) {
		return delivery.ClassTimeout
	}
	return delivery.ClassInternal
}

// request returns the signed POST of body to the target, its secrets opened
// for as long as it takes to put them into the request and cleared after.
//
// What the request carries of the path and the secret - the URL and the
// Authorization header - are strings that cannot be cleared: they last as
// long as the request does, and nothing keeps the request.
func (p *Publisher) request(ctx context.Context, e ports.Event, body []byte) (*http.Request, error) {
	timestamp := strconv.FormatInt(p.clock.Now().Unix(), 10)
	signKey, err := p.open(ctx, p.target.SignKey, delivery.SignKeyAAD(p.target.Name))
	if err != nil {
		return nil, err
	}
	signature := Sign(signKey, timestamp, body)
	clear(signKey)

	endpoint := p.target.Endpoint
	if p.target.PathSet() {
		path, err := p.open(ctx, p.target.SealedPath, delivery.PathAAD(p.target.Name))
		if err != nil {
			return nil, err
		}
		endpoint += string(path)
		clear(path)
	}
	target, err := p.endpointURL(endpoint)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.target.Endpoint, bytes.NewReader(body))
	if err != nil {
		// The origin was validated by New; the cause, which repeats it,
		// is left out all the same.
		return nil, fmt.Errorf("%w: the request could not be made", ErrMessage)
	}
	req.URL = target
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", p.userAgent)
	req.Header.Set(HeaderEventID, e.ID)
	req.Header.Set(HeaderEventType, e.Type)
	req.Header.Set(HeaderTimestamp, timestamp)
	req.Header.Set(HeaderSignature, signature)

	if p.target.SecretSet() {
		secret, err := p.open(ctx, p.target.Secret, delivery.SecretAAD(p.target.Name))
		if err != nil {
			return nil, err
		}
		usable := headerText(secret)
		if usable {
			req.Header.Set("Authorization", "Bearer "+string(secret))
		}
		clear(secret)
		if !usable {
			return nil, fmt.Errorf("%w: the secret is not one a header can carry", ErrCredential)
		}
	}
	return req, nil
}

// endpointURL parses the endpoint the request is sent to and makes sure that
// it is the target's origin and a path: https, the host of the origin, no
// user information, query or fragment. Its error repeats nothing of it.
func (p *Publisher) endpointURL(endpoint string) (*url.URL, error) {
	origin, err := url.Parse(p.target.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("%w: the endpoint does not parse", ErrCredential)
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host != origin.Host || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, fmt.Errorf("%w: the endpoint and its path are not one URL of the target", ErrCredential)
	}
	return u, nil
}

// open opens a sealed field of the target, bound to it by aad. The caller
// clears what it returns.
func (p *Publisher) open(ctx context.Context, sealed iam.SealedSecret, aad []byte) ([]byte, error) {
	if sealed.KMSKeyID != p.secrets.KeyID() {
		return nil, fmt.Errorf("%w: %w: sealed under another KMS key", ErrCredential, domain.ErrKeyUnavailable)
	}
	key, err := p.secrets.UnwrapDataKey(ctx, sealed.WrappedKey, aad)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCredential, err)
	}
	defer clear(key)
	plain, err := sealed.Open(key, aad)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCredential, err)
	}
	return plain, nil
}

// Sign returns the value of HeaderSignature of a message: SignaturePrefix and
// the lower-case hexadecimal of HMAC-SHA256 under key of the timestamp, a
// full stop and the body. A receiver computes the same over the bytes it
// received and compares in constant time (Verify).
func Sign(key []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(timestamp))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return SignaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// Verify reports whether signature is the signature under key of timestamp
// and body, compared in constant time. It does not judge the age of the
// timestamp: a receiver refuses one more than five minutes from its clock
// (ADR-0025).
func Verify(key []byte, timestamp string, body []byte, signature string) bool {
	return hmac.Equal([]byte(Sign(key, timestamp, body)), []byte(signature))
}

// messageBody returns the body of a message: e.Data as JSON. Its error says
// what is wrong and repeats nothing of the message.
func messageBody(e ports.Event) ([]byte, error) {
	if e.Type != ports.MessageNotificationObject && e.Type != ports.MessageAuditExported {
		return nil, fmt.Errorf("%w: of a type that is not sent to a target", ErrMessage)
	}
	if e.ID == "" || len(e.ID) > maxEventID || !headerText([]byte(e.ID)) {
		return nil, fmt.Errorf("%w: an ID of 1 to %d printable characters is needed", ErrMessage, maxEventID)
	}
	var body []byte
	switch data := e.Data.(type) {
	case json.RawMessage:
		body = data
	case []byte:
		body = data
	default:
		marshalled, err := json.Marshal(data)
		if err != nil {
			// The cause can quote a value of the message.
			return nil, fmt.Errorf("%w: the message does not marshal to JSON", ErrMessage)
		}
		body = marshalled
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("%w: the body is no JSON", ErrMessage)
	}
	return body, nil
}

// headerText reports whether b is printable ASCII without spaces: what a
// header value carries as it is.
func headerText(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}

// versionText reports whether v can be named in a User-Agent: 1 to 64
// letters, digits, '.', '-', '+' and '_'.
func versionText(v string) bool {
	if v == "" || len(v) > maxVersion {
		return false
	}
	for i := range len(v) {
		c := v[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') &&
			c != '.' && c != '-' && c != '+' && c != '_' {
			return false
		}
	}
	return true
}
