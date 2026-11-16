package s3client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/albievan/cactusfs/internal/adapters/egress"
	"github.com/albievan/cactusfs/internal/adapters/sigv4"
	"github.com/albievan/cactusfs/internal/domain/delivery"
)

// The bounds the other deployment holds a signature to (the S3 API of this
// release): more signed headers, or a longer Authorization header, are
// refused there however often they are sent.
const (
	maxSignedHeaders       = 64
	maxAuthorizationHeader = 4 << 10
)

// call is one request to the other deployment.
type call struct {
	// op names the call in errors: "Check", "PutReplica", ...
	op     string
	method string
	// bucket and key name what the request is of; key is empty for a
	// request of the bucket. Neither is ever in an error.
	bucket, key string
	// query is the query string, its names and values unencoded, in the
	// order they are sent.
	query [][2]string
	// header holds the headers of the request; every one of them is
	// signed.
	header http.Header
	// body is the payload of a small request - a document - signed by its
	// SHA-256.
	body []byte
	// stream, when not nil, is the payload of an object or a part:
	// exactly size bytes of it are sent as signed chunks.
	stream io.Reader
	size   int64
}

// response is what the other deployment answered, its body read.
type response struct {
	status int
	header http.Header
	// body is the body of an answer the caller reads - a 2xx of a call
	// that has a document - and nil otherwise.
	body []byte
	// code is the S3 error code of a refusal, "" when it has none.
	code string
}

// do sends c and returns the answer: any status, with what was read of its
// body. An error is a failure to get an answer, of its class.
func (c *Client) do(ctx context.Context, rq call, document int64) (response, error) {
	if err := ctx.Err(); err != nil {
		return response{}, delivery.Failed(localClass(err), fmt.Errorf("s3client.%s: %w", rq.op, err))
	}
	// Whether the request went out: a connection that fails after it did
	// has failed to answer, which is the target's (ClassServer).
	var wrote atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		if info.Err == nil {
			wrote.Store(true)
		}
	}})
	req, body, err := c.request(ctx, rq)
	if err != nil {
		return response{}, err
	}
	resp, err := c.client.Do(req)
	if body != nil {
		// net/http may still be reading the body when Do returns - an
		// answer that came before the body was sent, a request cut off.
		// The body is ended here and the read in flight waited for, so
		// that nothing reads the caller's src once the call has returned.
		body.release()
		if failed := body.failure(); failed != nil {
			// The body could not be read to its size: the request was
			// cut off, whatever the transport says of it.
			if err == nil {
				_ = egress.Discard(resp)
			}
			return response{}, delivery.Failed(delivery.ClassInternal, fmt.Errorf("s3client.%s: %w", rq.op, failed))
		}
	}
	if err != nil {
		if wrote.Load() && delivery.ClassOf(err) == delivery.ClassConnect {
			return response{}, delivery.Failed(delivery.ClassServer, fmt.Errorf("s3client.%s: %w: %w", rq.op, ErrNoAnswer, err))
		}
		// Wrapped again, so that the text of every error is its class and
		// nothing else - not even the origin the egress client's url.Error
		// names.
		return response{}, delivery.Failed(delivery.ClassOf(err), fmt.Errorf("s3client.%s: %w", rq.op, err))
	}
	defer func() { _ = resp.Body.Close() }()
	out := response{status: resp.StatusCode, header: resp.Header}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299 && document > 0:
		out.body, err = readBounded(resp.Body, document)
		if err != nil {
			return response{}, delivery.Failed(c.readClass(err), fmt.Errorf("s3client.%s: %w", rq.op, err))
		}
	case resp.StatusCode >= 400 && rq.method != http.MethodHead:
		// What is read of a refusal is its code; a refusal whose body
		// cannot be read is judged by its status.
		data, readErr := readBounded(resp.Body, maxErrorBody)
		if readErr == nil {
			out.code = parseErrorCode(data)
		}
	}
	// What is left is read and thrown away, so that the connection is
	// kept for the next request; Discard bounds what it reads.
	_ = egress.Discard(resp)
	return out, nil
}

// readClass returns the class of a failure to read the body of an answer:
// the class it has (a timeout, a connection that failed), or ClassServer
// for a body that is too long or no document.
func (c *Client) readClass(err error) delivery.Class {
	var failure *delivery.Error
	if errors.As(err, &failure) {
		return failure.Class
	}
	return delivery.ClassServer
}

// request returns the signed request of rq, and the body of signed chunks
// when it has one. The secret is opened for as long as it takes to sign and
// cleared after; what the body holds is the signing key of the day.
func (c *Client) request(ctx context.Context, rq call) (*http.Request, *chunkedBody, error) {
	fail := func(class delivery.Class, err error) (*http.Request, *chunkedBody, error) {
		return nil, nil, delivery.Failed(class, fmt.Errorf("s3client.%s: %w", rq.op, err))
	}
	path := "/" + rq.bucket
	if rq.key != "" {
		path += "/" + rq.key
	}
	target := &url.URL{
		Scheme: "https", Host: c.host, Path: path, RawPath: sigv4.URIEncode(path, false),
		RawQuery: encodeQuery(rq.query),
	}
	req, err := http.NewRequestWithContext(ctx, rq.method, c.target.Endpoint, http.NoBody)
	if err != nil {
		// The origin was validated by New; the cause, which repeats it,
		// is left out all the same.
		return fail(delivery.ClassInternal, errors.New("s3client: the request could not be made"))
	}
	req.URL, req.Host = target, target.Host
	req.Header = http.Header{}
	for name, values := range rq.header {
		req.Header[name] = slices.Clone(values)
	}

	payloadHash := sigv4.EmptyPayloadSHA256
	switch {
	case rq.stream != nil:
		payloadHash = payloadStreamingSigned
		encoding := contentEncodingChunked
		if stored := req.Header.Get("Content-Encoding"); stored != "" {
			encoding += "," + stored
		}
		req.Header.Set("Content-Encoding", encoding)
		req.Header.Set(headerDecodedLength, strconv.FormatInt(rq.size, 10))
		req.ContentLength = chunkedLength(rq.size, c.chunkSize)
	case len(rq.body) > 0:
		sum := sha256.Sum256(rq.body)
		payloadHash = hex.EncodeToString(sum[:])
		req.Body = io.NopCloser(bytes.NewReader(rq.body))
		req.ContentLength = int64(len(rq.body))
		body := rq.body
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}
	for name, values := range req.Header {
		for _, value := range values {
			if !headerValue(value) || !headerName(name) {
				return fail(delivery.ClassRejected, fmt.Errorf("%w: a header that cannot be sent", ErrUnsendable))
			}
		}
	}

	now := c.clock.Now().UTC()
	timestamp, date := now.Format(sigv4.AmzDateFormat), now.Format(sigv4.ScopeDateFormat)
	req.Header.Set(sigv4.HeaderAmzDate, timestamp)
	req.Header.Set(sigv4.HeaderContentSHA256, payloadHash)
	signed := make([]string, 0, len(req.Header)+1)
	signed = append(signed, "host")
	for name := range req.Header {
		signed = append(signed, strings.ToLower(name))
	}
	slices.Sort(signed)
	signed = slices.Compact(signed)
	if len(signed) > maxSignedHeaders {
		return fail(delivery.ClassRejected, fmt.Errorf("%w: more than %d signed headers", ErrUnsendable, maxSignedHeaders))
	}
	signedRaw := strings.Join(signed, ";")
	canonical := sigv4.CanonicalRequest(req, signed, signedRaw, payloadHash)

	secret, err := c.secrets.Open(ctx, c.target.Secret, delivery.SecretAAD(c.target.Name))
	if err != nil {
		return fail(localClass(err), fmt.Errorf("%w: %w", ErrCredential, err))
	}
	// The key of the day is derived once and the secret cleared at once; the
	// key is cleared as soon as nothing signs with it: here, or by the body
	// of chunks that signs on with it.
	key := sigv4.SigningKey(secret, date, c.target.Region)
	clear(secret)
	scope := sigv4.Scope(date, c.target.Region)
	signature := sigv4.Signature(key, sigv4.StringToSign(timestamp, scope, canonical))
	if rq.stream == nil {
		clear(key)
		key = nil
	}

	authorization := sigv4.Algorithm + " Credential=" + c.target.Auth.ID + "/" + scope +
		", SignedHeaders=" + signedRaw + ", Signature=" + signature
	if len(authorization) > maxAuthorizationHeader {
		clear(key)
		return fail(delivery.ClassRejected, fmt.Errorf("%w: an Authorization header of more than %d bytes", ErrUnsendable, maxAuthorizationHeader))
	}
	req.Header.Set("Authorization", authorization)
	// Not signed: nothing at the other deployment depends on it.
	req.Header.Set("User-Agent", c.userAgent)

	var body *chunkedBody
	if rq.stream != nil {
		chain := sigv4.Chain{Key: key, Timestamp: timestamp, Scope: scope, Previous: signature}
		body = newChunkedBody(rq.stream, rq.size, chain, c.chunkSize)
		req.Body = body
		// A body of chunks cannot be sent again: nothing may resend it.
		req.GetBody = nil
	}
	return req, body, nil
}

// encodeQuery returns the query string of pairs, each name and value
// encoded as SigV4's canonical query encodes them, so that what is sent is
// what is signed. A pair without a value is sent as its name alone.
func encodeQuery(pairs [][2]string) string {
	var b strings.Builder
	for i, pair := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(sigv4.URIEncode(pair[0], true))
		if pair[1] != "" {
			b.WriteByte('=')
			b.WriteString(sigv4.URIEncode(pair[1], true))
		}
	}
	return b.String()
}

// headerValue reports whether s can be sent as the value of a header as it
// is: no control character but the tab.
func headerValue(s string) bool {
	for i := range len(s) {
		if c := s[i]; (c < ' ' && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}

// headerName reports whether s is a token of HTTP: what a header is named
// by.
func headerName(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		if !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			return false
		}
	}
	return true
}
