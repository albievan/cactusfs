package s3api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/albievan/cactusfs/internal/domain/iam"
)

// AWS Signature Version 4 with the Authorization header, implemented from the
// public AWS documentation ("Authenticating Requests: Using the Authorization
// Header (AWS Signature Version 4)").
const (
	sigV4Algorithm   = "AWS4-HMAC-SHA256"
	sigV4Terminator  = "aws4_request"
	sigV4Service     = "s3"
	amzDateFormat    = "20060102T150405Z"
	scopeDateFormat  = "20060102"
	maxClockSkew     = 15 * time.Minute
	headerContentSHA = "X-Amz-Content-Sha256"
	headerAmzDate    = "X-Amz-Date"

	payloadUnsigned         = "UNSIGNED-PAYLOAD"
	payloadStreamingTrailer = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
	payloadStreamingSigned  = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	payloadSignedTrailer    = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	chunkAlgorithm          = "AWS4-HMAC-SHA256-PAYLOAD"
	trailerAlgorithm        = "AWS4-HMAC-SHA256-TRAILER"
	// maxPresignedExpiry is the longest a presigned URL may be valid:
	// seven days, as SigV4 defines.
	maxPresignedExpiry       = 7 * 24 * time.Hour
	payloadStreamingPrefix   = "STREAMING-"
	emptyPayloadSHA256       = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	maxAuthorizationHeader   = 4 << 10
	maxSignedHeaders         = 64
	signatureHexLength       = 2 * sha256.Size
	credentialScopeFieldsLen = 5
)

// Authentication failures. Each maps to one S3 error code.
var (
	errAuthMissing        = errors.New("request is not signed")
	errAuthMalformed      = errors.New("authorization header is malformed")
	errAuthUnsupported    = errors.New("signature method is not supported")
	errAuthSkew           = errors.New("request time is too far from the server time")
	errAuthSignature      = errors.New("signature does not match")
	errAuthMissingContent = errors.New("x-amz-content-sha256 header is required")
	errAuthExpired        = errors.New("presigned request has expired")
	errAuthQuery          = errors.New("presigned query parameters are malformed")
)

// Query parameters of a presigned URL.
const (
	queryAlgorithm     = "X-Amz-Algorithm"
	querySigner        = "X-Amz-Credential"
	queryDate          = "X-Amz-Date"
	queryExpires       = "X-Amz-Expires"
	querySignedHeaders = "X-Amz-SignedHeaders"
	querySignature     = "X-Amz-Signature"
)

func presignedParameters() []string {
	return []string{queryAlgorithm, querySigner, queryDate, queryExpires, querySignedHeaders, querySignature}
}

// verified is what a request that passed signature verification carries on:
// how its payload is declared, and what signs the chunks of a streamed one.
type verified struct {
	// payloadHash is the payload declaration the signature covers.
	payloadHash string
	// chunks verifies the chunk signatures of a signed streaming payload,
	// which continue from the signature of the request.
	chunks *chunkSigner
}

// chunkSigner verifies the chain of signatures of a signed aws-chunked body.
// It holds the signing key of one day, region and service - never the secret.
type chunkSigner struct {
	key       []byte
	timestamp string
	scope     string
	previous  string
}

// check verifies the declared signature of the next link of the chain:
// a chunk whose data hashes to digestHex, or the trailers.
func (c *chunkSigner) check(algorithm, declared, digestHex string) error {
	stringToSign := algorithm + "\n" + c.timestamp + "\n" + c.scope + "\n" + c.previous + "\n"
	if algorithm == chunkAlgorithm {
		stringToSign += emptyPayloadSHA256 + "\n"
	}
	want := hex.EncodeToString(hmacSHA256(c.key, stringToSign+digestHex))
	if !iam.SecretsEqual([]byte(want), []byte(declared)) {
		return fmt.Errorf("%w: chunk signature", errAuthSignature)
	}
	c.previous = want
	return nil
}

// signature is a parsed SigV4 Authorization header.
type signature struct {
	accessKeyID   string
	scopeDate     string
	region        string
	signedHeaders []string
	// signedHeadersRaw is the SignedHeaders value exactly as sent; it is
	// part of the canonical request.
	signedHeadersRaw string
	signatureHex     string
}

// parseAuthorization parses "AWS4-HMAC-SHA256 Credential=…, SignedHeaders=…,
// Signature=…". Every error wraps errAuthMalformed or errAuthUnsupported.
func parseAuthorization(header string) (signature, error) {
	if len(header) > maxAuthorizationHeader {
		return signature{}, fmt.Errorf("%w: header too long", errAuthMalformed)
	}
	algorithm, rest, found := strings.Cut(strings.TrimSpace(header), " ")
	if algorithm != sigV4Algorithm {
		return signature{}, fmt.Errorf("%w: %q", errAuthUnsupported, algorithm)
	}
	if !found {
		return signature{}, fmt.Errorf("%w: no parameters", errAuthMalformed)
	}

	var sig signature
	var credential string
	seen := map[string]bool{}
	for part := range strings.SplitSeq(rest, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || value == "" || seen[name] {
			return signature{}, fmt.Errorf("%w: bad parameter", errAuthMalformed)
		}
		seen[name] = true
		switch name {
		case "Credential":
			credential = value
		case "SignedHeaders":
			sig.signedHeadersRaw = value
		case "Signature":
			sig.signatureHex = value
		default:
			return signature{}, fmt.Errorf("%w: unknown parameter", errAuthMalformed)
		}
	}
	if credential == "" || sig.signedHeadersRaw == "" || sig.signatureHex == "" {
		return signature{}, fmt.Errorf("%w: missing parameter", errAuthMalformed)
	}

	// Credential = <access key ID>/<date>/<region>/s3/aws4_request
	scope := strings.Split(credential, "/")
	if len(scope) != credentialScopeFieldsLen || scope[3] != sigV4Service || scope[4] != sigV4Terminator {
		return signature{}, fmt.Errorf("%w: bad credential scope", errAuthMalformed)
	}
	sig.accessKeyID, sig.scopeDate, sig.region = scope[0], scope[1], scope[2]
	if iam.ValidateAccessKeyID(sig.accessKeyID) != nil || sig.region == "" {
		return signature{}, fmt.Errorf("%w: bad credential", errAuthMalformed)
	}
	if _, err := time.Parse(scopeDateFormat, sig.scopeDate); err != nil {
		return signature{}, fmt.Errorf("%w: bad credential date", errAuthMalformed)
	}

	sig.signedHeaders = strings.Split(sig.signedHeadersRaw, ";")
	if len(sig.signedHeaders) > maxSignedHeaders || !slices.IsSorted(sig.signedHeaders) {
		return signature{}, fmt.Errorf("%w: signed headers must be sorted", errAuthMalformed)
	}
	for _, name := range sig.signedHeaders {
		if name == "" || name != strings.ToLower(name) {
			return signature{}, fmt.Errorf("%w: signed headers must be lowercase", errAuthMalformed)
		}
	}
	if len(sig.signatureHex) != signatureHexLength {
		return signature{}, fmt.Errorf("%w: bad signature length", errAuthMalformed)
	}
	if _, err := hex.DecodeString(sig.signatureHex); err != nil {
		return signature{}, fmt.Errorf("%w: signature is not hexadecimal", errAuthMalformed)
	}
	return sig, nil
}

// requestTime returns the signing time of a request: X-Amz-Date, or Date when
// that is absent.
func requestTime(r *http.Request) (time.Time, string, error) {
	if value := r.Header.Get(headerAmzDate); value != "" {
		t, err := time.Parse(amzDateFormat, value)
		if err != nil {
			return time.Time{}, "", fmt.Errorf("%w: bad X-Amz-Date", errAuthMalformed)
		}
		return t, value, nil
	}
	value := r.Header.Get("Date")
	if value == "" {
		return time.Time{}, "", fmt.Errorf("%w: no X-Amz-Date or Date header", errAuthMalformed)
	}
	t, err := http.ParseTime(value)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: bad Date", errAuthMalformed)
	}
	return t, t.UTC().Format(amzDateFormat), nil
}

// checkSignedHeaders enforces what must be covered by the signature: the
// host, the signing time when it is carried in X-Amz-Date, every x-amz-*
// header of the request and Content-MD5. An unsigned x-amz-* header could otherwise be added
// or changed in transit.
func checkSignedHeaders(r *http.Request, sig signature) error {
	if !slices.Contains(sig.signedHeaders, "host") {
		return fmt.Errorf("%w: host must be signed", errAuthMalformed)
	}
	for name := range r.Header {
		lower := strings.ToLower(name)
		mustSign := strings.HasPrefix(lower, "x-amz-") || lower == "content-md5"
		if mustSign && !slices.Contains(sig.signedHeaders, lower) {
			return fmt.Errorf("%w: header %s must be signed", errAuthMalformed, lower)
		}
	}
	for _, name := range sig.signedHeaders {
		// net/http keeps the host and the content length out of the
		// header map.
		if name == "content-length" && r.ContentLength >= 0 {
			continue
		}
		if name != "host" && len(r.Header.Values(name)) == 0 {
			return fmt.Errorf("%w: signed header %s is absent", errAuthMalformed, name)
		}
	}
	return nil
}

// canonicalRequest builds the canonical request of the SigV4 specification.
func canonicalRequest(r *http.Request, sig signature, payloadHash string) string {
	var b strings.Builder
	b.WriteString(r.Method)
	b.WriteByte('\n')
	b.WriteString(uriEncode(r.URL.Path, false))
	b.WriteByte('\n')
	b.WriteString(canonicalQuery(r.URL.RawQuery))
	b.WriteByte('\n')
	for _, name := range sig.signedHeaders {
		b.WriteString(name)
		b.WriteByte(':')
		switch {
		case name == "host":
			b.WriteString(r.Host)
		case name == "content-length" && len(r.Header.Values(name)) == 0:
			b.WriteString(strconv.FormatInt(r.ContentLength, 10))
		default:
			for i, value := range r.Header.Values(name) {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(strings.Join(strings.Fields(value), " "))
			}
		}
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(sig.signedHeadersRaw)
	b.WriteByte('\n')
	b.WriteString(payloadHash)
	return b.String()
}

// canonicalQuery re-encodes and sorts the query string. A component that is
// not valid percent-encoding is kept as it is; the signature will then simply
// not match what the client computed.
func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	pairs := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		name, value, _ := strings.Cut(part, "=")
		pairs = append(pairs, uriEncode(unescape(name), true)+"="+uriEncode(unescape(value), true))
	}
	slices.Sort(pairs)
	return strings.Join(pairs, "&")
}

func unescape(s string) string {
	decoded, err := url.PathUnescape(s)
	if err != nil {
		return s
	}
	return decoded
}

// uriEncode percent-encodes every byte except the unreserved characters
// A-Z a-z 0-9 - _ . ~, and '/' unless encodeSlash is set, with uppercase
// hexadecimal digits, as the SigV4 specification defines UriEncode.
func uriEncode(s string, encodeSlash bool) string {
	const upperHex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&0x0F])
		}
	}
	return b.String()
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

func scopeOf(sig signature) string {
	return sig.scopeDate + "/" + sig.region + "/" + sigV4Service + "/" + sigV4Terminator
}

// signingKey derives the key that signs the requests of one day, region and
// service from the secret.
func signingKey(secret []byte, sig signature) []byte {
	seed := make([]byte, 0, len("AWS4")+len(secret))
	seed = append(append(seed, "AWS4"...), secret...)
	key := hmacSHA256(seed, sig.scopeDate)
	clear(seed)
	key = hmacSHA256(key, sig.region)
	key = hmacSHA256(key, sigV4Service)
	return hmacSHA256(key, sigV4Terminator)
}

// expectedSignature computes the signature a holder of secret would have
// produced for the request.
func expectedSignature(secret []byte, sig signature, timestamp, canonical string) string {
	digest := sha256.Sum256([]byte(canonical))
	stringToSign := sigV4Algorithm + "\n" + timestamp + "\n" + scopeOf(sig) + "\n" + hex.EncodeToString(digest[:])
	return hex.EncodeToString(hmacSHA256(signingKey(secret, sig), stringToSign))
}

// verifySignature checks a parsed signature against the request. It returns
// the payload hash declaration the signature covers.
func verifySignature(r *http.Request, sig signature, secret []byte, now time.Time) (verified, error) {
	signedAt, timestamp, err := requestTime(r)
	if err != nil {
		return verified{}, err
	}
	if signedAt.UTC().Format(scopeDateFormat) != sig.scopeDate {
		return verified{}, fmt.Errorf("%w: credential date does not match the request date", errAuthMalformed)
	}
	if skew := now.Sub(signedAt); skew > maxClockSkew || skew < -maxClockSkew {
		return verified{}, errAuthSkew
	}
	if err := checkSignedHeaders(r, sig); err != nil {
		return verified{}, err
	}
	payloadHash := r.Header.Get(headerContentSHA)
	if payloadHash == "" {
		return verified{}, errAuthMissingContent
	}
	want := expectedSignature(secret, sig, timestamp, canonicalRequest(r, sig, payloadHash))
	if !iam.SecretsEqual([]byte(want), []byte(sig.signatureHex)) {
		return verified{}, errAuthSignature
	}
	out := verified{payloadHash: payloadHash}
	if payloadHash == payloadStreamingSigned || payloadHash == payloadSignedTrailer {
		// The chunks of the body are signed in a chain that begins with
		// the signature just verified.
		out.chunks = &chunkSigner{key: signingKey(secret, sig), timestamp: timestamp, scope: scopeOf(sig), previous: want}
	}
	return out, nil
}

// presigned is the signature of a presigned URL: SigV4 carried in the query
// string, implemented from the public AWS documentation ("Authenticating
// Requests: Using Query Parameters (AWS Signature Version 4)").
type presigned struct {
	signature
	timestamp string
	signedAt  time.Time
	expires   time.Duration
}

// isPresigned reports whether a request carries any part of a signature in
// its query string.
func isPresigned(query url.Values) bool {
	for _, name := range presignedParameters() {
		if query.Has(name) {
			return true
		}
	}
	return false
}

// parsePresigned reads the signature out of the query string. Every error
// wraps errAuthQuery or errAuthUnsupported.
func parsePresigned(query url.Values) (presigned, error) {
	one := func(name string) (string, error) {
		values := query[name]
		if len(values) != 1 || values[0] == "" {
			return "", fmt.Errorf("%w: %s must be given once", errAuthQuery, name)
		}
		return values[0], nil
	}
	algorithm, err := one(queryAlgorithm)
	if err != nil {
		return presigned{}, err
	}
	if algorithm != sigV4Algorithm {
		return presigned{}, fmt.Errorf("%w: %q", errAuthUnsupported, algorithm)
	}
	var fields [5]string
	for i, name := range presignedParameters()[1:] {
		if fields[i], err = one(name); err != nil {
			return presigned{}, err
		}
	}
	credential, date, expiry, signedHeaders, signatureHex := fields[0], fields[1], fields[2], fields[3], fields[4]
	// The same checks as for the header form, on the same grammar.
	sig, err := parseAuthorization(sigV4Algorithm + " Credential=" + credential + ",SignedHeaders=" + signedHeaders + ",Signature=" + signatureHex)
	if err != nil {
		if strings.ContainsAny(credential+signedHeaders+signatureHex, ", ") || !errors.Is(err, errAuthMalformed) {
			return presigned{}, fmt.Errorf("%w: bad credential, signed headers or signature", errAuthQuery)
		}
		return presigned{}, fmt.Errorf("%w: %w", errAuthQuery, err)
	}
	signedAt, err := time.Parse(amzDateFormat, date)
	if err != nil {
		return presigned{}, fmt.Errorf("%w: bad %s", errAuthQuery, queryDate)
	}
	seconds, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil || seconds < 1 || seconds > int64(maxPresignedExpiry/time.Second) {
		return presigned{}, fmt.Errorf("%w: %s must be 1 to %d seconds", errAuthQuery, queryExpires, int64(maxPresignedExpiry/time.Second))
	}
	return presigned{signature: sig, timestamp: date, signedAt: signedAt, expires: time.Duration(seconds) * time.Second}, nil
}

// verifyPresigned checks the signature of a presigned URL against the
// request. The URL is good from its signing time until it expires; a request
// with a signing time in the future is refused like any other skewed one.
func verifyPresigned(r *http.Request, p presigned, secret []byte, now time.Time) (verified, error) {
	if p.signedAt.UTC().Format(scopeDateFormat) != p.scopeDate {
		return verified{}, fmt.Errorf("%w: credential date does not match %s", errAuthQuery, queryDate)
	}
	if p.signedAt.Sub(now) > maxClockSkew {
		return verified{}, errAuthSkew
	}
	if now.After(p.signedAt.Add(p.expires)) {
		return verified{}, errAuthExpired
	}
	if err := checkSignedHeaders(r, p.signature); err != nil {
		return verified{}, err
	}
	// The payload of a presigned request is not signed, unless the signer
	// chose to sign a declaration of it as a header.
	payloadHash := payloadUnsigned
	if declared := r.Header.Get(headerContentSHA); declared != "" {
		payloadHash = declared
	}
	if strings.HasPrefix(payloadHash, payloadStreamingPrefix) {
		return verified{}, fmt.Errorf("%w: a presigned request cannot stream signed chunks", errAuthQuery)
	}
	// Everything in the query string is signed, except the signature.
	unsigned := *r
	target := *r.URL
	target.RawQuery = withoutParameter(r.URL.RawQuery, querySignature)
	unsigned.URL = &target
	want := expectedSignature(secret, p.signature, p.timestamp, canonicalRequest(&unsigned, p.signature, payloadHash))
	if !iam.SecretsEqual([]byte(want), []byte(p.signatureHex)) {
		return verified{}, errAuthSignature
	}
	return verified{payloadHash: payloadHash}, nil
}

// withoutParameter removes every occurrence of a parameter from a raw query
// string and leaves the rest byte for byte as it was.
func withoutParameter(raw string, names ...string) string {
	parts := strings.Split(raw, "&")
	kept := parts[:0]
	for _, part := range parts {
		name, _, _ := strings.Cut(part, "=")
		if !slices.Contains(names, unescape(name)) {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "&")
}
